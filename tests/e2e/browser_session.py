"""Synthetic signed cookie inputs, real API authorization on two servers.

This tests JWT validation/current membership, not email/OAuth token issuance.
No production auth modules are imported and no DB state is written by the test.
"""

import base64
import hashlib
import hmac
import json
import os
import time

from api_access import servers
import run


def session(user, org, *, role="owner", expires=None):
    def encode(value):
        return base64.urlsafe_b64encode(json.dumps(value, separators=(",", ":")).encode()).rstrip(b"=")
    body = encode({"alg": "HS256", "typ": "JWT"}) + b"." + encode({"user_id": user,
        "org_id": org, "role": role, "email": "previous@example.invalid", "iss": "pufferfs",
        "iat": int(time.time()), "exp": expires if expires is not None else int(time.time()) + 3600})
    signature = base64.urlsafe_b64encode(hmac.digest(os.environ["JWT_SECRET"].encode(), body, hashlib.sha256)).rstrip(b"=")
    return (body + b"." + signature).decode()


def verify():
    state = json.loads(run.STATE.read_text())
    peers = servers()
    org, user = state["org"], state["users"][1]
    membership = f"/admin/orgs/{org}/members/{user}"
    run.request("PUT", membership, {"role": "owner"}, server=peers[0])
    cookie = session(user, org)
    try:
        for role, visible in (("owner", set(state["roots"])), ("viewer", {state["root"]}),
                              ("admin", set(state["roots"]))):
            run.request("PUT", membership, {"role": role}, server=peers[0])
            for peer in peers:
                before = session_calls()
                result = run.request("GET", "/roots", cookie=cookie, server=peer)
                assert session_calls() - before == 1
                assert {row["id"] for row in result} == visible
                identity = run.request("GET", "/auth/me", cookie=cookie, server=peer)
                assert identity["role"] == role and identity["user"]["id"] == user
                read = run.request("POST", f"/roots/{state['root']}/read", {
                    "path": "observations.txt", "lines": {"start": 1, "end": 1}}, cookie=cookie, server=peer)
                assert read["lines"][0]["content"] == state["access_text"].rstrip("\n")
                if role == "viewer":
                    run.request("POST", "/org/members", {"user_id": state["users"][0], "role": "viewer"},
                        cookie=cookie, server=peer, statuses=(403,))
        run.request("PUT", membership, {"role": "viewer"}, server=peers[0])
        run.request("DELETE", f"/org/members/{user}", key=state["key"], server=peers[0])
        for peer in peers:
            run.request("GET", "/roots", cookie=cookie, server=peer, statuses=(401,))
        run.request("PUT", membership, {"role": "viewer"}, server=peers[0])
        for peer in peers:
            for invalid in (session(user, org, expires=int(time.time())-60),
                            session(user, "unrelated-org"), session("missing-user", org),
                            cookie.rsplit(".", 1)[0] + ".invalid"):
                run.request("GET", "/roots", cookie=invalid, server=peer, statuses=(401,))
        state["browser_cookie"] = cookie
        run.save(state)
    finally:
        run.request("PUT", membership, {"role": "viewer"}, server=peers[0])
    print("One membership read per signed cookie; role changes/removal and invalid credentials agree across two API servers.", flush=True)


def session_calls():
    return run.sql("SELECT COALESCE(sum(calls),0)::bigint AS n FROM pg_stat_statements WHERE query LIKE %s",
        ("SELECT om.role,u.email FROM org_members om%",))[0]["n"]


def restarted():
    state = json.loads(run.STATE.read_text())
    for peer in servers():
        identity = run.request("GET", "/auth/me", cookie=state["browser_cookie"], server=peer)
        assert identity["role"] == "viewer"
        roots = run.request("GET", "/roots", cookie=state["browser_cookie"], server=peer)
        assert {root["id"] for root in roots} == {state["root"]}
    print("Server restarts did not restore the cookie's obsolete owner privileges.", flush=True)


if __name__ == "__main__":
    import sys
    {"verify": verify, "restarted": restarted}[sys.argv[1]]()
