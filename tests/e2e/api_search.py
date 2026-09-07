"""Cross-root searches use one routing read and skip uncaptured roots."""

from concurrent.futures import ThreadPoolExecutor
from collections import Counter
import json
import time
from pathlib import Path

from api_access import servers
from api_reads import relay
import run
from index_recovery import relay as write_relay


def calls():
    patterns = ("SELECT r.id,CASE WHEN EXISTS (%", "SELECT id, org_id, root_id, namespace%",
                "SELECT q.slot,r.id IS NOT NULL,f.path,%", "WITH requested AS (%FROM root_acls%")
    return [run.sql("SELECT COALESCE(sum(calls),0)::bigint AS n FROM pg_stat_statements WHERE query LIKE %s", (p,))[0]["n"] for p in patterns]


def observed_query(peer, key, body, expected_roots, namespaces, hits):
    before = calls()
    previous = {event["id"] for event in relay("GET", "/status")["events"]}
    result = run.request("POST", "/query", body, key=key, server=peer)
    assert [b-a for a,b in zip(before, calls())] == [1, 0, int(bool(hits)), int(bool(hits))], "routing/publication/access reads were not batched"
    events = [event for event in relay("GET", "/status")["events"] if event["id"] not in previous]
    assert len(events) == len(namespaces) and {event["namespace"] for event in events} == set(namespaces), "query called an empty root or missed a populated one"
    assert result["roots_searched"] == expected_roots
    assert {hit["root_id"] for hit in result["results"]} == set(hits)
    return result


def verify():
    state = json.loads(run.STATE.read_text())
    peers = servers()
    directory = Path("/state/api-search")
    directory.mkdir()
    def root(name, path, disabled=True):
        result = run.request("POST", f"/admin/orgs/{state['org']}/roots", {"name": name,
            "scope": "restricted", "source_path": str(path), "vector_disabled": disabled}, statuses=(201,))["id"]
        state["roots"].append(result)
        run.save(state)
        return result
    populated = []
    for name in ("north", "south"):
        path = directory / name
        path.mkdir()
        (path / (name + ".txt")).write_text("Heliotrope instruments measure pressure.\n")
        (path / "records").mkdir()
        for label in ("alpha", "beta"):
            (path / "records" / (label + ".txt")).write_text("Heliotrope calibration record " + label + ".\n")
        created = root(name, path)
        populated.append(created)
        run.cli(state, "sync", str(path), "--id", created, "--no-vector")
        run.wait_indexed(state, created)
    empty_path = directory / "empty"
    empty_path.mkdir()
    empty = [root("Empty " + str(i), empty_path, disabled=i != 0) for i in range(8)]
    namespace_rows = run.sql("SELECT root_id,namespace FROM root_index_namespaces WHERE org_id=%s AND retired_at IS NULL", (state["org"],))
    def namespaces(roots):
        return [row["namespace"] for row in namespace_rows if row["root_id"] in roots]
    for peer in peers:
        for mode in ("fts", "hybrid", "vector"):
            # The empty vector-enabled root needs no embedding provider call.
            # This Compose topology intentionally has no query-embedding process.
            result = observed_query(peer, state["key"], {"root_id": empty[0], "query": "Heliotrope", "mode": mode}, 1, [], [])
            assert result["results"] == [] and result["mode"] == mode
        run.request("POST", "/query", {"root_id": empty[1], "query": "Heliotrope", "mode": "vector"},
            key=state["key"], server=peer, statuses=(400,))
        for selector, roots, routed in (({"root_ids": populated + empty + [populated[0]]}, len(populated)+len(empty), populated),
                                       ({"all_roots": True}, len(state["roots"]), [state["root"]]+populated)):
            for mode in ("fts", "hybrid"):
                observed_query(peer, state["key"], dict(selector, query="Heliotrope", mode=mode), roots, namespaces(routed), populated)
        # A glob changes candidates, while routing remains one read over the selection.
        observed_query(peer, state["key"], {"root_ids": populated + empty, "query": "Heliotrope", "mode": "fts", "glob": "north.txt"},
            len(populated)+len(empty), namespaces(populated), populated[:1])
    # Nothing is cached on either server: a first capture enables the next query.
    (empty_path / "later.txt").write_text("Heliotrope measurements after first capture.\n")
    later = empty[-1]
    run.cli(state, "sync", str(empty_path), "--id", later, "--no-vector")
    run.wait_indexed(state, later)
    for peer in peers:
        observed_query(peer, state["key"], {"root_ids": populated+empty, "query": "Heliotrope", "mode": "fts"},
            len(populated)+len(empty), namespaces(populated+[later]), populated+[later])
    stale_round(state, peers, populated, directory, namespaces(populated))
    revoke_during_search(state, peers, populated, namespaces(populated[:1]))
    viewer = run.request("POST", f"/admin/orgs/{state['org']}/users/{state['users'][1]}/api-keys",
        {"name": "search-access", "scopes": ["query"]}, statuses=(201,))["key"]
    for peer in peers:
        before = calls()
        previous = relay("GET", "/status")["events"]
        run.request("POST", "/query", {"root_ids": populated+empty, "query": "Heliotrope", "mode": "fts"}, key=viewer, server=peer, statuses=(404,))
        assert calls() == before and relay("GET", "/status")["events"] == previous, "unauthorized roots reached routing/provider IO"
        observed_query(peer, viewer, {"all_roots": True, "query": "Heliotrope", "mode": "fts"}, 1, namespaces([state["root"]]), [])
    # A root deleted after provider IO starts must fail the batched publication
    # read even when another selected root has valid results.
    deleted = empty[-2]
    run.cli(state, "sync", str(empty_path), "--id", deleted, "--no-vector")
    run.wait_indexed(state, deleted)
    with ThreadPoolExecutor(max_workers=1) as pool:
        fault = relay("POST", "/fault", {"operation": "query", "mode": "hold_response", "namespaces": namespaces([deleted])})["fault_id"]
        try:
            future = pool.submit(run.request, "POST", "/query", {"root_ids": populated+[deleted], "query": "Heliotrope", "mode": "fts"},
                                 key=state["key"], server=peers[0], statuses=(404,))
            run.eventually("provider response held before root deletion", lambda: any(
                event["fault_id"] == fault and event["state"] == "response_held" for event in relay("GET", "/status")["events"]), 15)
            run.request("DELETE", f"/roots/{deleted}", key=state["key"], server=peers[1])
            relay("POST", "/release")
            assert future.result(timeout=15)["error"] == "root not found"
        finally:
            relay("POST", "/release")
    state["roots"].remove(deleted)
    run.save(state)
    # Hold a real provider response beyond the production 30-second search
    # deadline. The shared routing/provider error path must return HTTP 504.
    with ThreadPoolExecutor(max_workers=1) as pool:
        fault = relay("POST", "/fault", {"operation": "query", "mode": "hold_response", "namespaces": namespaces(populated[:1])})["fault_id"]
        try:
            previous = {event["id"] for event in relay("GET", "/status")["events"]}
            started = time.monotonic()
            future = pool.submit(run.request, "POST", "/query", {"root_ids": populated, "query": "Heliotrope", "mode": "fts"},
                                 key=state["key"], server=peers[0], statuses=(504,))
            run.eventually("real provider response held until search timeout", lambda: any(
                event["fault_id"] == fault and event["state"] == "response_held" and event["upstream_status"] == 200
                for event in relay("GET", "/status")["events"]), 20)
            run.eventually("other roots finish provider IO while one root is held", lambda: set(namespaces(populated[1:])) <= {
                event["namespace"] for event in relay("GET", "/status")["events"]
                if event["id"] not in previous and event["state"] == "response_released"}, 5)
            result = future.result(timeout=45)
            assert result["error"] == "index search timed out" and time.monotonic()-started >= 25
        finally:
            relay("POST", "/release")
    observed_query(peers[1], state["key"], {"root_id": populated[0], "query": "Heliotrope", "mode": "fts"},
                   1, namespaces(populated[:1]), populated[:1])
    state["search_roots"] = populated+[later]
    run.save(state)
    root_scoped_proofs(state, peers, directory)
    print("Two servers passed one routing query across 13 selected roots, exact provider-call counts, uncaptured vector roots, first capture, globs, authorization and provider timeout recovery.", flush=True)


