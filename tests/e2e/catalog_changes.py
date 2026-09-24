"""Incremental catalogs, concurrent capture, authorization and durable CLI cache."""
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request
import uuid

from api_access import servers
from capture_batches import packed
import run


def relay(method, path, body=None):
    request = urllib.request.Request("http://catalog-relay:8080" + path, method=method,
        data=None if body is None else json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "X-E2E-Control": "e2e-catalog-fault-only"})
    with urllib.request.urlopen(request, timeout=15) as response:
        return json.load(response)


def changes(state, root, cursor="", peer=None, limit=17, statuses=(200,)):
    return run.request("GET", f"/roots/{root}/catalog-changes?" + urllib.parse.urlencode({"cursor": cursor, "limit": limit}),
        key=state["key"], server=peer, statuses=statuses)


def drain(state, root, cursor="", peer=None):
    files = {}
    for _ in range(1000):
        page = changes(state, root, cursor, peer)
        files.update((f["path"], f) for f in page["files"])
        if not page["more"]:
            return files, page["cursor"]
        assert cursor != page["cursor"]
        cursor = page["cursor"]
    raise AssertionError("catalog cursor did not converge")


def prepare():
    state = run.provision()
    directory = Path("/state/incremental-catalog")
    directory.mkdir()
    for i in range(521):
        (directory / f"record-{i:04}.txt").write_text(f"Orchid calibration record {i}.\n")
    root = run.new_root(state, "Incremental catalog", directory, True)
    state["catalog_bulk_root"] = root
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    peers = servers()
    files, cursor = drain(state, root, peer=peers[0])
    assert len(files) == 521
    assert not changes(state, root, cursor, peers[1])["files"]
    assert changes(state, root, cursor + "x", peers[0], statuses=(409,))["code"] == "catalog_cursor_reset"

    # Two durable cache pages, with SIGKILL before the second response arrives.
    # The first page's transaction must survive and its cursor must be replayed.
    relay("POST", "/hold", {"root": root, "skip": 1})
    env = dict(os.environ, PUFFERFS_API_KEY=state["key"], PUFFERFS_SERVER_URL="http://catalog-relay:8080")
    command = ["pufferfs", "sync", str(directory), "--id", root, "--no-vector", "--json"]
    with tempfile.TemporaryFile(mode="w+") as log:
        process = subprocess.Popen(command, env=env, stdout=log, stderr=log)
        try:
            held = run.eventually("second catalog response held before cache commit", lambda: next((e for e in relay("GET", "/status")["events"] if e["state"] == "held"), None), 60)
            assert held["files"] == 21
            process.kill()
            process.wait(timeout=10)
        finally:
            relay("POST", "/release")
            if process.poll() is None:
                process.kill()
            process.wait(timeout=10)
    previous = len(relay("GET", "/status")["events"])
    result = subprocess.run(command, env=env, text=True, capture_output=True, timeout=120)
    assert result.returncode == 0, result.stderr[-2000:]
    assert json.loads(result.stdout)["changes"] == 0
    events = relay("GET", "/status")["events"][previous:]
    assert events and events[0]["cursor_sha256"] == held["cursor_sha256"] and events[0]["files"] == 21
    previous = len(relay("GET", "/status")["events"])
    result = subprocess.run(command, env=env, text=True, capture_output=True, timeout=120)
    assert result.returncode == 0 and json.loads(result.stdout)["changes"] == 0
    events = relay("GET", "/status")["events"][previous:]
    assert len(events) == 1 and events[0]["files"] == 0
    print("CLI SIGKILL preserved its first cache page; restart replayed only the uncommitted page and the next sync downloaded zero files.", flush=True)

    # Concurrent writers and readers on different API processes. All writes use
    # public source/capture endpoints; inspection never creates database state.
    # Each independent capture needs its own fresh upload. A pack is bound
    # atomically to the paths in its first accepted capture.
    sources = [packed(state, root, [f"Violet concurrent metadata {i}.\n".encode()], parts=1)[0] for i in range(20)]
    def register(index):
        return run.request("POST", f"/roots/{root}/versions", {"capture_id": str(uuid.uuid4()),
            "files": [{"path": f"concurrent-{index:02}/record.txt", "source": sources[index]}]},
            key=state["key"], server=peers[index % 2], statuses=(202,))
    seen = {}
    with ThreadPoolExecutor(max_workers=4) as pool:
        futures = [pool.submit(register, i) for i in range(20)]
        while not all(f.done() for f in futures):
            page = changes(state, root, cursor, peers[1], limit=3)
            seen.update((f["path"], f) for f in page["files"])
            cursor = page["cursor"]
        for f in futures:
            f.result()
    remaining, cursor = drain(state, root, cursor, peers[0])
    seen.update(remaining)
    assert set(seen) == {f"concurrent-{i:02}/record.txt" for i in range(20)}

    # Fresh ACLs invalidate prior metadata caches before they can be reused.
    acl_key = run.request("POST", f"/admin/orgs/{state['org']}/users/{state['users'][0]}/api-keys",
        {"name": "catalog-acl", "scopes": ["acl:write"]}, statuses=(201,))["key"]
    acl = run.request("POST", f"/roots/{root}/acls", {"path_prefix": "/concurrent-00/", "grant_to": "*", "permission": "none"}, key=acl_key, statuses=(201,))
    try:
        assert changes(state, root, cursor, peers[1], statuses=(409,))["code"] == "catalog_cursor_reset"
        visible, restricted_cursor = drain(state, root, peer=peers[1])
        assert len(visible) == 540 and "concurrent-00/record.txt" not in visible, len(visible)
    finally:
        run.request("DELETE", f"/roots/{root}/acls/{acl['id']}", key=acl_key)
    assert changes(state, root, restricted_cursor, peers[0], statuses=(409,))["code"] == "catalog_cursor_reset"
    print("Concurrent captures were never skipped; both APIs invalidated cursors when path permissions changed.", flush=True)

    # Remove the metadata-scale fixture before starting workers; this test does
    # not buy hundreds of unnecessary provider writes. A small separate root
    # exercises publication/deletion cursor updates through the complete flow.
    run.request("DELETE", f"/roots/{root}", key=state["key"])
    state["roots"].remove(root)
    small = Path("/state/catalog-publication")
    small.mkdir()
    (small / "record.txt").write_text("Orchid durable catalog publication.\n")
    root = run.new_root(state, "Catalog publication", small, True)
    state["root"] = root
    run.cli(state, "sync", str(small), "--id", root, "--no-vector")
    current, state["catalog_cursor"] = drain(state, root)
    assert not current["record.txt"]["indexed_version_id"]
    run.save(state)


