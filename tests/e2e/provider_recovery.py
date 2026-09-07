"""Provider recovery through actual CLI capture, SQS, workers and Gemini."""

import json
import os
from pathlib import Path
import sys
import time
import urllib.request

from google import genai
import pymupdf

import run
from api_access import servers


def relay(method, path, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request("http://provider-relay:8080" + path, data=data, method=method,
        headers={"Content-Type": "application/json", "X-E2E-Control": "e2e-provider-fault-only"})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def held(fault_id, status):
    return run.eventually("real Gemini submission to reach " + status, lambda:
        next((event for event in relay("GET", "/status")["events"]
              if event["fault_id"] == fault_id and event["state"] == status), None), 600)


def client():
    return genai.Client(api_key=os.environ["GEMINI_API_KEY"],
        http_options={"timeout": 60000, "retry_options": {"attempts": 1}})


def capture(state, name, pages, mode):
    directory = Path("/state") / name
    directory.mkdir()
    with pymupdf.open() as document:
        for text in pages:
            document.new_page().insert_text((72, 72), text, fontsize=24)
        document.save(directory / "document.pdf")
    root = run.new_root(state, name, directory, True)
    fault_id = relay("POST", "/fault", {"mode": mode})["fault_id"]
    state.update(root=root, fault_id=fault_id, expected_pages=pages)
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", root, "--no-vector")
    return held(fault_id, "response_held" if mode == "hold_response" else "request_held")


def requests(batch_id):
    batch, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (batch_id,))
    return run.provider_records(batch)


def stamp(ref):
    head = run.s3.head_object(Bucket=run.BUCKET, Key=ref)
    return {"key": ref, "etag": head["ETag"], "modified": head["LastModified"].isoformat(), "bytes": head["ContentLength"]}


def lost_capture():
    state = json.loads(run.STATE.read_text())
    # Cross the production 64-request boundary, losing the first submission's
    # response before the worker can prepare the final page or seal the count.
    pages = [f"Orchid observatory telescope {i}." for i in range(65)]
    event = capture(state, "e2e-lost-provider-response", pages, "hold_response")
    assert event["upstream_status"] == 200 and event["provider_job_id"]
    batch, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (event["batch_id"],))
    assert batch["status"] == "preparing" and batch["submission_started_at"] is not None
    assert batch["provider_job_id"] is None
    inputs = requests(event["batch_id"])
    assert len(inputs) == 64 and all(row["status"] == "pending" for row in inputs)
    extraction, = run.sql("SELECT prepared_request_count FROM file_extractions WHERE id=%s", (inputs[0]["extraction_id"],))
    assert extraction["prepared_request_count"] is None
    assert not run.sql("SELECT id FROM file_work WHERE extraction_id=%s AND stage='index'", (inputs[0]["extraction_id"],))
    reservation_counts(1, 64)
    state.update(lost_event=event, lost_inputs=inputs)
    run.save(state)
    print("Gemini accepted 64 pages with one input manifest and one batch row; response held before the final page or count seal.", flush=True)


def reservation_counts(batches, rows):
    baseline = json.loads(run.STATE.read_text()).get("manifest_baseline", {"batches": 0, "inputs": 0})
    batches += baseline["batches"]
    rows += baseline["inputs"]
    counts = run.sql("SELECT calls,rows FROM pg_stat_statements WHERE query LIKE %s",
                     ("INSERT INTO provider_batches%",))
    assert sum(row["calls"] for row in counts) == batches
    assert sum(row["rows"] for row in counts) == batches
    summary, = run.sql("SELECT COUNT(*) AS batches,SUM(request_count) AS inputs FROM provider_batches")
    assert summary == {"batches": batches, "inputs": rows}
    for table in ("provider_requests", "provider_files", "provider_batch_files"):
        assert run.sql("SELECT to_regclass(%s) AS relation", (table,))[0]["relation"] is None


def release():
    relay("POST", "/release")


