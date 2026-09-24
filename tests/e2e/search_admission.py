"""Shared search capacity through two API processes and real provider requests."""

from concurrent.futures import ThreadPoolExecutor
import http.client
import json
from pathlib import Path
import socket
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import urlsplit
import uuid

from api_access import servers
from api_reads import relay
import run


def search(tenant, peer, statuses=(200,)):
    return run.request("POST", "/query", {"root_ids": tenant["roots"], "query": "orchid", "mode": "fts"},
                       key=tenant["key"], server=peer, statuses=statuses)


def active_slots():
    return run.sql("SELECT org_id,sum(slots)::int AS slots FROM search_leases WHERE expires_at>clock_timestamp() GROUP BY org_id")


def hold(tenant, count):
    names = [r["namespace"] for r in run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=ANY(%s)", (tenant["roots"],))]
    return relay("POST", "/fault", {"operation": "query", "mode": "hold_response", "namespaces": names, "count": count})["fault_id"]


def held(fault, count):
    return run.eventually("real search responses held", lambda: len([e for e in relay("GET", "/status")["events"]
        if e["fault_id"] == fault and e["state"] == "response_held" and e.get("upstream_status") == 200]) == count, 60)


def assert_busy(tenant, peer):
    request = urllib.request.Request(peer + "/query",
        data=json.dumps({"root_ids": tenant["roots"], "query": "orchid", "mode": "fts"}).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + tenant["key"]})
    try:
        urllib.request.urlopen(request, timeout=10)
        raise AssertionError("search exceeded shared capacity")
    except urllib.error.HTTPError as error:
        assert error.code == 429 and error.headers["Retry-After"] == "1"
        assert json.load(error)["code"] == "search_capacity_busy"


def prepare():
    state = run.provision()
    tenants = [{"org": state["org"], "key": state["key"], "roots": []}]
    state["search_tenants"] = tenants
    for _ in range(2):
        org = run.request("POST", "/admin/orgs", {"name": "Search capacity E2E", "slug": "e2e-" + uuid.uuid4().hex})["id"]
        state.setdefault("other_orgs", []).append(org)
        run.save(state)
        user = state["users"][0]
        run.request("PUT", f"/admin/orgs/{org}/members/{user}", {"role": "owner"})
        key = run.request("POST", f"/admin/orgs/{org}/users/{user}/api-keys",
            {"name": "search-capacity", "scopes": ["query", "sync", "root:delete"]}, statuses=(201,))["key"]
        tenants.append({"org": org, "key": key, "roots": []})
        run.save(state)
    for index, tenant in enumerate(tenants):
        for slot in range(2 if index == 0 else 1):
            directory = Path(f"/state/search-{index}-{slot}")
            directory.mkdir()
            (directory / "record.txt").write_text("Orchid observatory measures atmospheric pressure.\n")
            root = run.request("POST", "/roots", {"name": directory.name, "source_path": str(directory),
                "scope": "user", "vector_disabled": True}, key=tenant["key"], statuses=(201,))["id"]
            tenant["roots"].append(root)
            if index == 0:
                state["roots"].append(root)
            run.save(state)
            run.cli(tenant, "sync", str(directory), "--id", root, "--no-vector")
            run.wait_indexed(tenant, root)
    run.save(state)
    print("Three isolated tenants captured and indexed through real CLI/workers.", flush=True)


def verify():
    state = json.loads(run.STATE.read_text())
    first, second, third = state["search_tenants"]
    peers = servers()
    # One two-namespace search occupies its tenant's entire allowance. The
    # remaining global slot belongs to another tenant, across the other API.
    names = [r["namespace"] for r in run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=ANY(%s)",
        (first["roots"] + second["roots"],))]
    fault = relay("POST", "/fault", {"operation": "query", "mode": "hold_response", "namespaces": names, "count": 3})["fault_id"]
    before = {e["id"] for e in relay("GET", "/status")["events"]}
    with ThreadPoolExecutor(max_workers=2) as pool:
        try:
            a = pool.submit(search, first, peers[0])
            held(fault, 2)
            assert active_slots() == [{"org_id": first["org"], "slots": 2}]
            assert_busy(first, peers[1])
            b = pool.submit(search, second, peers[1])
            held(fault, 3)
            assert sum(r["slots"] for r in active_slots()) == 3
            assert_busy(third, peers[0])
            # Rejected requests must not reach the provider.
            events = [e for e in relay("GET", "/status")["events"] if e["id"] not in before]
            assert len(events) == 3
            relay("POST", "/release")
            assert {r["root_id"] for r in a.result(timeout=30)["results"]} == set(first["roots"])
            assert b.result(timeout=30)["results"]
        finally:
            relay("POST", "/release")
    assert not active_slots()
    assert search(third, peers[0])["results"]
    print("Tenant and aggregate limits held across both API processes; released slots were immediately reusable.", flush=True)

    # Cancel a real client connection while both upstream responses are held.
    fault = hold(first, 2)
    parsed = urlsplit(peers[0])
    connection = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=15)
    try:
        connection.request("POST", "/query", json.dumps({"root_ids": first["roots"], "query": "orchid", "mode": "fts"}),
            {"Content-Type": "application/json", "Authorization": "Bearer " + first["key"]})
        held(fault, 2)
        connection.sock.shutdown(socket.SHUT_RDWR)
        connection.close()
        run.eventually("client cancellation to release shared capacity", lambda: not active_slots(), 10)
    finally:
        connection.close()
        relay("POST", "/release")
    assert search(first, peers[1])["results"]

    # A provider response held beyond the production 30s request deadline must
    # release capacity before the 45s crash lease expires.
    fault = hold(first, 2)
    with ThreadPoolExecutor(max_workers=1) as pool:
        try:
            started = time.monotonic()
            result = pool.submit(search, first, peers[0], (504,))
            held(fault, 2)
            assert "timed out" in result.result(timeout=40)["error"]
            assert 25 < time.monotonic() - started < 40
            assert not active_slots()
        finally:
            relay("POST", "/release")
    print("Client cancellation and the production request deadline both released reservations.", flush=True)


def crash_query():
    state = json.loads(run.STATE.read_text())
    tenant = state["search_tenants"][0]
    fault = hold(tenant, 2)
    state["crash_search_fault"] = fault
    run.save(state)
    try:
        search(tenant, servers()[0])
        raise AssertionError("API process was not killed with its request in flight")
    except (http.client.RemoteDisconnected, urllib.error.URLError, ConnectionError):
        print("In-flight caller lost the killed API process.", flush=True)


def crash_held():
    def state_ready():
        state = json.loads(run.STATE.read_text())
        return state.get("crash_search_fault")
    fault = run.eventually("crash query armed", state_ready, 30)
    held(fault, 2)
    assert sum(r["slots"] for r in active_slots()) == 2


def recovered():
    state = json.loads(run.STATE.read_text())
    tenant = state["search_tenants"][0]
    peers = servers()
    assert active_slots(), "crash reservation expired before the restart assertion"
    assert_busy(tenant, peers[1])
    relay("POST", "/release")
    run.eventually("crashed query reservation to expire normally", lambda: not active_slots(), 60)
    for peer in peers:
        assert {r["root_id"] for r in search(tenant, peer)["results"]} == set(tenant["roots"])
    print("API crash retained the shared limit until lease expiry; both restarted APIs recovered automatically.", flush=True)


if __name__ == "__main__":
    {"prepare": prepare, "verify": verify, "crash-query": crash_query, "crash-held": crash_held, "recovered": recovered}[sys.argv[1]]()
