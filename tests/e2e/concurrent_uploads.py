"""Production CLI transfers through real S3, with bounded and interrupted IO."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import urllib.request

import run


LINE = b"Orchid observatory calibration log.".ljust(63, b" ") + b"\n"


def relay(method, path, value=None):
    request = urllib.request.Request("http://upload-relay:8080" + path,
        data=None if value is None else json.dumps(value).encode(), method=method,
        headers={"Content-Type": "application/json", "X-E2E-Control": "e2e-upload-fault-only"})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def fixture(state, name, size):
    directory = Path("/state") / name
    directory.mkdir()
    with (directory / "record.txt").open("wb") as output:
        for _ in range(size // (len(LINE) * 1024)):
            output.write(LINE * 1024)
    root = run.new_root(state, name, directory, True)
    return directory, root


def verify_capture(state, directory, root, expected_size):
    file = run.catalog(state, root)["record.txt"]
    assert file["size"] == expected_size
    manifest = run.assert_source_retained(file)
    assert manifest["size"] == expected_size
    # Preserve the large accepted version; the replacement is indexed after
    # workers start so this upload-specific test does not buy bulk indexing.
    content = "Violet observatory replacement after upload recovery.\n"
    (directory / "record.txt").write_text(content)
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    state.setdefault("upload_cases", []).append({"root": root, "content": content})
    run.save(state)


def capture():
    state = run.provision()
    directory, root = fixture(state, "bounded-uploads", 64 << 20)
    prefix = f"sources/{state['org']}/{root}/"
    relay("POST", "/hold", {"prefix": prefix, "parts": []})
    with tempfile.TemporaryFile(mode="w+") as log:
        process = subprocess.Popen(["pufferfs", "sync", str(directory), "--id", root, "--no-vector"],
            env=dict(os.environ, PUFFERFS_API_KEY=state["key"], PUFFERFS_UPLOAD_CONCURRENCY="3"), stdout=log, stderr=log)
        try:
            run.eventually("three simultaneous S3 transfers", lambda: relay("GET", "/status")["active"] == 3, 60)
            time.sleep(1)
            assert process.poll() is None
            status = relay("GET", "/status")
            assert status["peak"] == 3 and status["active"] == 3
            held = [e for e in status["events"] if e["key"].startswith(prefix)]
            assert len(held) == 3 and all(e["state"] == "held" for e in held)
            relay("POST", "/release", {})
            assert process.wait(timeout=180) == 0
            assert relay("GET", "/status")["peak"] == 3
        finally:
            relay("POST", "/release", {})
            if process.poll() is None:
                process.kill()
            process.wait(timeout=10)
    verify_capture(state, directory, root, 64 << 20)
    print("Two packs uploaded concurrently under one three-transfer cap; exact originals retained.", flush=True)

    directory, root = fixture(state, "out-of-order-uploads", 32 << 20)
    prefix = f"sources/{state['org']}/{root}/"
    relay("POST", "/hold", {"prefix": prefix, "parts": [1]})
    with tempfile.TemporaryFile(mode="w+") as log:
        process = subprocess.Popen(["pufferfs", "sync", str(directory), "--id", root, "--no-vector"],
            env=dict(os.environ, PUFFERFS_API_KEY=state["key"], PUFFERFS_UPLOAD_CONCURRENCY="4"), stdout=log, stderr=log)
        try:
            def second_part_saved():
                assert process.poll() is None, "CLI exited before held part was released"
                for path in Path("/root/.tpfs/roots", root).glob("file-capture-*/pending/*/journal.json"):
                    for pack in json.loads(path.read_text())["packs"]:
                        if [p["part_number"] for p in (pack.get("multipart") or {}).get("parts", [])] == [2]:
                            return pack
            pack = run.eventually("durable second part before the first", second_part_saved, 60)
            process.kill()
            process.wait(timeout=10)
            relay("POST", "/release", {})
            run.eventually("late first part accepted by S3", lambda: any(e["key"] == pack["object_key"]
                and e["part"] == 1 and e.get("status") == 200 for e in relay("GET", "/status")["events"]), 30)
        finally:
            relay("POST", "/release", {})
            if process.poll() is None:
                process.kill()
            process.wait(timeout=10)
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    events = [e for e in relay("GET", "/status")["events"] if e["key"] == pack["object_key"]]
    assert sum(e["part"] == 2 for e in events) == 1, "confirmed part uploaded again"
    assert sum(e["part"] == 1 for e in events) == 2, "ambiguous first part was not replayed"
    assert len({e["sha256"] for e in events if e["part"] == 1}) == 1
    verify_capture(state, directory, root, 32 << 20)
    print("Out-of-order journal survived SIGKILL; only the unacknowledged part replayed.", flush=True)


def verify():
    state = json.loads(run.STATE.read_text())
    for case in state["upload_cases"]:
        file = run.wait_indexed(state, case["root"])["record.txt"]
        run.assert_source_retained(file)
        result = run.request("POST", f"/roots/{case['root']}/read",
            {"path": "record.txt", "lines": {"start": 1, "end": 1}}, key=state["key"])
        assert [line["content"] for line in result["lines"]] == case["content"].splitlines()
        assert run.request("POST", "/query", {"root_id": case["root"], "query": "violet", "mode": "fts"}, key=state["key"])["results"]
    print("Updated captured files published and passed exact read/search after worker startup.", flush=True)


if __name__ == "__main__":
    {"capture": capture, "verify": verify}[sys.argv[1]]()