def verify_pages(state):
    file, = run.wait_indexed(state).values()
    run.assert_source_retained(file)
    extraction, = run.sql("""SELECT e.* FROM file_extractions e
        JOIN file_catalog f ON f.indexed_extraction_id=e.id WHERE f.id=%s""", (file["file_id"],))
    records = list(run.chunks(extraction["chunks_ref"]))
    assert [row["chunk_index"] for row in records] == list(range(len(records)))
    print(json.dumps({"extraction_id": extraction["id"], "chunks": records}), flush=True)
    for ordinal, expected in enumerate(state["expected_pages"]):
        text = " ".join(row["content"] for row in records if row["location"]["page_number"] == ordinal).lower()
        assert all(word.strip(".").lower() in text for word in expected.split()), "missing or misordered page content"
    for peer in servers():
        result = run.request("POST", "/query", {"root_id": state["root"], "query": "observatory",
            "mode": "fts", "top_k": 10}, key=state["key"], server=peer)
        assert result["results"] and all(hit["file_path"] == file["path"] for hit in result["results"])
        read = run.request("POST", f"/roots/{state['root']}/read", {"path": file["path"],
            "pages": {"start": 1, "end": len(state["expected_pages"])}}, key=state["key"], server=peer)
        assert [page["page_number"] for page in read["pages"]] == list(range(len(state["expected_pages"])))


def lost_recovered():
    state = json.loads(run.STATE.read_text())
    verify_pages(state)
    event = state["lost_event"]
    batch, = run.sql("SELECT status,provider_job_id FROM provider_batches WHERE id=%s", (event["batch_id"],))
    assert batch == {"status": "complete", "provider_job_id": event["provider_job_id"]}
    after = requests(event["batch_id"])
    for before, current in zip(state["lost_inputs"], after, strict=True):
        assert current["input_file_id"] == before["input_file_id"] and current["attempt_count"] == 1
    batches = run.sql("""SELECT id,request_count AS count FROM provider_batches WHERE extraction_id=%s""", (after[0]["extraction_id"],))
    assert sorted(row["count"] for row in batches) == [1, 64]
    reservation_counts(2, 65)
    sent = [item for item in relay("GET", "/status")["events"] if item["batch_id"] == event["batch_id"]]
    assert len(sent) == 1, "accepted batch was submitted again after its response was lost"
    with client() as provider:
        remote = provider.batches.get(name=event["provider_job_id"])
    assert remote.display_name == event["batch_id"]
    # Every production create traverses the relay. Its single original request
    # above proves no client-side duplicate without scanning account history.
    run.provider_cleanup()
    print("Recovered the accepted 64-page job without reupload or resubmission; the final page used one new batch, all 65 pages searchable.", flush=True)


def partial_capture():
    state = json.loads(run.STATE.read_text())
    pages = ["Orchid observatory " + word + "." for word in ("sapphire", "citrine", "indigo", "vermilion")]
    event = capture(state, "e2e-partial-provider-retry", pages, "hold_request")
    inputs = requests(event["batch_id"])
    assert len(inputs) == len(pages)
    # Invalidate alternate, exact run-owned uploads after the real request
    # envelope is durable and in flight. The provider itself must produce the
    # per-request errors: we do not edit requests or synthesize responses.
    removed = [item["input_file_id"] for item in inputs if item["ordinal"] % 2]
    expirations = {}
    with client() as provider:
        for name in removed:
            upload = provider.files.get(name=name)
            assert upload.expiration_time is not None
            expirations[name] = upload.expiration_time.isoformat()
            provider.files.delete(name=name)
    state.update(partial_event=event, partial_inputs=inputs, removed_inputs=removed, removed_expirations=expirations)
    run.save(state)
    release()
    event = held(state["fault_id"], "response_released")
    assert event["upstream_status"] == 200 and event["provider_job_id"]
    state["partial_event"] = event
    run.save(state)
    run.eventually("durable provider handoff", lambda: run.sql("""SELECT id FROM file_work
        WHERE extraction_id=%s AND stage='transform' AND status='waiting_provider'""", (inputs[0]["extraction_id"],)))
    run.wait_queue_empty("transform")
    reservation_counts(3, 69)
    with client() as provider:
        def terminal():
            job = provider.batches.get(name=event["provider_job_id"])
            return job if job.state.name in {"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED"} else None
        remote = run.eventually("real partial batch completion before collection", terminal)
        assert remote.dest and remote.dest.file_name
        output = [json.loads(line) for line in provider.files.download(file=remote.dest.file_name).splitlines() if line.strip()]
    successful = {row["key"] for row in output if row.get("response") and not row.get("error")}
    assert successful == {item["request_key"] for item in inputs if item["input_file_id"] not in removed}
    print("Real Gemini produced alternating successes and failures for deleted inputs; collector has not yet processed them.", flush=True)


