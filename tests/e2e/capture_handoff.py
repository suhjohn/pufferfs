"""Capture batches/retries through two APIs and a real SQS network boundary."""

import json
import sys
import time
import uuid

from api_access import servers
from api_groups import calls, parallel
from capture_batches import packed, durable, published
import run


def counts():
    return [calls(pattern) for pattern in (
        "SELECT w.id,r.org_id,f.root_id,f.id,v.id,e.id,w.stage%",
        "UPDATE file_work SET enqueued_at=NOW() WHERE id=ANY(%")]


def fault(allowed):
    run.request("POST", "/fault", {"allow_send_requests": allowed}, server="http://capture-sqs:8080")


def events():
    return run.request("GET", "/events", server="http://capture-sqs:8080")


def register(state, case, peer, acknowledgments, *, body=None):
    before, started = counts(), time.monotonic()
    body = body or case["body"]
    result = run.request("POST", f"/roots/{case['root']}/versions", body,
                         key=state["key"], server=peer, statuses=(202,))
    delta = [after - prior for prior, after in zip(before, counts())]
    assert delta == [0, acknowledgments], f"capture handoff SQL: {delta}"
    durable(case["root"], body, result)
    print(json.dumps({"event": "capture_handoff", "files": len(body["files"]),
        "delivery_selects": delta[0], "acknowledgment_statements": delta[1],
        "seconds": round(time.monotonic() - started, 3)}), flush=True)
    return result


def fixture(state, count):
    root = run.new_root(state, "Capture queue handoff", "/state/capture-handoff/" + str(len(state["cases"])), True)
    payloads = [f"Orchid calibration queue measurement {i}.\n".encode() for i in range(count)]
    sources = packed(state, root, payloads, parts=1)
    body = {"capture_id": str(uuid.uuid4()), "files": [
        {"path": f"record-{i:03}.txt", "source": source} for i, source in enumerate(sources)]}
    case = {"root": root, "body": body,
            "expected": {file["path"]: data.decode() for file, data in zip(body["files"], payloads)}}
    state["cases"].append(case)
    run.save(state)
    return case


def capture():
    state = run.provision()
    state["cases"] = []
    peers = servers()
    largest = fixture(state, 128)
    fault(None)
    before = len(events())
    largest["response"] = register(state, largest, peers[0], 1)
    sent = events()[before:]
    assert len(sent) == 13 and sum(len(item["work_ids"]) for item in sent) == 128
    assert all(item["state"] == "forwarded" and item["status"] == 200 for item in sent)
    for peer in peers:
        assert register(state, largest, peer, 0) == largest["response"]
    assert len(events()) == before + 13, "acknowledged replay sent more SQS requests"

    partial = fixture(state, 25)
    fault(1)
    partial["response"] = register(state, partial, peers[0], 1)
    rows = run.work_rows(partial["root"])
    confirmed = {row["id"]: row["enqueued_at"] for row in rows if row["enqueued_at"] is not None}
    missing = {row["id"] for row in rows if row["enqueued_at"] is None}
    assert len(confirmed) == 10 and len(missing) == 15
    assert all(row["status"] == "pending" and row["attempt_count"] == 0 for row in rows)
    fault(None)
    before = len(events())
    assert register(state, partial, peers[1], 1) == partial["response"]
    repaired = events()[before:]
    assert len(repaired) == 2 and {item for event in repaired for item in event["work_ids"]} == missing
    assert all(row["enqueued_at"] is not None for row in run.work_rows(partial["root"]))
    assert {row["id"]: row["enqueued_at"] for row in run.work_rows(partial["root"]) if row["id"] in confirmed} == confirmed

    # Mixed-stage handoff still uses one acknowledgment. No execution roles
    # have started: these newer versions supersede durable queued captures.
    changed, deleted = largest["body"]["files"][:2]
    data = b"New calibration version before workers start.\n"
    source, = packed(state, largest["root"], [data], parts=1)
    update = {"capture_id": str(uuid.uuid4()), "files": [
        {"path": changed["path"], "source": source, "previous_version_id": largest["response"]["versions"][0]["version_id"]},
        {"path": deleted["path"], "deleted": True, "previous_version_id": largest["response"]["versions"][1]["version_id"]}]}
    register(state, largest, peers[1], 1, body=update)
    largest["expected"].update({changed["path"]: data.decode(), deleted["path"]: None})
    assert register(state, largest, peers[0], 0) == largest["response"]

    race = fixture(state, 1)
    state["concurrent_root"] = race["root"]
    results = parallel(peers, 8, lambda peer, _: run.request("POST", f"/roots/{race['root']}/versions",
        race["body"], key=state["key"], server=peer, statuses=(202,)))
    assert all(result == results[0] for result in results)
    race["response"] = results[0]
    assert len(run.work_rows(race["root"])) == 1
    durable(race["root"], race["body"], results[0])

    outage = fixture(state, 12)
    fault(0)
    outage["response"] = register(state, outage, peers[1], 0)
    assert all(row["enqueued_at"] is None for row in run.work_rows(outage["root"]))
    run.save(state)
    print("128-file capture used 13 real SQS batches and one acknowledgment; partial failure preserved the first ten acknowledgments, the other API retried only 15 missing IDs, and concurrent/historical/mixed-stage captures remained valid.", flush=True)


def recovered():
    state = json.loads(run.STATE.read_text())
    outage = state["cases"][-1]
    def ready():
        rows = run.work_rows(outage["root"])
        return rows if rows and all(row["enqueued_at"] is not None for row in rows) else False
    run.eventually("scheduled repair after API restart with API SQS still disconnected", ready, 180)
    for stage in ("transform", "index"):
        work = [row for case in state["cases"] for row in run.work_rows(case["root"], stage)]
        assert all(row["status"] == "pending" and row["attempt_count"] == 0 for row in work)
        run.inspect_sqs_deliveries(work, stage)
    fault(None)
    before = len(events())
    assert register(state, outage, servers()[0], 0) == outage["response"]
    assert len(events()) == before
    print("Scheduled reconciliation repaired all disconnected captures after both APIs restarted; exact SQS references were present and API retry did not resend them.", flush=True)


def verify():
    state = json.loads(run.STATE.read_text())
    for case in state["cases"]:
        published(state, case["root"], case["expected"])
    assert all(row["attempt_count"] == 1 for stage in ("transform", "index")
               for row in run.work_rows(state["concurrent_root"], stage))
    assert calls("SELECT status FROM file_work WHERE id=%") == 0, "consumer reread status after a durable worker response"
    print("Both APIs expose exact source bytes, reads, FTS and tombstones after full production processing; concurrent capture created one execution per stage.", flush=True)


if __name__ == "__main__":
    phase, status, started = sys.argv[1], "failed", time.monotonic()
    try:
        {"capture": capture, "recovered": recovered, "verify": verify}[phase]()
        status = "passed"
    finally:
        state = json.loads(run.STATE.read_text())
        with run.REPORT.open("a") as output:
            output.write(json.dumps({"run_id": state["nonce"], "phase": "capture-handoff-" + phase,
                "status": status, "seconds": round(time.monotonic() - started, 2)}) + "\n")
