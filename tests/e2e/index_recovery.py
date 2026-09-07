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


def arm(root, mode):
    names = run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=%s AND retired_at IS NULL", (root,))
    assert names
    return relay("POST", "/fault", {"namespaces": [row["namespace"] for row in names], "mode": mode})["fault_id"]


def held(fault_id, state):
    def ready():
        return next((event for event in relay("GET", "/status")["events"]
                     if event["fault_id"] == fault_id and event["state"] == state), None)
    return run.eventually("the real index request to reach " + state, ready, 180)


def work(root, version):
    rows = run.sql("""SELECT w.id,w.status,w.attempt_count,w.attempt_token,w.mutation_ref,
        w.mutation_batch_count,w.acknowledged_batches,w.extraction_id
        FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
        JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
        WHERE f.root_id=%s AND v.id=%s AND w.stage='index'""", (root, version))
    assert len(rows) == 1
    return rows[0]


def published(state, root, version, timeout=900):
    queue = run.sqs.get_queue_url(QueueName="file-index-dlq.fifo")["QueueUrl"]
    def ready():
        attributes = run.sqs.get_queue_attributes(QueueUrl=queue,
            AttributeNames=["ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible"])["Attributes"]
        assert not any(int(value) for value in attributes.values()), "index work reached DLQ before crash recovery"
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
    run.worker_authentication()
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
    assert row["mutation_ref"] and row["mutation_batch_count"] == 1 and row["acknowledged_batches"] == 0
    assert not file["indexed_version_id"]
    assert raw_rows(event["namespace"], row["extraction_id"]), "held write did not reach real Turbopuffer"
    assert not search(state, state["root"], "Orchid"), "unacknowledged write became publicly visible"
    vectors = run.embedding_locations(state["org"])
    assert len(vectors) == 1 and vectors[0]["dimensions"] == 768
    state.update(lost_version=file["version_id"], lost_work=row, lost_event=event, vectors=vectors,
                 stamps=[object_stamp(key) for key in [row["mutation_ref"], vectors[0]["object_key"]]])
    run.save(state)
    print("Real Nomic vectors and mutation are durable; Turbopuffer accepted the write, but publication remains unacknowledged.")


def release():
    relay("POST", "/release")


def database_recovered():
    state = json.loads(run.STATE.read_text())
    run.eventually("API database readiness after Postgres restart",
        lambda: run.request("GET", "/readyz", statuses=(200, 503)).get("status") == "ready", 90)
    vectors = run.embedding_locations(state["org"])
    directory = Path("/state/lost-index-response")
    source = (directory / "record.txt").read_text()
    path = directory / "reconnected.txt"
    path.write_text(source)
    run.cli(state, "sync", str(directory), "--id", state["root"])
    files = run.wait_indexed(state)
    assert run.embedding_locations(state["org"]) == vectors, "reconnection lost the durable cache"
    run.assert_source_retained(files[path.name])
    result = run.request("POST", f"/roots/{state['root']}/read",
        {"path": path.name, "lines": {"start": 1, "end": 1}}, key=state["key"])
    assert result["lines"][0]["content"] == source.rstrip("\n")
    print("After an actual Postgres restart, existing worker pools reconnected and reused the S3 vector cache; exact read passed.")


def lost_recovered():
    state = json.loads(run.STATE.read_text())
    published(state, state["root"], state["lost_version"])
    row = work(state["root"], state["lost_version"])
    assert row["attempt_count"] == 2 and row["attempt_token"] != state["lost_work"]["attempt_token"]
    assert row["mutation_ref"] == state["lost_work"]["mutation_ref"] and row["acknowledged_batches"] == 1
    assert [object_stamp(stamp["key"]) for stamp in state["stamps"]] == state["stamps"], "crash recovery rewrote vectors or mutations"
    events = [event for event in relay("GET", "/status")["events"]
              if event["namespace"] == state["lost_event"]["namespace"] and event.get("upstream_status") == 200]
    print(json.dumps({"replayed_index_requests": events}), flush=True)
    # The Python 3.12 SDK's gzip header includes the current timestamp. Compare
    # exact decompressed JSON bytes, without parsing/reserializing mutations.
    assert len(events) >= 2 and all(event["payload_sha256"] == state["lost_event"]["payload_sha256"] for event in events)
    for mode in ("fts", "vector", "hybrid"):
        assert search(state, state["root"], "observatory telescope", mode)
    run.wait_queue_empty("index")
    print("A new worker attempt replayed identical index payload bytes after the normal lease; vector/mutation objects were unchanged and all search modes work.")


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
        assert before["status"] == "running" and before["acknowledged_batches"] == 0
        stamp = object_stamp(before["mutation_ref"])
        latest = capture(None if deleted else "Current vermilion calibration notes.\n",old,peers[1])
        assert run.catalog(state,root)["record.txt"]["indexed_version_id"] == initial
        release()  # The original worker remains alive and receives the real acknowledgment.
        published(state,root,latest)
        after = work(root,old)
        assert after["status"] == "superseded" and after["acknowledged_batches"] == 1
        assert after["attempt_count"] == 1 and after["attempt_token"] == before["attempt_token"]
        assert object_stamp(after["mutation_ref"]) == stamp
        for peer in peers:
            assert not run.request("POST","/query",{"root_id":root,"query":"citrine","mode":"fts"},
                key=state["key"],server=peer)["results"]
            result = run.request("POST",f"/roots/{root}/read",{"path":"record.txt","lines":{"start":1,"end":1}},
                key=state["key"],server=peer,statuses=(404,) if deleted else (200,))
            if not deleted:
                assert result["lines"][0]["content"] == "Current vermilion calibration notes."
        print(f"Live index acknowledgment fenced by a newer {'tombstone' if deleted else 'capture'} from the second API; one attempt, durable mutation unchanged.",flush=True)


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
    assert row["status"] == "running" and row["acknowledged_batches"] == 0 and row["mutation_ref"]
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
    row = work(state["stale_root"], state["stale_version"])
    assert row["status"] == "superseded" and row["acknowledged_batches"] == 0
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
    row = work(state["stale_root"], state["stale_version"])
    assert row["status"] == "superseded" and row["acknowledged_batches"] == 0
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
    assert row["status"] == "running" and row["acknowledged_batches"] == 0
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
    state = json.loads(run.STATE.read_text())
    root = state["deleted_root"]
    def checked():
        targets = run.sql("""SELECT kind,target,mutation_ref,last_checked_at FROM root_cleanup_targets
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
        if target["kind"] == "namespace":
            assert target["mutation_ref"]
            run.s3.head_object(Bucket=run.BUCKET, Key=target["mutation_ref"])
        else:
            assert not run.s3.list_objects_v2(Bucket=run.BUCKET, Prefix=target["target"]).get("Contents")
    assert not run.sql("SELECT id FROM roots WHERE id=%s", (root,))
    run.wait_queue_empty("index")
    print("The real scheduled reconciler removed late index rows using durable deletion artifacts; tombstones survived catalog deletion.")


if __name__ == "__main__":
    phase = sys.argv[1]
    phases = {"lost-capture": lost_capture, "release": release, "lost-recovered": lost_recovered,
              "database-recovered": database_recovered, "live-superseded": live_superseded,
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