def partial_collected():
    state = json.loads(run.STATE.read_text())
    batch_id = state["partial_event"]["batch_id"]
    def collected():
        rows = requests(batch_id)
        return rows if len(rows) == 4 and all(row["status"] in {"complete", "failed"} for row in rows) else None
    rows = run.eventually("partial result persistence before the next scheduled retry", collected, 600)
    successes = [row for row in rows if row["status"] == "complete"]
    assert [row["ordinal"] for row in successes] == [0, 2]
    state.update(successes=successes, success_stamps=[stamp(ref) for ref in sorted({row["result_ref"] for row in successes})])
    run.save(state)
    print("Successful page artifacts are durable before failed pages retry.", flush=True)


def partial_recovered():
    state = json.loads(run.STATE.read_text())
    verify_pages(state)
    extraction_id = state["partial_inputs"][0]["extraction_id"]
    batch, = run.sql("SELECT * FROM provider_batches WHERE extraction_id=%s", (extraction_id,))
    after = run.provider_records(batch)
    assert len(after) == 4 and all(row["status"] == "complete" for row in after)
    for before, current in zip(state["partial_inputs"], after, strict=True):
        if before["input_file_id"] in state["removed_inputs"]:
            assert current["attempt_count"] == 2 and current["input_file_id"] != before["input_file_id"]
        else:
            success = next(row for row in state["successes"] if row["request_key"] == current["request_key"])
            assert current["attempt_count"] == 1 and current["input_file_id"] == before["input_file_id"]
            assert current["result_ref"] == success["result_ref"] and current["batch_id"] == before["batch_id"]
    assert [stamp(item["key"]) for item in state["success_stamps"]] == state["success_stamps"]
    assert batch["attempt_count"] == 2 and batch["provider_job_id"]
    latest = run.provider_manifest(batch["output_ref"])
    earlier = run.provider_manifest(latest["previous"])
    assert earlier["provider_job_id"] == state["partial_event"]["provider_job_id"]
    assert latest["provider_job_id"] != earlier["provider_job_id"]
    jobs = {latest["provider_job_id"], earlier["provider_job_id"]}
    assert len([event for event in relay("GET", "/status")["events"] if event.get("provider_job_id") in jobs]) == 2
    reservation_counts(3, 69)
    run.provider_cleanup(externally_deleted=state["removed_expirations"])
    print("Only failed pages were regenerated; successful result objects are unchanged, source order and public search are correct.", flush=True)


def manifest_relay(method, path, payload=None):
    request = urllib.request.Request("http://provider-manifest-relay:8080" + path,
        data=None if payload is None else json.dumps(payload).encode(), method=method,
        headers={"Content-Type": "application/json", "X-E2E-Control": "e2e-provider-manifest-only"})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def manifest_held(fault_id):
    return run.eventually("S3 manifest response held", lambda:
        next((event for event in manifest_relay("GET", "/status")["events"]
              if event["fault_id"] == fault_id and event["state"] == "response_held"), None), 600)


