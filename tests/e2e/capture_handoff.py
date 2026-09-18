"""Atomic capture scheduling and concurrent replay through two real API processes."""
import json
import time
import sys
import uuid
from capture_batches import packed, durable, published
from api_access import servers
from api_groups import parallel
import run


def fixture(state, count):
    root = run.new_root(state, "Atomic capture scheduling", "/state/scheduling", True)
    payloads = [f"Orchid calibration measurement {i}.\n".encode() for i in range(count)]
    sources = packed(state, root, payloads, parts=1)
    body = {"capture_id":str(uuid.uuid4()), "files":[{"path":f"record-{i:03}.txt", "source":source} for i,source in enumerate(sources)]}
    case = {"root":root,"body":body,"expected":{f["path"]:d.decode() for f,d in zip(body["files"],payloads)}}
    state["cases"].append(case)
    run.save(state)
    return case


def register(state, case, peer):
    return run.request("POST",f"/roots/{case['root']}/versions",case["body"],key=state["key"],server=peer,statuses=(202,))


def capture():
    state = run.provision()
    state["cases"] = []
    peers = servers()
    for count in (128,25,12):
        case = fixture(state,count)
        started = time.monotonic()
        case["response"] = register(state,case,peers[0])
        print(json.dumps({"capture_files":count,"accept_seconds":time.monotonic()-started}),flush=True)
        durable(case["root"],case["body"],case["response"])
        rows = run.work_rows(case["root"])
        assert len(rows)==count and all(r["status"]=="pending" and r["attempt_count"]==0 for r in rows)
        for peer in peers:
            assert register(state,case,peer)==case["response"]
        assert run.work_rows(case["root"])==rows
    case=fixture(state,1)
    results=parallel(peers,8,lambda peer,_:register(state,case,peer))
    assert all(result==results[0] for result in results)
    case["response"]=results[0]
    assert len(run.work_rows(case["root"]))==1
    run.save(state)


def recovered():
    state=json.loads(run.STATE.read_text())
    for case in state["cases"]:
        for peer in servers():
            assert register(state,case,peer)==case["response"]
        assert len(run.work_rows(case["root"]))==len(case["body"]["files"])
    print("Capture registration and work survived both API restarts; retries created no duplicate jobs.",flush=True)


def verify():
    state=json.loads(run.STATE.read_text())
    for case in state["cases"]:
        published(state,case["root"],case["expected"])
        rows=run.work_rows(case["root"],"index")
        assert len(rows)==len(case["body"]["files"]) and all(r["attempt_count"]==1 for r in rows)
    print("Multiple real worker processes claimed each publication exactly once; source/read/search matched.",flush=True)


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
