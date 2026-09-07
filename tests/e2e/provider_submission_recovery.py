"""Bounded submission discovery across a collector process restart."""

import hashlib
import json
import sys
import time

import provider_recovery as recovery
import run


def capture():
    state = run.provision()
    event = recovery.capture(state, "e2e-provider-discovery",
        ["Orchid observatory durable cursor one.", "Orchid observatory durable cursor two."], "hold_request")
    assert not event.get("provider_job_id")
    batch, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (event["batch_id"],))
    assert batch["submission_started_at"] and batch["provider_job_id"] is None
    state.update(discovery_event=event, discovery_inputs=run.provider_records(batch))
    run.save(state)


def scanned():
    state = json.loads(run.STATE.read_text())
    batch_id = state["discovery_event"]["batch_id"]

    def checkpoint():
        listings = recovery.relay("GET", "/status")["listings"]
        rows = run.sql("SELECT calls FROM pg_stat_statements WHERE query LIKE %s",
                       ("UPDATE provider_batches SET reconciliation_cursor=%",))
        batch, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (batch_id,))
        if not listings or not rows or batch["lease_token"] is not None:
            return None
        assert batch["status"] == "preparing" and batch["submission_started_at"]
        assert batch["provider_job_id"] is None, "negative discovery invented a provider job"
        assert all(item["page_size"] == "100" and item["count"] <= 100 for item in listings)
        cursor_hash = hashlib.sha256(batch["reconciliation_cursor"].encode()).hexdigest()
        assert cursor_hash == listings[-1]["next_cursor_hash"]
        return {"cursor_hash": cursor_hash, "listings": len(listings)}

    state["discovery_checkpoint"] = run.eventually("bounded negative listing and durable cursor", checkpoint, 600)
    assert len(recovery.relay("GET", "/status")["events"]) == 1, "negative listing replayed a paid create"
    run.save(state)
    print("One bounded provider page checked; its cursor is committed while the submission remains ambiguous. No paid create was replayed.", flush=True)


def release():
    state = json.loads(run.STATE.read_text())
    recovery.release()
    event = recovery.held(state["fault_id"], "response_released")
    assert event["upstream_status"] == 200 and event["provider_job_id"]
    state["discovery_event"] = event
    run.save(state)


def verify():
    state = json.loads(run.STATE.read_text())
    recovery.verify_pages(state)
    event = state["discovery_event"]
    batch, = run.sql("SELECT * FROM provider_batches WHERE id=%s", (event["batch_id"],))
    assert batch["status"] == "complete" and batch["provider_job_id"] == event["provider_job_id"]
    assert batch["reconciliation_cursor"] == ""
    assert [r["input_file_id"] for r in run.provider_records(batch)] == [r["input_file_id"] for r in state["discovery_inputs"]]
    traffic = recovery.relay("GET", "/status")
    assert len(traffic["events"]) == 1
    assert all(item["page_size"] == "100" and item["count"] <= 100 for item in traffic["listings"])
    before = state["discovery_checkpoint"]
    assert len(traffic["listings"]) > before["listings"]
    assert traffic["listings"][before["listings"]]["request_cursor_hash"] == before["cursor_hash"]
    run.provider_cleanup()
    print("Restarted discovery continued the committed cursor, found the original delayed job, and indexed both pages without reupload or duplicate inference.", flush=True)


if __name__ == "__main__":
    phase = sys.argv[1]
    started, status = time.monotonic(), "failed"
    try:
        {"capture": capture, "scanned": scanned, "release": release, "verify": verify}[phase]()
        status = "passed"
    finally:
        state = json.loads(run.STATE.read_text()) if run.STATE.exists() else {}
        with run.REPORT.open("a") as output:
            output.write(json.dumps({"run_id": state.get("nonce"), "phase": "provider-discovery-" + phase,
                                    "status": status, "seconds": round(time.monotonic() - started, 2)}) + "\n")
        print(f"provider-discovery-{phase}: {status}", flush=True)
