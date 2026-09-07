"""Delete a root while an accepted provider submission response is in flight."""

import json
import time

from api_access import servers
import provider_recovery as recovery
import run


def verify():
    state = run.provision()
    event = recovery.capture(state, "e2e-provider-root-deletion",
                             ["Orchid observatory erasure one.", "Orchid observatory erasure two."], "hold_response")
    assert event["upstream_status"] == 200 and event["provider_job_id"]
    state["deletion_event"] = event
    run.save(state)
    batch, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (event["batch_id"],))
    assert batch["submission_started_at"] and batch["provider_job_id"] is None
    uploads = run.provider_uploads(batch)
    assert len(uploads) == 3
    run.request("DELETE", f"/roots/{state['root']}", key=state["key"])
    for peer in servers():
        run.request("GET", f"/roots/{state['root']}/captured-files", key=state["key"], server=peer, statuses=(404,))
    assert not run.sql("SELECT id FROM file_extractions WHERE id=%s", (batch["extraction_id"],))
    assert not run.sql("SELECT id FROM file_work WHERE extraction_id=%s", (batch["extraction_id"],))
    retained, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (batch["id"],))
    assert retained["input_ref"] == batch["input_ref"]
    assert run.provider_uploads(retained) == uploads
    recovery.release()

    def cleaned():
        current, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (batch["id"],))
        return current if current["status"] == "failed" and current["cleanup_complete"] else None

    current = run.eventually("abandoned provider job terminality and S3 upload cleanup", cleaned, 900)
    assert current["provider_job_id"] == event["provider_job_id"]
    with recovery.client() as provider:
        remote = provider.batches.get(name=current["provider_job_id"])
        assert remote.state.name in {"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED"}
    run.provider_cleanup()
    run.wait_queue_empty("transform")
    assert not run.sql("SELECT id FROM file_work WHERE extraction_id=%s", (batch["extraction_id"],))
    for kind in ("sources", "extractions", "mutations"):
        assert not run.s3.list_objects_v2(Bucket=run.BUCKET, Prefix=f"{kind}/{state['org']}/{state['root']}/").get("Contents")
    print("Root deletion during accepted submission preserved one batch row and S3 upload identities; the same job reached terminal state, uploads cleaned, no index work resurrected.", flush=True)


if __name__ == "__main__":
    started, status = time.monotonic(), "failed"
    try:
        verify()
        status = "passed"
    finally:
        state = json.loads(run.STATE.read_text()) if run.STATE.exists() else {}
        with run.REPORT.open("a") as output:
            output.write(json.dumps({"run_id": state.get("nonce"), "phase": "provider-root-deletion",
                                    "status": status, "seconds": round(time.monotonic() - started, 2)}) + "\n")
        print(f"provider-root-deletion: {status}", flush=True)
