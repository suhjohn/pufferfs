"""Black-box in-flight index faults through real CLI, workers and Turbopuffer."""

import json
import hashlib
import os
from pathlib import Path
import sys
import time
import urllib.error
import urllib.request
import uuid

import run


def relay(method, path, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request("http://index-relay:8080" + path, data=data, method=method,
        headers={"Content-Type": "application/json", "X-E2E-Control": "e2e-index-fault-only"})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def arm(root, mode, count=1):
    names = run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=%s AND retired_at IS NULL", (root,))
    assert names
    return relay("POST", "/fault", {"namespaces": [row["namespace"] for row in names],
                                   "mode": mode, "count": count})["fault_id"]


def held(fault_id, state):
    def ready():
        return next((event for event in relay("GET", "/status")["events"]
                     if event["fault_id"] == fault_id and event["state"] == state), None)
    return run.eventually("the real index request to reach " + state, ready, 180)


def work(root, version):
    rows = run.sql("""SELECT w.id,w.status,w.attempt_count,w.attempt_token,w.lease_until::text,
        w.index_cursor,e.chunks_ref,w.extraction_id
        FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
        JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
        WHERE f.root_id=%s AND v.id=%s""", (root, version))
    assert len(rows) == 1
    return rows[0]


def published(state, root, version, timeout=900):
    def ready():
        rows = work(root, version)
        assert rows["status"] != "failed", "publication exhausted its retry budget"
        file = run.catalog(state, root)["record.txt"]
        return file if file["indexed_version_id"] == version and file["processing"]["status"] == "complete" else None
    return run.eventually("exact captured version publication after index crash", ready, timeout)


def search(state, root, term, mode="fts"):
    return run.request("POST", "/query", {"root_id": root, "query": term,
        "mode": mode, "top_k": 5}, key=state["key"])["results"]


def object_stamp(key):
    obj = run.s3.head_object(Bucket=run.BUCKET, Key=key)
    return {"key": key, "etag": obj["ETag"], "modified": obj["LastModified"].isoformat(), "bytes": obj["ContentLength"]}


def raw_rows(namespace, extraction):
    # Read-only provider assertion: prove stale physical rows actually exist,
    # independently of the API's publication filtering. Never write index rows.
    body = {"rank_by": ["chunk_index", "asc"], "limit": 10,
            "filters": ["extraction_id", "Eq", extraction], "include_attributes": ["content"]}
    request = urllib.request.Request(os.environ["TURBOPUFFER_API_URL"].rstrip("/")
        + "/v2/namespaces/" + namespace + "/query", data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + os.environ["TURBOPUFFER_API_KEY"], "Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)["rows"]


def lost_capture():
    state = run.provision()
    directory = Path("/state/lost-index-response")
    directory.mkdir()
    (directory / "record.txt").write_text("Orchid observatory telescope studies distant galaxies.\n")
    state["root"] = run.new_root(state, "e2e-lost-index-response", directory, False)
    state["lost_fault"] = arm(state["root"], "hold_response")
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", state["root"])
    event = held(state["lost_fault"], "response_held")
    assert event["upstream_status"] == 200
    file = run.catalog(state)["record.txt"]
    row = work(state["root"], file["version_id"])
    assert row["status"] == "running" and row["attempt_count"] == 1
    assert row["chunks_ref"]
    assert not file["indexed_version_id"]
    assert raw_rows(event["namespace"], row["extraction_id"]), "held write did not reach real Turbopuffer"
    assert not search(state, state["root"], "Orchid"), "unacknowledged write became publicly visible"
    records = list(run.chunks(row["chunks_ref"]))
    assert all("vector" not in record and isinstance(record["content"], str) for record in records)
    state.update(lost_version=file["version_id"], lost_work=row, lost_event=event,
                 stamps=[object_stamp(row["chunks_ref"])])
    run.save(state)
    print("Canonical chunks are durable; Turbopuffer write succeeded, but publication remains unacknowledged.")


def release():
    relay("POST", "/release")


def database_recovered():
    state = json.loads(run.STATE.read_text())
    run.eventually("API database readiness after Postgres restart",
        lambda: run.request("GET", "/readyz", statuses=(200, 503)).get("status") == "ready", 90)
    directory = Path("/state/lost-index-response")
    source = (directory / "record.txt").read_text()
    path = directory / "reconnected.txt"
    path.write_text(source)
    run.cli(state, "sync", str(directory), "--id", state["root"])
    files = run.wait_indexed(state)
    def cleaned():
        return run.sql("""SELECT count(*) AS pending FROM file_catalog
            WHERE root_id=%s AND index_cleanup_due_at<NOW()+INTERVAL '1 hour'""",
            (state["root"],))[0]["pending"] == 0
    run.eventually("scheduled cleanup of the published native-vector files", cleaned, 180)
    run.assert_index_vectors(state, state["root"], dimensions=4096)
    run.assert_source_retained(files[path.name])
    result = run.request("POST", f"/roots/{state['root']}/read",
        {"path": path.name, "lines": {"start": 1, "end": 1}}, key=state["key"])
    assert result["lines"][0]["content"] == source.rstrip("\n")
    for mode in ("fts", "vector", "hybrid"):
        assert search(state, state["root"], "observatory telescope", mode)
    print("After Postgres restart and scheduled cleanup, native vectors/schema, all searches and exact reads remain valid.")


def lost_recovered():
    state = json.loads(run.STATE.read_text())
    published(state, state["root"], state["lost_version"])
    row = work(state["root"], state["lost_version"])
    assert row["attempt_count"] == 2 and row["attempt_token"] != state["lost_work"]["attempt_token"]
    assert row["chunks_ref"] == state["lost_work"]["chunks_ref"] and row["status"] == "complete"
    assert [object_stamp(stamp["key"]) for stamp in state["stamps"]] == state["stamps"], "crash recovery rewrote canonical chunks"
    events = [event for event in relay("GET", "/status")["events"]
              if event["namespace"] == state["lost_event"]["namespace"] and event.get("upstream_status") == 200 and event["operation"] == "write"]
    print(json.dumps({"replayed_index_requests": events}), flush=True)
    # The Python 3.12 SDK's gzip header includes the current timestamp. Compare
    # exact decompressed JSON bytes, without parsing/reserializing mutations.
    assert len(events) >= 2 and all(event["payload_sha256"] == state["lost_event"]["payload_sha256"] for event in events)
    for mode in ("fts", "vector", "hybrid"):
        assert search(state, state["root"], "observatory telescope", mode)
    run.wait_work_idle("index")
    print("A new worker attempt replayed identical provider request bytes after the normal lease; all search modes work.")


def admission_capture():
    state = json.loads(run.STATE.read_text())
    directory = Path("/state/admission")
    directory.mkdir()
    root = run.new_root(state, "Bounded publication", directory, True)
    slots = int(os.environ["PUFFERFS_WORKER_CONCURRENCY"])
    expected = {f"observation-{i}.txt": f"Orchid telescope calibration observation {i}.\n" for i in range(slots * 2)}
    for name, content in expected.items():
        (directory / name).write_text(content)
    case = {"root":root,"expected":expected,"slots":slots,"fault":arm(root,"hold_response",count=len(expected))}
    state["admission"] = case
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", root)
    def full():
        events = [e for e in relay("GET", "/status")["events"] if e["fault_id"]==case["fault"] and e["state"]=="response_held"]
        return len(events)==slots
    run.eventually("configured publication slots occupied", full, 120)


def admission_bounded():
    state = json.loads(run.STATE.read_text())
    case = state["admission"]
    for _ in range(5):
        rows = run.sql("""SELECT w.status,count(*) AS n FROM file_work w
            JOIN file_extractions e ON e.id=w.extraction_id JOIN file_versions v ON v.id=e.version_id
            JOIN file_catalog f ON f.id=v.file_id WHERE f.root_id=%s AND w.stage='index' GROUP BY w.status""", (case["root"],))
        counts = {r["status"]:r["n"] for r in rows}
        assert counts.get("running",0)==case["slots"] and counts.get("pending",0)==case["slots"]
        time.sleep(2)
    release()
    files = run.wait_indexed(state,case["root"])
    assert set(files)==set(case["expected"])
    for name,content in case["expected"].items():
        result = json.loads(run.cli(state,"read",name,"--root",case["root"],"--lines","1:1","--json"))
        assert result["lines"][0]["content"]==content.rstrip("\n")
    print("Database claims remained bounded while real provider responses were held; every file then published.",flush=True)


def pause_deletions():
    relay("POST", "/deletions", {"paused": True})


def live_superseded():
    from api_access import servers
    from source_retention import upload

    state = json.loads(run.STATE.read_text())
    peers = servers()
    for deleted in (False, True):
        root = run.new_root(state, "Live publication supersession", "/state/live-publication-"+str(deleted), True)

        def capture(text, previous, peer):
            file = {"path":"record.txt","previous_version_id":previous}
            if text is None:
                file["deleted"] = True
            else:
                data = text.encode()
                key = upload(state,root,data)["object_key"]
                file["source"] = {"format":1,"size":len(data),
                    "content_hash":"sha256:"+hashlib.sha256(data).hexdigest(),
                    "extents":[{"object_key":key,"offset":0,"length":len(data)}]}
            return run.request("POST",f"/roots/{root}/versions",{"capture_id":str(uuid.uuid4()),"files":[file]},
                key=state["key"],server=peer,statuses=(202,))["versions"][0]["version_id"]

        initial = capture("Original orchid calibration notes.\n","",peers[0])
        published(state,root,initial)
        fault = arm(root,"hold_response")
        old = capture("Pending citrine calibration notes.\n",initial,peers[0])
        event = held(fault,"response_held")
        assert event["upstream_status"] == 200
        before = work(root,old)
        assert before["status"] == "running"
        stamp = object_stamp(before["chunks_ref"])
        latest = capture(None if deleted else "Current vermilion calibration notes.\n",old,peers[1])
        assert run.catalog(state,root)["record.txt"]["indexed_version_id"] == initial
        release()  # The original worker remains alive and receives the real acknowledgment.
        published(state,root,latest)
        after = work(root,old)
        assert after["status"] == "superseded"
        assert after["attempt_count"] == 1 and after["attempt_token"] == before["attempt_token"]
        assert object_stamp(after["chunks_ref"]) == stamp
        for peer in peers:
            assert not run.request("POST","/query",{"root_id":root,"query":"citrine","mode":"fts"},
                key=state["key"],server=peer)["results"]
            result = run.request("POST",f"/roots/{root}/read",{"path":"record.txt","lines":{"start":1,"end":1}},
                key=state["key"],server=peer,statuses=(404,) if deleted else (200,))
            if not deleted:
                assert result["lines"][0]["content"] == "Current vermilion calibration notes."
        print(f"Live index acknowledgment fenced by a newer {'tombstone' if deleted else 'capture'} from the second API; one attempt, canonical chunks unchanged.",flush=True)


def stale_capture():
    state = json.loads(run.STATE.read_text())
    directory = Path("/state/stale-index-write")
    directory.mkdir()
    path = directory / "record.txt"
    path.write_text("Original sapphire observatory notes.\n")
    root = run.new_root(state, "e2e-stale-index-write", directory, True)
    state["stale_root"] = root
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    first = run.catalog(state, root)["record.txt"]["version_id"]
    published(state, root, first)
    state["stale_fault"] = arm(root, "hold_request")
    run.save(state)
    path.write_text("Stale citrine observatory notes.\n")
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    event = held(state["stale_fault"], "request_held")
    second = run.catalog(state, root)["record.txt"]["version_id"]
    row = work(root, second)
    assert row["status"] == "running" and row["chunks_ref"]
    assert search(state, root, "sapphire") and not search(state, root, "citrine")
    path.write_text("Current vermilion observatory notes.\n")
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    third = run.catalog(state, root)["record.txt"]["version_id"]
    assert len({first, second, third}) == 3
    state.update(stale_version=second, current_version=third, stale_work=row, stale_event=event)
    run.save(state)
    print("Second-version write is held in flight; third-version capture succeeded while the first remains published.")


def stale_current():
    state = json.loads(run.STATE.read_text())
    published(state, state["stale_root"], state["current_version"])
    run.eventually("expired old work to be superseded",
        lambda: work(state["stale_root"], state["stale_version"])["status"] == "superseded", timeout=360)
    assert search(state, state["stale_root"], "vermilion")
    assert held(state["stale_fault"], "request_held")
    print("After the worker crash, the third version published and the old attempt was superseded; the old network write remains held.")


def stale_released():
    state = json.loads(run.STATE.read_text())
    release()
    event = held(state["stale_fault"], "response_released")
    assert event["upstream_status"] == 200
    rows = raw_rows(event["namespace"], state["stale_work"]["extraction_id"])
    assert rows and all("Stale citrine" in row["content"] for row in rows)
    for _ in range(3):
        assert not search(state, state["stale_root"], "citrine"), "late stale index write leaked through search"
        assert search(state, state["stale_root"], "vermilion")
        result = run.request("POST", f"/roots/{state['stale_root']}/read",
            {"path": "record.txt", "lines": {"start": 1, "end": 1}}, key=state["key"])
        assert result["lines"][0]["content"] == "Current vermilion observatory notes."
    run.eventually("expired old work to be superseded",
        lambda: work(state["stale_root"], state["stale_version"])["status"] == "superseded", timeout=360)
    print("Real Turbopuffer accepted stale rows after newer publication; reads/search still expose only the current version.")


def root_deleted():
    state = json.loads(run.STATE.read_text()) if run.STATE.exists() else run.provision()
    directory = Path("/state/deleted-root-write")
    directory.mkdir()
    path = directory / "record.txt"
    path.write_text("Original indigo observatory notes.\n")
    root = run.new_root(state, "e2e-deleted-root-write", directory, True)
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    run.wait_indexed(state, root)
    fault_id = arm(root, "hold_request")
    path.write_text("Late violet observatory notes.\n")
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    event = held(fault_id, "request_held")
    version = run.catalog(state, root)["record.txt"]["version_id"]
    row = work(root, version)
    assert row["status"] == "running"
    assert search(state, root, "indigo") and not search(state, root, "violet")
    state.update(deleted_root=root, deleted_work=row, deleted_fault=fault_id, deleted_event=event)
    run.save(state)
    # Delete through the public API while the actual worker's write is still
    # held in flight. The shell keeps reconciliation stopped until after the
    # late physical rows have been independently observed.
    run.request("DELETE", f"/roots/{root}", key=state["key"])
    assert not run.sql("SELECT id FROM roots WHERE id=%s", (root,))
    assert not run.sql("SELECT id FROM file_work WHERE id=%s", (row["id"],))
    targets = run.sql("SELECT kind,target,last_checked_at FROM root_cleanup_targets WHERE root_id=%s", (root,))
    assert targets and all(t["last_checked_at"] is None for t in targets)
    assert any(t["kind"] == "namespace" and t["target"] == event["namespace"] for t in targets)
    release()
    event = held(fault_id, "response_released")
    assert event["upstream_status"] == 200
    rows = raw_rows(event["namespace"], row["extraction_id"])
    assert rows and all("Late violet" in r["content"] for r in rows)
    for method, endpoint, body in (
        ("GET", f"/roots/{root}/captured-files", None),
        ("POST", f"/roots/{root}/read", {"path": "record.txt", "lines": {"start": 1, "end": 1}}),
        ("POST", "/query", {"root_id": root, "query": "violet", "mode": "fts", "top_k": 5}),
    ):
        run.request(method, endpoint, body, key=state["key"], statuses=(404,))
    print("Root deletion finished while an index write was held; late real provider rows exist but catalog/read/search deny access.")


def root_cleaned():
    relay("POST", "/deletions", {"paused": False})
    state = json.loads(run.STATE.read_text())
    root = state["deleted_root"]
    def checked():
        targets = run.sql("""SELECT kind,target,last_checked_at FROM root_cleanup_targets
            WHERE root_id=%s""", (root,))
        return targets if targets and all(t["last_checked_at"] for t in targets) else None
    targets = run.eventually("scheduled root cleanup after the late write", checked, 300)
    try:
        rows = raw_rows(state["deleted_event"]["namespace"], state["deleted_work"]["extraction_id"])
    except urllib.error.HTTPError as error:
        assert error.code == 404
        rows = []
    assert not rows, "scheduled cleanup left late physical rows"
    for target in targets:
        if target["kind"] != "namespace":
            assert not run.s3.list_objects_v2(Bucket=run.BUCKET, Prefix=target["target"]).get("Contents")
    assert not run.sql("SELECT id FROM roots WHERE id=%s", (root,))
    run.wait_work_idle("index")
    print("The real scheduled reconciler removed late index rows using durable deletion artifacts; tombstones survived catalog deletion.")


if __name__ == "__main__":
    phase = sys.argv[1]
    phases = {"lost-capture": lost_capture, "release": release, "lost-recovered": lost_recovered,
              "database-recovered": database_recovered, "live-superseded": live_superseded,
              "admission-capture": admission_capture, "admission-bounded": admission_bounded,
              "pause-deletions": pause_deletions,
              "stale-capture": stale_capture, "stale-current": stale_current, "stale-released": stale_released,
              "root-deleted": root_deleted, "root-cleaned": root_cleaned}
    started, status = time.monotonic(), "failed"
    try:
        phases[phase]()
        status = "passed"
    finally:
        with run.REPORT.open("a") as output:
            state = json.loads(run.STATE.read_text()) if run.STATE.exists() else {}
            output.write(json.dumps({"run_id": state.get("nonce"), "phase": "index-" + phase,
                                     "status": status, "seconds": round(time.monotonic() - started, 2)}) + "\n")
        print(f"index-{phase}: {status}", flush=True)
