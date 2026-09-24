"""Real native embedding calls share request/token admission across roles."""
import json
from pathlib import Path
import sys
import uuid

from api_access import servers
from index_recovery import relay, held
import run


def reservations():
    return run.sql("SELECT org_id,tokens FROM provider_reservations WHERE expires_at>clock_timestamp() ORDER BY id")


def events():
    return [event for event in relay("GET", "/status")["events"] if event["operation"] == "write"]


def query(tenant, server=None, statuses=(200,), mode="vector"):
    return run.request("POST", "/query", {"root_id": tenant["root"], "query": "orchid calibration", "mode": mode},
        key=tenant["key"], server=server, statuses=statuses)


def prepare():
    state = run.provision()
    tenants = [{"org": state["org"], "key": state["key"]}]
    state["capacity_tenants"] = tenants
    org = run.request("POST", "/admin/orgs", {"name": "Embedding capacity competitor", "slug": "e2e-" + uuid.uuid4().hex})["id"]
    state["other_orgs"] = [org]
    run.save(state)
    user = state["users"][0]
    run.request("PUT", f"/admin/orgs/{org}/members/{user}", {"role": "owner"})
    key = run.request("POST", f"/admin/orgs/{org}/users/{user}/api-keys",
        {"name": "capacity", "scopes": ["query", "sync", "root:delete"]}, statuses=(201,))["key"]
    tenants.append({"org": org, "key": key})
    for index, tenant in enumerate(tenants):
        directory = Path(f"/state/capacity-{index}")
        directory.mkdir()
        line = "Orchid calibration " + "spectrometer calibration and wavelength. " * 145 + "\n"
        assert 5700 < len(line.encode()) < 6000
        text = line * (15 if index == 0 else 1)
        (directory / "record.txt").write_text(text)
        root = run.request("POST", "/roots", {"name": directory.name, "source_path": str(directory),
            "scope": "user", "vector_disabled": False}, key=tenant["key"], statuses=(201,))["id"]
        tenant.update(root=root, directory=str(directory), text=text)
        if index == 0:
            state["roots"].append(root)
            state["root"] = root
        run.save(state)
        run.cli(tenant, "sync", str(directory), "--id", root)
    names = [row["namespace"] for row in run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=ANY(%s)", ([t["root"] for t in tenants],))]
    state["capacity_fault"] = relay("POST", "/fault", {"namespaces": names, "mode": "hold_response", "count": 64})["fault_id"]
    run.save(state)


def held_budget():
    state = json.loads(run.STATE.read_text())
    event = held(state["capacity_fault"], "response_held")
    assert event["upstream_status"] == 200
    # Depending on claim order, the small file fits first and the other worker
    # can reserve a smaller batch, or the full batch leaves too little room for
    # the small file. Both schedules must respect the same token window.
    def occupied():
        active = reservations()
        calls = events()
        return (sum(row["tokens"] for row in active) > 20000
            and all(e["state"] == "response_held" for e in calls)
            and (len(calls) == 2 or run.sql("SELECT id FROM file_work WHERE stage='index' AND status='pending' AND attempt_count=0 AND next_attempt_at>NOW()")))
    run.eventually("shared token window occupied across competing workers", occupied, 30)
    active = reservations()
    assert len(active) in (1, 2) and 20000 < sum(row["tokens"] for row in active) <= 32768
    before = len(active)
    result = run.request("POST", "/query", {"root_id": state["capacity_tenants"][0]["root"],
        "query": "orchid " * 2000, "mode": "vector"}, key=state["key"], server=servers()[1], statuses=(429,))
    assert result["code"] == "embedding_capacity_busy" and len(reservations()) == before
    state["held_namespace"] = event["namespace"]
    run.save(state)
    print("Two background replicas respected one token window; pending work retained its attempts.", flush=True)


def release():
    relay("POST", "/release")


def verify():
    state = json.loads(run.STATE.read_text())
    tenants = state["capacity_tenants"]
    for tenant in tenants:
        file = run.wait_indexed(tenant, tenant["root"])["record.txt"]
        run.assert_source_retained(file)
        result = run.request("POST", f"/roots/{tenant['root']}/read", {"path": "record.txt", "lines": {"start": 1, "end": 100}}, key=tenant["key"])
        assert [line["content"] for line in result["lines"]] == tenant["text"].splitlines()
    accepted = [e for e in events() if e.get("upstream_status") == 200]
    assert len({e["namespace"] for e in accepted}) == 2
    assert sum(e["upsert_count"] for e in accepted) == 16
    assert max(e["upsert_count"] for e in accepted) <= 5
    assert all(type(e["response_metadata"]["performance"].get("embedding_tokens")) is int for e in accepted)
    # Fill the remaining shared request window through alternating API
    # replicas. Every vector query spends the same budget as worker writes.
    peers = servers()
    for i in range(20):
        status, result = run.request("POST", "/query", {"root_id": tenants[0]["root"], "query": "orchid calibration", "mode": "vector"},
            key=tenants[0]["key"], server=peers[i % 2], statuses=(200, 429), with_status=True)
        if status == 429:
            assert result["code"] == "embedding_capacity_busy"
            break
        assert result["results"]
    else:
        raise AssertionError("request budget was not enforced")
    assert len(reservations()) == 12
    assert query(tenants[1], peers[1], statuses=(429,))["code"] == "embedding_capacity_busy"
    assert query(tenants[1], peers[0], mode="fts")["results"]
    print("Token-aware batches and both API replicas shared the same 12-request window; FTS remained available.", flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    # Restart cannot reset persisted capacity. No database timestamp edits.
    assert len(reservations()) == 12
    for peer in servers():
        assert query(state["capacity_tenants"][0], peer, statuses=(429,))["code"] == "embedding_capacity_busy"
    run.eventually("normal provider reservation expiry", lambda: not reservations(), 80)
    print("Shared request debits survived API/worker restart and expired naturally.", flush=True)


def throttled():
    state = json.loads(run.STATE.read_text())
    tenant = state["capacity_tenants"][0]
    names = [r["namespace"] for r in run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=%s", (tenant["root"],))]
    fault = relay("POST", "/fault", {"namespaces": names, "mode": "reject_429", "count": 6})["fault_id"]
    text = "Orchid successful throttling recovery.\n"
    (Path(tenant["directory"]) / "record.txt").write_text(text)
    run.cli(tenant, "sync", tenant["directory"], "--id", tenant["root"])
    def first_rejection():
        return [e for e in events() if e["fault_id"] == fault and e["state"] == "rate_limited"]
    run.eventually("external 429 response", first_rejection, 30)
    before = len(reservations())
    assert query(tenant, servers()[1], statuses=(429,))["code"] == "embedding_capacity_busy"
    assert len(reservations()) == before, "cooldown allowed a query to reach provider"
    file = run.wait_indexed(tenant, tenant["root"])["record.txt"]
    assert len(first_rejection()) == 6
    work = run.sql("SELECT w.attempt_count FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id WHERE e.version_id=%s", (file["version_id"],))
    assert work == [{"attempt_count": 1}], work
    result = run.request("POST", f"/roots/{tenant['root']}/read", {"path": "record.txt", "lines": {"start": 1, "end": 1}}, key=tenant["key"])
    assert result["lines"][0]["content"] == text.rstrip("\n")
    relay("POST", "/release")
    print("Six external 429s shared their cooldown with queries and recovered without exhausting five work attempts.", flush=True)


if __name__ == "__main__":
    {"prepare": prepare, "held": held_budget, "release": release, "verify": verify, "restarted": restarted, "throttled": throttled}[sys.argv[1]]()
