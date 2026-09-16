"""Real Gemini partial failure -> Modal vision -> public page read/search."""

import json
import os
import sys
import time

import provider_recovery as recovery
import run


def capture():
    state = run.provision()
    pages = ["Orchid observatory " + word + "." for word in ("sapphire", "citrine", "indigo", "vermilion")]
    scenario = os.environ.get("PUFFERFS_E2E_VISION_CASE", "partial")
    fixtures = {
        "partial": {"mode": "corrupt_inputs", "ordinals": [1, 3], "providers": ["gemini", "modal", "gemini", "modal"], "error_code": None},
        "cancel": {"mode": "hold_response", "ordinals": [], "providers": ["modal"] * 4, "error_code": 1},
    }
    fixture = fixtures[scenario]
    event = recovery.capture(state, "e2e-vision-fallback", pages, fixture["mode"], ordinals=fixture["ordinals"])
    inputs = recovery.requests(event["batch_id"])
    assert event.get("corrupted_keys", []) == [inputs[n]["request_key"] for n in fixture["ordinals"]]
    if scenario == "cancel":
        # Cancel only this run-owned real Google job, before its accepted
        # response reaches the transformation worker. No fabricated results.
        with recovery.client() as provider:
            provider.batches.cancel(name=event["provider_job_id"])
    recovery.release()
    event = recovery.held(state["fault_id"], "response_released")
    assert event["upstream_status"] == 200 and event["provider_job_id"]
    state.update(partial_event=event, original_inputs=inputs, expected_providers=fixture["providers"],
                 expected_error_code=fixture["error_code"], vision_case=scenario)
    run.save(state)


def verify():
    state = json.loads(run.STATE.read_text())
    recovery.verify_pages(state)
    event = state["partial_event"]
    batch, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (event["batch_id"],))
    assert batch["status"] == "complete" and batch["attempt_count"] == 1
    assert batch["provider_job_id"] == event["provider_job_id"]
    records = run.provider_records(batch)
    assert [r["result_provider"] for r in records] == state["expected_providers"]
    assert all(r["attempt_count"] == 1 and r["status"] == "complete" for r in records)
    assert all(r["result_model"] == os.environ["PUFFERFS_VISION_MODEL"] for r in records if r["result_provider"] == "modal")
    manifest = run.provider_manifest(batch["input_ref"])
    assert len(manifest["uploads"]) == 1
    assert all("input_uri" not in r and "input_file_id" not in r for r in manifest["requests"])
    with recovery.client() as provider:
        remote = provider.batches.get(name=batch["provider_job_id"])
        assert remote.state.name in {"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED"}
        raw = provider.files.download(file=remote.dest.file_name) if remote.dest and remote.dest.file_name else b""
        output = [json.loads(line) for line in raw.splitlines() if line.strip()]
    successful = {r["key"] for r in output if r.get("response") and not r.get("error")}
    assert successful == {r["request_key"] for r in records if r["result_provider"] == "gemini"}
    if state["expected_error_code"] is not None:
        # Cancellation is asynchronous/best-effort. Google can represent it
        # as an operation error or as item errors in a finished batch.
        # Require the supplied cancellation code, not merely missing results.
        if remote.error:
            assert remote.error.code == state["expected_error_code"]
        else:
            assert len(output) == len(records)
            assert all(r.get("error", {}).get("code") == state["expected_error_code"] for r in output)
    assert len(recovery.relay("GET", "/status")["events"]) == 1, "fallback resubmitted Gemini"
    run.provider_cleanup()
    print(f"Vision case {state['vision_case']}: providers matched {state['expected_providers']}. All four pages survived collector restart and passed public read/search, with one Gemini job and one cleaned upload.", flush=True)


if __name__ == "__main__":
    started, status = time.monotonic(), "failed"
    try:
        {"capture": capture, "verify": verify,
         "result-held": lambda: recovery.result_held(timeout=run.TIMEOUT)}[sys.argv[1]]()
        status = "passed"
    finally:
        with run.REPORT.open("a") as output:
            output.write(json.dumps({"phase": "vision-" + sys.argv[1], "status": status,
                                    "seconds": round(time.monotonic() - started, 2)}) + "\n")
        print(f"vision-{sys.argv[1]}: {status}", flush=True)
