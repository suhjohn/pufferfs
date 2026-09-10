"""Byte-bounded CLI captures through production processes and real search."""

import hashlib
import json
from pathlib import Path
import sys

import run
from retention_security import (capture, check_local_retention,
    check_tenant_uploads, check_unaccepted_retention, local_spools)


def published(state):
    root = state["spool_root"]
    expected = state["spool_expected"]
    files = run.wait_indexed(state, root)
    assert set(files) == set(expected)
    for path, item in expected.items():
        file = files[path]
        assert file["deleted"] == item["deleted"]
        if item["deleted"]:
            run.request("POST", f"/roots/{root}/read", {"path": path, "lines": {"start": 1, "end": 1}},
                key=state["key"], statuses=(404,))
            continue
        assert file["size"] == item["size"] and file["content_hash"] == item["hash"]
        run.assert_source_retained(file)
        read = run.request("POST", f"/roots/{root}/read", {"path": path, "lines": {"start": 1, "end": 1}},
            key=state["key"], statuses=(200,) if item["first_line"] else (400,))
        if item["first_line"]:
            assert [line["content"] for line in read["lines"]] == [item["first_line"]]
    hits = run.request("POST", "/query", {"root_id": root, "query": "orchid", "mode": "fts", "top_k": 20},
        key=state["key"])["results"]
    assert {hit["file_path"] for hit in hits} == {
        path for path, item in expected.items() if not item["deleted"] and item["first_line"]}
    return files


def remember(state, payloads):
    state["spool_expected"] = {path: {"deleted": data is None,
        "size": len(data or b""), "hash": "sha256:" + hashlib.sha256(data or b"").hexdigest(),
        "first_line": data.decode().splitlines()[0] if data else ""}
        for path, data in payloads.items()}
    run.save(state)


def verify():
    state = run.provision()
    directory = Path("/state/byte-batches")
    directory.mkdir()
    # The first file exactly fills the 4 MiB payload allowance. The whole
    # directory exceeds the 8 MiB spool limit, but each file fits by itself.
    payloads = {
        "a.txt": b"Orchid telescope calibration.\n".ljust(4 << 20, b" "),
        "b.txt": b"",
        "c.txt": b"Orchid garden measurements.\n".ljust(3 << 20, b" "),
        "d.txt": b"Orchid weather records.\n",
        "e.txt": b"Orchid soil measurements.\n".ljust(3 << 20, b" "),
        "f.txt": b"Orchid rainfall records.\n",
    }
    for path, data in payloads.items():
        (directory / path).write_bytes(data)
    root = run.new_root(state, "byte-bounded batches", directory, True)
    state["spool_root"] = root
    remember(state, payloads)

    # Disconnect the real upload endpoint after capture creation. Only the
    # completed prefix may be journaled; retry must preserve its identity.
    run.fault("POST", "/proxies/source-upload", {"enabled": False})
    try:
        failed = capture(state, directory, root, success=False)
        assert "upload transport failed" in failed.stderr, failed.stderr[-2000:]
    finally:
        run.fault("POST", "/proxies/source-upload", {"enabled": True})
    cache = local_spools(root)
    pending, = (cache / "pending").iterdir()
    journal = json.loads((pending / "journal.json").read_text())
    assert [file["path"] for file in journal["request"]["files"]] == ["a.txt", "b.txt"]
    assert sum(pack["size"] for pack in journal["packs"]) == 4 << 20
    assert not run.catalog(state, root)

    capture(state, directory, root)
    first = published(state)
    receipts = [json.loads(path.read_text()) for path in (cache / "completed").glob("*/journal.json")]
    assert len(receipts) == 3
    assert sum(len(receipt["request"]["files"]) for receipt in receipts) == len(payloads)
    retried, = [receipt for receipt in receipts if receipt["request"]["capture_id"] == journal["request"]["capture_id"]]
    assert retried["request"] == journal["request"] and retried["accepted"]
    assert not list((cache / "pending").iterdir())
    assert not list((cache / "completed").glob("*/pack-*"))
    original = run.assert_source_retained(first["a.txt"])

    # A verified append fits even though the complete file now exceeds the
    # payload allowance. A rewritten file and a tombstone cross the next batch.
    payloads["a.txt"] += b"\nOrchid appended calibration.\n".ljust(2 << 20, b" ")
    payloads["c.txt"] = b"Orchid replacement measurements.\n".ljust(3 << 20, b" ")
    payloads["e.txt"] = None
    for path, data in payloads.items():
        if data is None:
            (directory / path).unlink()
        else:
            (directory / path).write_bytes(data)
    remember(state, payloads)
    capture(state, directory, root)
    updated = published(state)
    appended = run.assert_source_retained(updated["a.txt"])
    assert appended["extents"][:len(original["extents"])] == original["extents"]
    assert updated["e.txt"]["deleted"]
    assert not list((cache / "completed").glob("*/pack-*"))
    assert not list((cache / "pending").iterdir())
    capture(state, directory, root)
    assert {path: file["version_id"] for path, file in run.catalog(state, root).items()} == {
        path: file["version_id"] for path, file in updated.items()}
    run.request("GET", f"/roots/{root}/captured-files", key=state["outsider_key"], statuses=(404,))
    print("Byte-bounded batches, exact capacity, empty files, upload retry, append reuse, replacement, deletion and unchanged sync passed.", flush=True)
    check_local_retention(state)
    check_unaccepted_retention(state)
    check_tenant_uploads(state)


def restarted():
    state = json.loads(run.STATE.read_text())
    published(state)
    print("Captured originals, reads, search and deletion survived process restarts.", flush=True)


if __name__ == "__main__":
    {"verify": verify, "restarted": restarted}[sys.argv[1]]()
