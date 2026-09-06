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


def relay(method, path, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request("http://provider-relay:8080" + path, data=data, method=method,
        headers={"Content-Type": "application/json", "X-E2E-Control": "e2e-provider-fault-only"})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def held(fault_id, status):
    return run.eventually("real Gemini submission to reach " + status, lambda:
        next((event for event in relay("GET", "/status")["events"]
              if event["fault_id"] == fault_id and event["state"] == status), None), 180)


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
    return run.sql("""SELECT request_key,ordinal,extraction_id,batch_id,input_file_id,
        status,attempt_count,result_ref FROM provider_requests WHERE batch_id=%s ORDER BY ordinal""", (batch_id,))


def stamp(ref):
    head = run.s3.head_object(Bucket=run.BUCKET, Key=ref)
    return {"key": ref, "etag": head["ETag"], "modified": head["LastModified"].isoformat(), "bytes": head["ContentLength"]}


def lost_capture():
    state = run.provision()
    event = capture(state, "e2e-lost-provider-response", ["Orchid observatory telescope."], "hold_response")
    assert event["upstream_status"] == 200 and event["provider_job_id"]
    batch, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (event["batch_id"],))
    assert batch["status"] == "preparing" and batch["submission_started_at"] is not None
    assert batch["provider_job_id"] is None
    inputs = requests(event["batch_id"])
    assert len(inputs) == 1 and inputs[0]["status"] == "pending"
    state.update(lost_event=event, lost_inputs=inputs)
    run.save(state)
    print("Gemini accepted one paid batch; its response is held and Postgres has only the pre-submission marker.", flush=True)


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
    result = run.request("POST", "/query", {"root_id": state["root"], "query": "observatory",
        "mode": "fts", "top_k": 10}, key=state["key"])
    assert result["results"] and all(hit["file_path"] == file["path"] for hit in result["results"])


def lost_recovered():
    state = json.loads(run.STATE.read_text())
    verify_pages(state)
    event = state["lost_event"]
    batch, = run.sql("SELECT status,provider_job_id FROM provider_batches WHERE id=%s", (event["batch_id"],))
    assert batch == {"status": "complete", "provider_job_id": event["provider_job_id"]}
    before, = state["lost_inputs"]
    after, = requests(event["batch_id"])
    assert after["input_file_id"] == before["input_file_id"] and after["attempt_count"] == 1
    sent = [item for item in relay("GET", "/status")["events"] if item["batch_id"] == event["batch_id"]]
    assert len(sent) == 1, "accepted batch was submitted again after its response was lost"
    with client() as provider:
        matches = [job.name for job in provider.batches.list() if job.display_name == event["batch_id"]]
    assert matches == [event["provider_job_id"]], "more than one real paid provider job"
    run.provider_cleanup()
    print("Provider listing recovered the exact accepted job; no second submission or media upload, and search succeeds.", flush=True)


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
    rows = run.eventually("partial result persistence before the next scheduled retry", collected, 90)
    successes = [row for row in rows if row["status"] == "complete"]
    assert [row["ordinal"] for row in successes] == [0, 2]
    state.update(successes=successes, success_stamps=[stamp(ref) for ref in sorted({row["result_ref"] for row in successes})])
    run.save(state)
    print("Successful page artifacts are durable before failed pages retry.", flush=True)


def partial_recovered():
    state = json.loads(run.STATE.read_text())
    verify_pages(state)
    extraction_id = state["partial_inputs"][0]["extraction_id"]
    after = run.sql("SELECT * FROM provider_requests WHERE extraction_id=%s ORDER BY ordinal", (extraction_id,))
    assert len(after) == 4 and all(row["status"] == "complete" for row in after)
    for before, current in zip(state["partial_inputs"], after, strict=True):
        if before["input_file_id"] in state["removed_inputs"]:
            assert current["attempt_count"] == 2 and current["input_file_id"] != before["input_file_id"]
        else:
            success = next(row for row in state["successes"] if row["request_key"] == current["request_key"])
            assert current["attempt_count"] == 1 and current["input_file_id"] == before["input_file_id"]
            assert current["result_ref"] == success["result_ref"] and current["batch_id"] == before["batch_id"]
    assert [stamp(item["key"]) for item in state["success_stamps"]] == state["success_stamps"]
    batches = run.sql("SELECT id,provider_job_id FROM provider_batches WHERE id=%s OR retry_of=%s", (state["partial_event"]["batch_id"],) * 2)
    assert len(batches) == 2 and all(row["provider_job_id"] for row in batches)
    ids = {row["id"] for row in batches}
    assert len([event for event in relay("GET", "/status")["events"] if event["batch_id"] in ids]) == 2
    run.provider_cleanup(externally_deleted=state["removed_expirations"])
    print("Only failed pages were regenerated; successful result objects are unchanged, source order and public search are correct.", flush=True)


if __name__ == "__main__":
    phase = sys.argv[1]
    actions = {"lost-capture": lost_capture, "release": release, "lost-recovered": lost_recovered,
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
