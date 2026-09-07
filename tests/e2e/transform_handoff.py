"""CLI capture -> direct transform handoff, plus network failure/reconciliation."""

import json
import os
from pathlib import Path
import sys
import time

from api_access import servers
from api_groups import calls
import run


def delivery_scans():
    return calls("SELECT w.id,w.stage,w.extraction_id,v.id AS version_id,v.file_id,f.root_id,r.org_id%")


def queue_fault(enabled):
    run.request("POST", "/proxies/queue-delivery", {"enabled": enabled},
                server="http://transform-sqs:8474")


def transformed(state, case, delivered):
    def ready():
        transform = run.work_rows(case["root"])
        index = run.work_rows(case["root"], "index")
        if len(index) != len(case["expected"]) or any(row["status"] != "complete" for row in transform):
            return False
        if any((row["enqueued_at"] is not None) != delivered for row in index):
            return False
        return transform, index
    transform, index = run.eventually("committed transformations and expected queue acknowledgments", ready, 180)
    assert len(transform) == len(index)
    assert all(row["attempt_count"] == 1 and row["chunks_ref"] for row in transform)
    assert all(row["status"] == "pending" and row["attempt_count"] == 0 for row in index)
    for file in run.catalog(state, case["root"]).values():
        run.assert_source_retained(file)
    return index


def capture():
    state = run.provision()
    servers()
    state["handoff_cases"] = []
    before = delivery_scans()
    for delivered in (True, False):
        queue_fault(delivered)
        directory = Path("/state/handoff-" + str(len(state["handoff_cases"])))
        directory.mkdir()
        expected = {f"record-{i:02}.txt": f"Orchid telemetry measurement {i}.\n" for i in range(12)}
        for name, text in expected.items():
            (directory / name).write_text(text)
        case = {"directory": str(directory), "expected": expected,
                "root": run.new_root(state, "Transformation queue handoff", directory, True)}
        state["handoff_cases"].append(case)
        run.save(state)
        run.cli(state, "sync", str(directory), "--id", case["root"], "--no-vector")
        work = transformed(state, case, delivered)
        case["index_ids"] = [row["id"] for row in work]
        case["chunks"] = {row["chunks_ref"]: run.s3.head_object(
            Bucket=run.BUCKET, Key=row["chunks_ref"])["ETag"] for row in work}
        if delivered:
            run.inspect_sqs_deliveries(work, "index")
        run.save(state)
    # Wait for the actual transform consumers to acknowledge all executions,
    # including the failed SQS-send attempts after extraction committed.
    def drained():
        attributes = run.sqs.get_queue_attributes(
            QueueUrl=os.environ["PUFFERFS_SQS_TRANSFORM_QUEUE_URL"],
            AttributeNames=["ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible"])["Attributes"]
        return all(int(value) == 0 for value in attributes.values())
    run.eventually("transformation execution to finish during its SQS outage", drained, 180)
    assert delivery_scans() == before, "transformation reread the delivery ledger"
    state["delivery_scans"] = before
    run.save(state)
    print("24 CLI-captured files transformed once with no delivery SELECT; 12 exact SQS messages succeeded and 12 committed handoffs survived a real network outage.", flush=True)


def recovered():
    state = json.loads(run.STATE.read_text())
    work = []
    for case in state["handoff_cases"]:
        rows = transformed(state, case, True)
        assert [row["id"] for row in rows] == case["index_ids"]
        work.extend(rows)
        for key, etag in case["chunks"].items():
            assert run.s3.head_object(Bucket=run.BUCKET, Key=key)["ETag"] == etag
    assert delivery_scans() > state["delivery_scans"], "reconciliation did not scan the durable handoff ledger"
    run.inspect_sqs_deliveries(work, "index")
    queue_fault(True)
    print("With transformation processes stopped, scheduled reconciliation delivered the original missing IDs in bounded batches without changing chunks or executing index work.", flush=True)


def verify(state):
    for case in state["handoff_cases"]:
        files = run.wait_indexed(state, case["root"])
        for peer in servers():
            for name, expected in case["expected"].items():
                read = run.request("POST", f"/roots/{case['root']}/read",
                    {"path": name, "lines": {"start": 1, "end": 10}}, key=state["key"], server=peer,
                    statuses=(404,) if expected is None else (200,))
                if expected is None:
                    assert files[name]["deleted"]
                else:
                    assert [row["content"] for row in read["lines"]] == expected.splitlines()
                    run.assert_source_retained(files[name])
            hits = run.request("POST", "/query", {"root_id": case["root"], "query": "telemetry",
                "mode": "fts", "top_k": 100}, key=state["key"], server=peer)["results"]
            assert {hit["file_path"] for hit in hits} == {name for name, text in case["expected"].items() if text}
            assert all(hit["content"] in case["expected"][hit["file_path"]] for hit in hits)


def updated():
    state = json.loads(run.STATE.read_text())
    verify(state)
    before = delivery_scans()
    for case in state["handoff_cases"]:
        directory = Path(case["directory"])
        name, removed = sorted(case["expected"])[:2]
        case["expected"][name] = "Revised orchid telemetry after process restart.\n"
        (directory / name).write_text(case["expected"][name])
        (directory / removed).unlink()
        case["expected"][removed] = None
        run.save(state)
        run.cli(state, "sync", str(directory), "--id", case["root"], "--no-vector")
    verify(state)
    assert delivery_scans() == before, "normal restarted handoff used a repair scan"
    print("Both APIs read/searched exact publications after recovery and process restarts; newer content and tombstones superseded old results without a delivery scan.", flush=True)


if __name__ == "__main__":
    phase, status, started = sys.argv[1], "failed", time.monotonic()
    try:
        {"capture": capture, "recovered": recovered, "updated": updated}[phase]()
        status = "passed"
    finally:
        state = json.loads(run.STATE.read_text())
        with run.REPORT.open("a") as output:
            output.write(json.dumps({"run_id": state["nonce"], "phase": "transform-handoff-" + phase,
                "status": status, "seconds": round(time.monotonic() - started, 2)}) + "\n")
