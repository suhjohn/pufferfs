"""API-key workflows, including a real HTTP body paused after authentication."""

from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
import hashlib
import http.client
import json
import os
import threading
from urllib.parse import urlsplit
import uuid

from api_access import servers
from browser_session import session
import run


def counts():
    patterns = ("INSERT INTO api_keys%SELECT%FROM org_members member%",
                "SELECT ak.id, ak.org_id%", "SELECT u.id, u.email, u.name, u.avatar_url, om.role, om.joined_at%")
    return [run.sql("SELECT COALESCE(sum(calls),0)::bigint AS n FROM pg_stat_statements WHERE query LIKE %s", (p,))[0]["n"] for p in patterns]


@contextmanager
def held_body(peer, path, body, *, key=None, cookie=None):
    data = json.dumps(body).encode()
    conn = http.client.HTTPConnection(urlsplit(peer).netloc, timeout=30)
    try:
        conn.putrequest("POST", path)
        conn.putheader("Content-Type", "application/json")
        conn.putheader("Content-Length", str(len(data)))
        conn.putheader("Expect", "100-continue")
        if cookie is not None:
            conn.putheader("Cookie", "pf_session=" + cookie)
        else:
            conn.putheader("Authorization", "Bearer " + key)
        conn.endheaders()
        # Go emits 100 Continue when the handler reads the request body, after
        # middleware authentication. No DB locks, process hooks or fake response.
        with conn.sock.makefile("rb", buffering=0) as stream:
            assert stream.readline().split()[1] == b"100", "handler did not reach its body read"
            while True:
                line = stream.readline()
                assert line, "connection closed before 100 Continue headers ended"
                if line == b"\r\n":
                    break
        def finish(expected):
            conn.send(data)
            with conn.getresponse() as response:
                payload = json.loads(response.read())
                assert response.status == expected, f"held request returned HTTP {response.status}"
                return payload
        yield finish
    finally:
        conn.close()