def manifest_capture():
    state = run.provision()
    directory = Path("/state/e2e-uncommitted-manifest")
    directory.mkdir()
    pages = [f"Orchid observatory uncommitted telescope {i}." for i in range(17)]
    with pymupdf.open() as document:
        for text in pages:
            document.new_page().insert_text((72, 72), text, fontsize=24)
        document.save(directory / "document.pdf")
    state.update(root=run.new_root(state, "e2e-uncommitted-manifest", directory, True), expected_pages=pages)
    state["manifest_fault"] = manifest_relay("POST", "/fault", {"kind": "input", "mode": "hold_response"})["fault_id"]
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", state["root"], "--no-vector")
    event = manifest_held(state["manifest_fault"])
    assert event["upstream_status"] == 200
    assert not run.sql("SELECT id FROM provider_batches"), "worker committed a batch before its S3 PUT completed"
    manifest = run.provider_manifest(event["key"])
    assert len(manifest["requests"]) == 17 and len(manifest["uploads"]) == 18
    assert not relay("GET", "/status")["events"], "uncommitted manifest submitted paid work"
    state.update(orphan_manifest=event["key"], orphan_uploads=manifest["uploads"])
    run.save(state)
    print("S3 has the input manifest, but its response is held: no batch row or paid submission exists.", flush=True)


def manifest_release():
    manifest_relay("POST", "/release")


def manifest_recovered():
    state = json.loads(run.STATE.read_text())
    verify_pages(state)
    batch, = run.sql("SELECT * FROM provider_batches WHERE root_id=%s", (state["root"],))
    assert batch["attempt_count"] == 1 and batch["request_count"] == 17
    assert batch["input_ref"] != state["orphan_manifest"]
    old_ids = {item["file_id"] for item in state["orphan_uploads"]}
    assert old_ids.isdisjoint(run.provider_uploads(batch))
    events = relay("GET", "/status")["events"]
    assert len(events) == 1 and events[0]["provider_job_id"] == batch["provider_job_id"]
    counts = manifest_relay("GET", "/status")["counts"]
    assert counts["input"]["puts"] == 2, counts
    state["manifest_baseline"] = {"batches": 1, "inputs": 17}
    run.save(state)
    run.provider_cleanup()
    print("Uncommitted preparation retried as one batch: two S3 manifests, one PG row, one paid job, all 17 pages indexed.", flush=True)


def result_arm():
    state = json.loads(run.STATE.read_text())
    state["result_fault"] = manifest_relay("POST", "/fault", {"kind": "result", "mode": "hold_response"})["fault_id"]
    run.save(state)


def result_held():
    state = json.loads(run.STATE.read_text())
    event = manifest_held(state["result_fault"])
    assert event["upstream_status"] == 200
    batch, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (state["partial_event"]["batch_id"],))
    assert batch["status"] == "submitted" and not batch["output_ref"]
    assert batch["provider_job_id"] == state["partial_event"]["provider_job_id"]
    state["uncommitted_result_ref"] = event["key"]
    run.save(state)
    print("Result manifest is durable in S3 but uncommitted in PG; collector restart must recover the same paid job.", flush=True)


if __name__ == "__main__":
    phase = sys.argv[1]
    actions = {"manifest-capture": manifest_capture, "manifest-release": manifest_release,
               "manifest-recovered": manifest_recovered, "result-arm": result_arm, "result-held": result_held,
               "lost-capture": lost_capture, "release": release, "lost-recovered": lost_recovered,
               "partial-capture": partial_capture, "partial-collected": partial_collected,
               "partial-recovered": partial_recovered}
    started, status = time.monotonic(), "failed"
    try:
        actions[phase]()
        status = "passed"
    finally:
        state = json.loads(run.STATE.read_text()) if run.STATE.exists() else {}
        with run.REPORT.open("a") as output:
            output.write(json.dumps({"run_id": state.get("nonce"), "phase": "provider-" + phase,
                "status": status, "seconds": round(time.monotonic() - started, 2)}) + "\n")
        print(f"provider-{phase}: {status}", flush=True)
