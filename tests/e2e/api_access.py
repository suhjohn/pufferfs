"""Public API/CLI authorization across two production servers, with SQL counts."""

from functools import partial
import json
from pathlib import Path
import socket
import sys

import run


def servers():
    addresses = sorted({row[4][0] for row in socket.getaddrinfo("api", 8080, type=socket.SOCK_STREAM)})
    assert len(addresses) == 2, "this scenario requires two actual API processes"
    return ["http://" + address + ":8080" for address in addresses]


def counts():
    patterns = ("%SELECT ak.id, ak.org_id%", "%SELECT email FROM users WHERE id%",
                "%SELECT%jsonb_agg(jsonb_build_object%", "%SELECT%COALESCE((SELECT patterns FROM org_ignore_policies%")
    return [run.sql("SELECT COALESCE(sum(calls),0)::bigint AS n FROM pg_stat_statements WHERE query LIKE %s", (p,))[0]["n"] for p in patterns]


def verify():
    state = run.provision()
    peers = servers()
    admin = partial(run.request, server=peers[0])
    for index, field in enumerate(("key", "outsider_key")):
        scopes = ["query", "sync", "root:delete", "api_keys:read", "api_keys:write"]
        if index == 0:
            scopes.append("org:admin")
        state[field] = admin("POST", f"/admin/orgs/{state['org']}/users/{state['users'][index]}/api-keys",
            {"name": "api-access", "scopes": scopes}, statuses=(201,))["key"]
    run.save(state)
    viewer = state["outsider_key"]
    directory = Path("/state/api-access")
    directory.mkdir()
    text = "Orchid instruments record atmospheric pressure.\n"
    (directory / "observations.txt").write_text(text)
    roots = []
    for scope in ("org", "user", "restricted"):
        root = admin("POST", f"/admin/orgs/{state['org']}/roots", {
            "name": scope, "scope": scope, "source_path": str(directory),
            "owner_user_id": state["users"][0], "vector_disabled": True}, statuses=(201,))["id"]
        state["roots"].append(root)
        run.save(state)
        roots.append(root)
    shared, personal, restricted = roots

    def check(expected):
        for peer in peers:
            request = partial(run.request, server=peer, key=viewer)
            before = counts()
            result = request("GET", "/roots")
            after = counts()
            assert [b-a for a,b in zip(before, after)] == [1, 0, 1, 0], "root listing did not use one identity + one access query"
            assert {r["id"]: r["access"] for r in result} == expected
            for root in roots:
                before = counts()
                result = request("GET", f"/roots/{root}", statuses=(200,) if root in expected else (404,))
                assert [b-a for a,b in zip(before, counts())] == [1, 0, 1, 0]
                if root in expected:
                    assert result["access"] == expected[root]

    check({shared: ["read"]})
    group = admin("POST", f"/admin/orgs/{state['org']}/groups", {"name": "Observers"}, statuses=(201,))["id"]
    membership = f"/admin/orgs/{state['org']}/groups/{group}/members/{state['users'][1]}"
    grants = f"/admin/orgs/{state['org']}/roots/{restricted}/grants"
    admin("PUT", membership)
    grant = admin("POST", grants, {"principal_type": "group", "principal_id": group, "permissions": ["sync"]}, statuses=(201,))
    check({shared: ["read"], restricted: ["read", "sync"]})
    admin("DELETE", membership)
    check({shared: ["read"]})
    admin("DELETE", grants + "/" + grant["id"])
    for kind, principal in (("user", state["users"][1]), ("org", state["org"])):
        grant = admin("POST", grants, {"principal_type": kind, "principal_id": principal, "permissions": ["read"]}, statuses=(201,))
        check({shared: ["read"], restricted: ["read"]})
        admin("DELETE", grants + "/" + grant["id"])
        check({shared: ["read"]})
    # Effective policy combines two independent persisted policies in one query.
    for peer in peers:
        before = counts()
        policy = run.request("GET", "/ignore-policy", key=viewer, server=peer)
        assert policy == {"org_patterns": "", "user_patterns": ""}
        assert [b-a for a,b in zip(before, counts())] == [1, 0, 0, 1]
    run.request("PUT", "/ignore-policy/org", {"patterns": "*.tmp\n"}, key=state["key"], server=peers[0])
    run.request("PUT", "/ignore-policy/user", {"patterns": "*.bak\n"}, key=viewer, server=peers[1])
    assert run.request("GET", "/ignore-policy", key=viewer, server=peers[0]) == {"org_patterns": "*.tmp\n", "user_patterns": "*.bak\n"}

    state.update(root=shared, access_text=text)
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", shared, "--no-vector")
    files = run.wait_indexed(state)
    run.assert_source_retained(files["observations.txt"])
    for peer in peers:
        request = partial(run.request, server=peer, key=viewer)
        result = request("POST", f"/roots/{shared}/read", {"path": "observations.txt", "lines": {"start": 1, "end": 1}})
        assert result["lines"][0]["content"] == text.rstrip("\n")
        for selector in ({"root_id": shared}, {"root_ids": [shared, " " + shared + " ", ""]}, {"all_roots": True}):
            before = counts()
            result = request("POST", "/query", dict(selector, query="Orchid", mode="fts"))
            assert [b-a for a,b in zip(before, counts())] == [1, 0, 1, 0]
            assert result["roots_searched"] == 1 and result["results"]
            assert {r["root_id"] for r in result["results"]} == {shared}
        request("POST", "/query", {"root_ids": [shared, personal], "query": "Orchid", "mode": "fts"}, statuses=(404,))
        request("POST", "/query", {"root_ids": [" "], "query": "Orchid", "mode": "fts"}, statuses=(400,))
    admin("PUT", f"/admin/orgs/{state['org']}/members/{state['users'][1]}", {"role": "admin"})
    check({root: ["read", "sync", "delete", "admin"] for root in roots})
    for peer in peers:
        for selector in ({"root_ids": [restricted, shared, personal, restricted]}, {"all_roots": True}):
            before = counts()
            result = run.request("POST", "/query", dict(selector, query="Orchid", mode="fts"), key=viewer, server=peer)
            assert [b-a for a,b in zip(before, counts())] == [1, 0, 1, 0]
            assert result["roots_searched"] == 3
            assert {hit["root_id"] for hit in result["results"]} == {shared}
    admin("PUT", f"/admin/orgs/{state['org']}/members/{state['users'][1]}", {"role": "viewer"})
    check({shared: ["read"]})
    # Delete on one server; the other must reject this credential immediately.
    keys = run.request("GET", "/auth/api-keys", key=viewer, server=peers[0])
    keys = [key for key in keys if key["name"] == "api-access"]
    assert len(keys) == 1
    run.request("DELETE", "/auth/api-keys/" + keys[0]["id"], key=viewer, server=peers[0])
    restarted()
    print("Two API servers passed role/grant/revocation visibility, exact CLI source/read/search and constant query counts.", flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    for peer in servers():
        run.request("GET", "/roots", key=state["outsider_key"], server=peer, statuses=(401,))
        assert {r["id"] for r in run.request("GET", "/roots", key=state["key"], server=peer)} == set(state["roots"])
        result = run.request("POST", f"/roots/{state['root']}/read", {"path": "observations.txt", "lines": {"start": 1, "end": 1}}, key=state["key"], server=peer)
        assert result["lines"][0]["content"] == state["access_text"].rstrip("\n")
    print("Credential revocation and published reads agree across both API processes.", flush=True)


if __name__ == "__main__":
    {"verify": verify, "restarted": restarted}[sys.argv[1]]()
