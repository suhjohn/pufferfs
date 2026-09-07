"""CLI captures and real provider reads with an external response-holding relay."""

from concurrent.futures import ThreadPoolExecutor
from functools import partial
import json
from pathlib import Path
import urllib.request

from api_access import servers
import run


def relay(method, path, body=None):
    request = urllib.request.Request("http://read-relay:8080" + path, method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"X-E2E-Control": "e2e-index-fault-only", "Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def read_calls():
    patterns = ("SELECT f.indexed_extraction_id, (%", "SELECT id, org_id, root_id, namespace%",
                "%FROM root_acls%UNION ALL SELECT%FROM file_content_proofs%",
                "SELECT id, org_id, root_id, path_prefix, grant_to, permission, created_at%FROM root_acls%")
    return [run.sql("SELECT COALESCE(sum(calls),0)::bigint AS n FROM pg_stat_statements WHERE query LIKE %s",
                    (pattern,))[0]["n"] for pattern in patterns]


def verify():
    state = json.loads(run.STATE.read_text())
    peers = servers()
    admin = partial(run.request, server=peers[1])
    owner, viewer = state["users"][:2]
    reader = admin("POST", f"/admin/orgs/{state['org']}/users/{viewer}/api-keys",
        {"name": "read-proof", "scopes": ["query", "sync"]}, statuses=(201,))["key"]
    acl_key = admin("POST", f"/admin/orgs/{state['org']}/users/{owner}/api-keys",
        {"name": "read-acl", "scopes": ["acl:write"]}, statuses=(201,))["key"]
    directory = Path("/state/api-reads")
    directory.mkdir()
    (directory / "protected").mkdir()
    # One Unicode line exceeds a complete 512-row provider page.
    large = "Orchid pressure αβγδ; " * 160000
    assert len(large.encode()) > 512 * 6000
    (directory / "large.txt").write_text(large + "\n")
    short = "Orchid instruments record atmospheric pressure.\n"
    (directory / "protected/short.txt").write_text(short)
    root = admin("POST", f"/admin/orgs/{state['org']}/roots", {"name": "Read snapshots",
        "scope": "user", "owner_user_id": owner, "source_path": str(directory),
        "vector_disabled": True}, statuses=(201,))["id"]
    state["roots"].append(root)
    run.save(state)
    grants = f"/admin/orgs/{state['org']}/roots/{root}/grants"
    grant = admin("POST", grants, {"principal_type": "user", "principal_id": viewer,
        "permissions": ["sync"]}, statuses=(201,))
    try:
        run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
        files = run.wait_indexed(state, root)
        publications = {row["path"]: row["indexed_extraction_id"] for row in run.sql(
            "SELECT path,indexed_extraction_id FROM file_catalog WHERE root_id=%s", (root,))}
        namespaces = run.sql("SELECT namespace,xmin::text AS revision FROM root_index_namespaces WHERE root_id=%s ORDER BY namespace", (root,))
        names = [row["namespace"] for row in namespaces]
        read_path = f"/roots/{root}/read"
        read = {"path": "protected/short.txt", "lines": {"start": 1, "end": 1}}
        search = {"root_id": root, "query": "Orchid", "glob": "protected/short.txt", "mode": "fts"}
        for peer in peers:
            # Root access alone is insufficient for a personal file without a current proof.
            result = run.request("POST", read_path, read, key=reader, server=peer, statuses=(400,))
            assert "no indexed chunks" in result["error"]
            assert not run.request("POST", "/query", search, key=reader, server=peer)["results"]
        proofs = [{"path": path, "version_id": file["version_id"], "content_hash": file["content_hash"]}
                  for path, file in files.items()]
        run.request("POST", f"/roots/{root}/captured-proofs", {"files": proofs}, key=reader, server=peers[1])
        for peer in peers:
            request = partial(run.request, key=reader, server=peer)
            for path, expected, minimum_queries in (("protected/short.txt", short.rstrip("\n"), 1), ("large.txt", large, 2)):
                before = read_calls()
                events_before = {e["id"] for e in relay("GET", "/status")["events"]}
                result = request("POST", read_path, dict(read, path=path))
                assert result["lines"] and result["lines"][0]["content"] == expected, "read lost or duplicated bytes"
                assert [b-a for a,b in zip(before, read_calls())] == [1, 0, 1, 1], "read routing/access round trips changed"
                events = [e for e in relay("GET", "/status")["events"] if e["id"] not in events_before]
                assert len(events) >= minimum_queries, "fixture did not exercise expected provider queries"
                assert all(e["publication_ids"] == [publications[path]] for e in events)
                before = read_calls()
                events_before = {e["id"] for e in relay("GET", "/status")["events"]}
                error = request("POST", read_path, dict(read, path=path, lines={"start": 9, "end": 9}), statuses=(400,))
                assert "indexed line range is 1:1" in error["error"]
                assert [b-a for a,b in zip(before, read_calls())] == [1, 0, 1, 1], "empty range reloaded routing/publication"
                events = [e for e in relay("GET", "/status")["events"] if e["id"] not in events_before]
                assert len(events) == 2 and events[-1]["response_rows"] > 0
                assert all(e["response_content_bytes"] == 0 for e in events), "metadata fallback downloaded chunk content"
            request("POST", read_path, dict(read, path="missing.txt"), statuses=(404,))
            request("POST", read_path, dict(read, path="missing.txt", lines={"start": 0, "end": 1}), statuses=(400,))

        def hold(pool, path, body, key, statuses):
            fault = relay("POST", "/fault", {"operation": "query", "mode": "hold_response", "namespaces": names})["fault_id"]
            future = pool.submit(run.request, "POST", path, body, key=key, server=peers[0], statuses=statuses)
            run.eventually("real search response held at the network boundary", lambda: any(
                e["fault_id"] == fault and e["state"] == "response_held" and e["upstream_status"] == 200
                for e in relay("GET", "/status")["events"]), 60)
            return future

        with ThreadPoolExecutor(max_workers=1) as pool:
            for path, body, statuses in ((read_path, read, (400,)), ("/query", search, (200,))):
                acl = None
                try:
                    future = hold(pool, path, body, reader, statuses)
                    acl = run.request("POST", f"/roots/{root}/acls", {"path_prefix": "/protected/",
                        "grant_to": "user:" + viewer, "permission": "none"}, key=acl_key, server=peers[1], statuses=(201,))
                    relay("POST", "/release")
                    result = future.result(timeout=90)
                    if path == read_path:
                        assert "no indexed chunks" in result["error"]
                    else:
                        assert not result["results"], "in-flight query leaked newly denied content"
                finally:
                    relay("POST", "/release")
                    if acl:
                        run.request("DELETE", f"/roots/{root}/acls/{acl['id']}", key=acl_key, server=peers[1])
            # Publish a replacement between the empty-range query and metadata fallback.
            # Both queries must retain the original extraction ID, even if old rows expire.
            events_before = {e["id"] for e in relay("GET", "/status")["events"]}
            try:
                future = hold(pool, read_path, dict(read, lines={"start": 9, "end": 9}), state["key"], (400,))
                (directory / "protected/short.txt").write_text(short + "Second measurement.\n")
                run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
                updated = run.wait_indexed(state, root)
                assert run.sql("SELECT indexed_extraction_id FROM file_catalog WHERE root_id=%s AND path=%s",
                    (root, "protected/short.txt"))[0]["indexed_extraction_id"] != publications["protected/short.txt"]
                relay("POST", "/release")
                result = future.result(timeout=90)
                assert "indexed line range is 1:2" not in result["error"]
                events = [e for e in relay("GET", "/status")["events"] if e["id"] not in events_before]
                assert len(events) == 2 and all(e["publication_ids"] == [publications["protected/short.txt"]] for e in events)
            finally:
                relay("POST", "/release")
        for peer in peers:
            result = run.request("POST", read_path, read, key=reader, server=peer, statuses=(400,))
            assert "no indexed chunks" in result["error"], "stale content proof authorized replacement bytes"
        file = updated["protected/short.txt"]
        run.request("POST", f"/roots/{root}/captured-proofs", {"files": [{"path": "protected/short.txt",
            "version_id": file["version_id"], "content_hash": file["content_hash"]}]}, key=reader, server=peers[0])
        result = run.request("POST", read_path, dict(read, lines={"start": 2, "end": 2}), key=reader, server=peers[1])
        assert result["lines"][0]["content"] == "Second measurement."
        assert run.sql("SELECT namespace,xmin::text AS revision FROM root_index_namespaces WHERE root_id=%s ORDER BY namespace", (root,)) == namespaces
    finally:
        relay("POST", "/release")
        admin("DELETE", grants + "/" + grant["id"])
    print("Two servers passed exact multi-page Unicode reads, fixed query counts, current/stale proofs, in-flight ACL revocation and pinned metadata fallback.", flush=True)


if __name__ == "__main__":
    verify()
