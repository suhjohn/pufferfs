"""Bounded status responses through public API/CLI, with real processing."""

import hashlib
import json
from pathlib import Path
import sys

from api_access import servers
import run


def calls():
    patterns = ("WITH requested AS (%statuses AS MATERIALIZED%", "SELECT f.id,f.path,v.id,v.sequence,%")
    return [run.sql("SELECT COALESCE(sum(calls),0)::bigint AS n FROM pg_stat_statements WHERE query LIKE %s", (p,))[0]["n"] for p in patterns]


def selection(path, text):
    return {"path": path, "size": len(text.encode()), "content_hash": "sha256:" + hashlib.sha256(text.encode()).hexdigest()}


def prepare():
    state = run.provision()
    directory = Path("/state/status-summary")
    directory.mkdir()
    (directory / "secret").mkdir()
    text = "Orchid observatory records telescope calibration.\n"
    for index in range(1024):
        (directory / f"record-{index:04}.txt").write_text(text)
    (directory / "secret/record.txt").write_text(text)
    root = run.new_root(state, "Bounded status summary", directory, True)
    state.update(root=root, summary_text=text)
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    endpoint = f"/roots/{root}/capture-summary"
    for peer in servers():
        result = run.request("GET", endpoint, key=state["key"], server=peer)
        assert result["total"] == 1025 and result["states"] == {"pending": 1025}
        assert len(result["examples"]) == 20 and len(json.dumps(result)) < 10000
        assert all(e["processing"]["stage"] == "transform" for e in result["examples"])
    before = calls()
    result = json.loads(run.cli(state, "sync", "status", "--root", root, "--json"))
    assert result["total"] == 1025 and result["states"] == {"pending": 1025}
    assert [b-a for a,b in zip(before, calls())] == [1, 0], "CLI enumerated catalog instead of one summary"
    files = [selection("record-0000.txt", text), selection("record-0001.txt", text + "changed"), selection("absent.txt", text)]
    result = run.request("POST", endpoint, {"files": files}, key=state["key"])
    assert result["total"] == 3 and result["states"] == {"pending": 1, "uncaptured": 2}
    assert run.request("POST", endpoint, {"files": []}, key=state["key"])["status"] == "empty"
    run.request("POST", endpoint, {"files": [files[0], files[0]]}, key=state["key"], statuses=(400,))
    for method, body in (("GET", None), ("POST", {"files": files})):
        run.request(method, endpoint, body, key=state["outsider_key"], statuses=(404,))
    acl_key = run.request("POST", f"/admin/orgs/{state['org']}/users/{state['users'][0]}/api-keys",
        {"name": "summary-acl", "scopes": ["acl:write"]}, statuses=(201,))["key"]
    acl = run.request("POST", f"/roots/{root}/acls", {"path_prefix": "/secret/", "grant_to": "*",
        "permission": "none"}, key=acl_key, statuses=(201,))
    try:
        for peer in servers():
            result = run.request("GET", endpoint, key=state["key"], server=peer)
            assert result["total"] == 1024 and result["states"] == {"pending": 1024}
            assert all(not e["path"].startswith("secret/") for e in result["examples"])
            result = run.request("POST", endpoint, {"files": [selection("secret/record.txt", text)]}, key=state["key"], server=peer)
            assert result["states"] == {"uncaptured": 1}
            assert not result["examples"][0].get("version_id") and result["examples"][0].get("processing") is None
    finally:
        run.request("DELETE", f"/roots/{root}/acls/{acl['id']}", key=acl_key)
    print("1025 pending files summarized in one CLI request; selected hashes, missing paths and ACL changes verified on both APIs.", flush=True)


def verify():
    state = json.loads(run.STATE.read_text())
    root = state["root"]
    endpoint = f"/roots/{root}/capture-summary"
    def completed():
        result = run.request("GET", endpoint, key=state["key"])
        assert result["status"] != "failed", result
        return result if result["status"] == "complete" else None
    result = run.eventually("all exact versions published in summary", completed)
    assert result["total"] == 1025 and result["states"] == {"complete": 1025} and not result.get("examples")
    result = json.loads(run.cli(state, "sync", "status", "--root", root, "--json"))
    assert result["status"] == "complete" and result["states"] == {"complete": 1025}
    for peer in servers():
        result = run.request("POST", endpoint, {"files": [selection("record-0000.txt", state["summary_text"])]}, key=state["key"], server=peer)
        assert result["states"] == {"complete": 1}
        result = run.request("POST", f"/roots/{root}/read", {"path": "record-0000.txt", "lines": {"start": 1, "end": 1}}, key=state["key"], server=peer)
        assert result["lines"][0]["content"] == state["summary_text"].rstrip("\n")
        assert run.request("POST", "/query", {"root_id": root, "query": "orchid", "mode": "fts"}, key=state["key"], server=peer)["results"]
    print("Completed summaries matched exact reads/search across both APIs.", flush=True)


if __name__ == "__main__":
    {"prepare": prepare, "verify": verify}[sys.argv[1]]()