def stale_round(state, peers, roots, directory, namespaces):
    fault = write_relay("POST", "/fault", {"mode": "hold_response", "namespaces": namespaces})["fault_id"]
    try:
        (directory / "north/north.txt").write_text("Heliotrope pending zircon calibration.\n")
        run.cli(state, "sync", str(directory / "north"), "--id", roots[0], "--no-vector")
        held = run.eventually("pending index rows accepted before publication", lambda: next((
            event for event in write_relay("GET", "/status")["events"]
            if event["fault_id"] == fault and event["state"] == "response_held" and event["upstream_status"] == 200), None), 60)
        before = calls()
        previous = {event["id"] for event in relay("GET", "/status")["events"]}
        result = run.request("POST", "/query", {"root_ids": roots, "query": "Heliotrope", "mode": "fts"}, key=state["key"], server=peers[0])
        assert [b-a for a,b in zip(before, calls())] == [1, 0, 1, 1]
        assert len(result["results"]) == 6 and not any("pending zircon" in hit["content"] for hit in result["results"])
        events = [event for event in relay("GET", "/status")["events"] if event["id"] not in previous]
        expected = Counter(namespaces)
        expected[held["namespace"]] += 1
        assert Counter(event["namespace"] for event in events) == expected, "a validated namespace was unnecessarily queried again"
        assert all(event["response_rows"] > 0 for event in events), "fixture did not populate every shard"
    finally:
        write_relay("POST", "/release")
    run.wait_indexed(state, roots[0])
    for peer in peers:
        result = run.request("POST", "/query", {"root_ids": roots, "query": "zircon", "mode": "fts"}, key=state["key"], server=peer)
        assert result["results"] and all("pending zircon" in hit["content"] for hit in result["results"])
    print("One SQL publication read checked four populated shards; only the stale shard retried, then both APIs saw the newly published version.", flush=True)