def verify():
    state = json.loads(run.STATE.read_text())
    root = state["root"]
    file = run.wait_indexed(state)["record.txt"]
    changed, cursor = drain(state, root, state["catalog_cursor"], servers()[1])
    assert changed["record.txt"]["indexed_version_id"] == file["version_id"]
    state["catalog_cursor"] = cursor
    run.save(state)
    run.assert_source_retained(file)
    text = run.request("POST", f"/roots/{root}/read", {"path": "record.txt", "lines": {"start": 1, "end": 1}}, key=state["key"])
    assert text["lines"][0]["content"] == "Orchid durable catalog publication."
    directory = Path("/state/catalog-publication")
    (directory / "record.txt").unlink()
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    run.wait_indexed(state)
    changed, state["catalog_cursor"] = drain(state, root, cursor, servers()[0])
    assert changed["record.txt"]["deleted"] and changed["record.txt"]["indexed_version_id"] == changed["record.txt"]["version_id"]
    run.save(state)
    print("Publication and tombstones arrived through the same incremental cursor; exact source/read verified.", flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    for peer in servers():
        assert not changes(state, state["root"], state["catalog_cursor"], peer)["files"]
    print("Catalog cursor remained valid across both API restarts.", flush=True)


if __name__ == "__main__":
    {"prepare": prepare, "verify": verify, "restarted": restarted}[sys.argv[1]]()