def verify():
    state = json.loads(run.STATE.read_text())
    peers = servers()
    for redundant, unique in (("idx_api_keys_hash", "api_keys_key_hash_key"),
                              ("idx_root_index_namespaces_root", "root_index_namespaces_root_id_shard_index_key")):
        observed = run.sql("SELECT to_regclass(%s) IS NULL AS removed,EXISTS(SELECT 1 FROM pg_index WHERE indexrelid=to_regclass(%s) AND indisunique AND indisvalid) AS retained", (redundant, unique))[0]
        assert observed == {"removed": True, "retained": True}
    org, user = state["org"], state["users"][1]
    admin_path = f"/admin/orgs/{org}/users/{user}/api-keys"
    cookie = session(user, org, role="viewer")
    def provision(name, scopes, peer=peers[1]):
        before = counts()
        response = run.request("POST", admin_path, {"name": name, "scopes": scopes}, server=peer, statuses=(201,))
        assert [b-a for a,b in zip(before, counts())] == [1, 0, 0], "admin key creation reloaded membership"
        return response["key"]
    def key_id(raw):
        rows = run.sql("SELECT id FROM api_keys WHERE key_hash=%s", (hashlib.sha256(raw.encode()).hexdigest(),))
        assert len(rows) == 1
        return rows[0]["id"]
    def absent(name):
        assert not run.sql("SELECT id FROM api_keys WHERE org_id=%s AND user_id=%s AND name=%s", (org, user, name)), "denied creation persisted a key"

    for scope in ("api_keys:write", "admin", "write", "*"):
        parent = provision("parent-" + scope, [scope])
        name = "child-" + scope
        before = counts()
        child = run.request("POST", "/auth/api-keys", {"name": name, "scopes": [" query ", "query", " "]},
            key=parent, server=peers[0], statuses=(201,))["key"]
        assert [b-a for a,b in zip(before, counts())] == [1, 1, 0]
        child_id = key_id(child)
        for peer in peers:
            assert run.request("GET", "/auth/me", key=child, server=peer)["scopes"] == ["query"]
            listed = run.request("GET", "/auth/api-keys", key=parent, server=peer)
            assert all(set(row) <= {"id", "name", "scopes", "created_at", "expires_at"} for row in listed)
            assert any(row["id"] == child_id and row["scopes"] == ["query"] for row in listed)
        # Revocation is visible on the other server and repeated deletion is harmless.
        run.request("DELETE", "/auth/api-keys/" + child_id, key=parent, server=peers[0])
        run.request("GET", "/auth/me", key=child, server=peers[1], statuses=(401,))
        run.request("DELETE", "/auth/api-keys/missing-key", key=parent, server=peers[1])
    query_only = provision("query-only", ["query"])
    for peer in peers:
        for method, path, body in (("POST", "/auth/api-keys", {"scopes": ["query"]}),
                                  ("GET", "/auth/api-keys", None), ("DELETE", "/auth/api-keys/missing-key", None)):
            run.request(method, path, body, key=query_only, server=peer, statuses=(403,))
        for scopes in ([], [" "], ["not-a-scope"]):
            run.request("POST", "/auth/api-keys", {"scopes": scopes}, cookie=cookie, server=peer, statuses=(400,))
        run.request("POST", f"/admin/orgs/{org}/users/missing-user/api-keys", {"scopes": ["query"]}, server=peer, statuses=(404,))
        run.request("POST", f"/admin/orgs/missing-org/users/{user}/api-keys", {"scopes": ["query"]}, server=peer, statuses=(404,))

    empty = run.request("POST", "/admin/users", {"email": "empty-keys-" + uuid.uuid4().hex + "@example.invalid", "name": "Empty keys"})["id"]
    state["users"].append(empty)
    run.save(state)
    empty_path = f"/admin/orgs/{org}/users/{empty}/api-keys"
    run.request("POST", empty_path, {"scopes": ["query"]}, statuses=(404,))
    run.request("PUT", f"/admin/orgs/{org}/members/{empty}", {"role": "viewer"})
    empty_cookie = session(empty, org, role="viewer")
    for peer in peers:
        assert run.request("GET", "/auth/api-keys", cookie=empty_cookie, server=peer) is None
    only = run.request("POST", "/auth/api-keys", {"scopes": ["query"]}, cookie=empty_cookie, statuses=(201,))["key"]
    run.request("DELETE", "/auth/api-keys/" + key_id(only), cookie=empty_cookie, server=peers[0])
    assert run.request("GET", "/auth/api-keys", cookie=empty_cookie, server=peers[1]) is None
    parent = run.request("POST", empty_path, {"scopes": ["api_keys:write"]}, statuses=(201,))["key"]
    barrier = threading.Barrier(9)
    def delete_race(index):
        barrier.wait(timeout=20)
        if index == 8:
            return run.request("DELETE", f"/admin/users/{empty}", server=peers[1])
        return run.request("POST", "/auth/api-keys", {"scopes": ["query"]}, key=parent,
            server=peers[index % 2], statuses=(201, 401, 403))
    with ThreadPoolExecutor(max_workers=9) as pool:
        list(pool.map(delete_race, range(9)))
    assert not run.sql("SELECT 1 FROM api_keys WHERE user_id=%s UNION ALL SELECT 1 FROM org_members WHERE user_id=%s", (empty, empty))
    print("Empty key collections preserved their response; eight concurrent creations plus user deletion left no keys or memberships.", flush=True)

    # A complete, delayed body succeeds while authorization remains valid.
    with held_body(peers[0], "/auth/api-keys", {"name": "held-valid", "scopes": ["query"]}, cookie=cookie) as finish:
        active = finish(201)["key"]
    for peer in peers:
        assert run.request("GET", "/auth/me", key=active, server=peer)["user"]["id"] == user
    parent = provision("held-revoked-parent", ["api_keys:write", "query"])
    parent_id = key_id(parent)
    with held_body(peers[0], "/auth/api-keys", {"name": "after-key-revocation", "scopes": ["query"]}, key=parent) as finish:
        run.request("DELETE", "/auth/api-keys/" + parent_id, key=state["key"], server=peers[1])
        finish(403)
    absent("after-key-revocation")
    # API keys and signed sessions both recheck membership at insertion.
    for credential in ({"key": query_only}, {"cookie": cookie}):
        # The API-key variant needs creation scope before the body is accepted.
        if "key" in credential:
            credential = {"key": provision("held-removed-member", ["api_keys:write"])}
        try:
            with held_body(peers[0], "/auth/api-keys", {"name": "after-member-removal", "scopes": ["query"]}, **credential) as finish:
                run.request("DELETE", f"/org/members/{user}", key=state["key"], server=peers[1])
                finish(403)
            absent("after-member-removal")
        finally:
            run.request("PUT", f"/admin/orgs/{org}/members/{user}", {"role": "viewer"})

    try:
        with held_body(peers[0], admin_path, {"name": "after-admin-member-removal", "scopes": ["query"]},
                       key=os.environ["PUFFERFS_ADMIN_KEY"]) as finish:
            run.request("DELETE", f"/org/members/{user}", key=state["key"], server=peers[1])
            finish(404)
        absent("after-admin-member-removal")
    finally:
        run.request("PUT", f"/admin/orgs/{org}/members/{user}", {"role": "viewer"})

    # The caller's organization bounds key deletion; cross-org IDs do not revoke.
    other = run.request("POST", "/admin/orgs", {"name": "Key isolation", "slug": "keys-" + uuid.uuid4().hex})["id"]
    state.setdefault("other_orgs", []).append(other)
    run.save(state)
    run.request("PUT", f"/admin/orgs/{other}/members/{user}", {"role": "viewer"})
    foreign = run.request("POST", f"/admin/orgs/{other}/users/{user}/api-keys", {"scopes": ["query"]}, statuses=(201,))["key"]
    run.request("DELETE", "/auth/api-keys/" + key_id(foreign), key=state["key"], server=peers[0])
    assert run.request("GET", "/auth/me", key=foreign, server=peers[1])["org_id"] == other
    state.update(key_active=active, key_revoked=parent)
    run.save(state)
    print("One-statement key creation passed scope/hash/metadata checks, cross-server revocation, and deterministic credential/membership changes during HTTP body arrival.", flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    for peer in servers():
        assert run.request("GET", "/auth/me", key=state["key_active"], server=peer)["user"]["id"] == state["users"][1]
        run.request("GET", "/auth/me", key=state["key_revoked"], server=peer, statuses=(401,))
    print("Active and revoked API-key state survived both API restarts.", flush=True)


if __name__ == "__main__":
    import sys
    {"verify": verify, "restarted": restarted}[sys.argv[1]]()