def revoke_during_search(state, peers, roots, names):
    key = run.request("POST", f"/admin/orgs/{state['org']}/users/{state['users'][0]}/api-keys",
                      {"name": "search-deny", "scopes": ["acl:write"]}, statuses=(201,))["key"]
    with ThreadPoolExecutor(max_workers=1) as pool:
        fault = relay("POST", "/fault", {"operation": "query", "mode": "hold_response", "namespaces": names})["fault_id"]
        acl = None
        try:
            future = pool.submit(run.request, "POST", "/query", {"root_ids": roots, "query": "Heliotrope", "mode": "fts"},
                                 key=state["key"], server=peers[0])
            run.eventually("cross-root search held before access revocation", lambda: any(
                event["fault_id"] == fault and event["state"] == "response_held" for event in relay("GET", "/status")["events"]), 15)
            acl = run.request("POST", f"/roots/{roots[1]}/acls", {"path_prefix": "/records/", "grant_to": state["users"][0], "permission": "none"},
                              key=key, server=peers[1], statuses=(201,))
            relay("POST", "/release")
            hits = future.result(timeout=15)["results"]
            assert len(hits) == 4
            assert {hit["root_id"] for hit in hits if hit["file_path"].startswith("records/")} == {roots[0]}, "deny prefixes leaked across roots or used stale access"
        finally:
            relay("POST", "/release")
            if acl:
                run.request("DELETE", f"/roots/{roots[1]}/acls/{acl['id']}", key=key, server=peers[1])


def root_scoped_proofs(state, peers, directory):
    reader = run.request("POST", f"/admin/orgs/{state['org']}/users/{state['users'][1]}/api-keys",
                         {"name": "cross-root-proofs", "scopes": ["query", "sync"]}, statuses=(201,))["key"]
    roots, grants, files = [], [], []
    try:
        for label in ("amber", "violet"):
            path = directory / label
            path.mkdir()
            (path / "shared.txt").write_text("Heliotrope " + label + " measurements.\n")
            root = run.request("POST", f"/admin/orgs/{state['org']}/roots", {"name": label,
                "scope": "user", "owner_user_id": state["users"][0], "source_path": str(path), "vector_disabled": True}, statuses=(201,))["id"]
            state["roots"].append(root)
            run.save(state)
            roots.append(root)
            endpoint = f"/admin/orgs/{state['org']}/roots/{root}/grants"
            grant = run.request("POST", endpoint, {"principal_type": "user", "principal_id": state["users"][1], "permissions": ["sync"]}, statuses=(201,))
            grants.append(endpoint + "/" + grant["id"])
            run.cli(state, "sync", str(path), "--id", root, "--no-vector")
            files.append(run.wait_indexed(state, root)["shared.txt"])

        def prove(index):
            file = files[index]
            run.request("POST", f"/roots/{roots[index]}/captured-proofs", {"files": [{"path": "shared.txt",
                "version_id": file["version_id"], "content_hash": file["content_hash"]}]}, key=reader, server=peers[1])

        def check(expected):
            for peer in peers:
                for selection in (roots, roots[::-1]):
                    before = calls()
                    hits = run.request("POST", "/query", {"root_ids": selection, "query": "Heliotrope", "mode": "fts"}, key=reader, server=peer)["results"]
                    assert [b-a for a,b in zip(before, calls())] == [1, 0, 1, 1]
                    assert {hit["root_id"] for hit in hits} == set(expected), "proof matched the same path in another root or an obsolete hash"
        check([])
        prove(0)
        check(roots[:1])
        prove(1)
        check(roots)
        (directory / "amber/shared.txt").write_text("Heliotrope revised amber measurements.\n")
        run.cli(state, "sync", str(directory / "amber"), "--id", roots[0], "--no-vector")
        files[0] = run.wait_indexed(state, roots[0])["shared.txt"]
        check(roots[1:])
        prove(0)
        check(roots)
    finally:
        for endpoint in grants:
            run.request("DELETE", endpoint, server=peers[1])
    print("One access read preserved root-scoped proofs for identical paths, missing proofs, changed hashes, both selection orders and both API servers.", flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    roots = state["search_roots"]
    namespaces = [row["namespace"] for row in run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=ANY(%s) AND retired_at IS NULL", (roots,))]
    for peer in servers():
        observed_query(peer, state["key"], {"root_ids": roots, "query": "Heliotrope", "mode": "fts"}, len(roots), namespaces, roots)
    print("Batched routing and published search results survived both API restarts.", flush=True)


if __name__ == "__main__":
    import sys
    {"verify": verify, "restarted": restarted}[sys.argv[1]]()
