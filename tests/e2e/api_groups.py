"""Group provisioning and retries through two API processes and real Postgres."""

from concurrent.futures import ThreadPoolExecutor
import json
import threading
import uuid

from api_access import servers
import run


def parallel(peers, count, action):
    barrier = threading.Barrier(count)
    def call(index):
        barrier.wait(timeout=20)
        return action(peers[index % len(peers)], index)
    with ThreadPoolExecutor(max_workers=count) as pool:
        return list(pool.map(call, range(count)))


def calls(pattern):
    return run.sql("SELECT COALESCE(sum(calls),0)::bigint AS n FROM pg_stat_statements WHERE query LIKE %s", (pattern,))[0]["n"]


def group_version(group):
    return run.sql("SELECT xmin::text AS revision,* FROM groups WHERE id=%s", (group,))


def verify():
    state = json.loads(run.STATE.read_text())
    org, user = state["org"], state["users"][1]
    peers = servers()
    base = f"/admin/orgs/{org}/groups"
    patterns = {"create": "SELECT * FROM upsert_org_group%",
                "add": "SELECT group_id,user_id,joined_at FROM add_org_group_member%",
                "list": "SELECT (SELECT jsonb_agg(to_jsonb(g)%FROM groups g%",
                "members": "SELECT (SELECT jsonb_agg(jsonb_build_object%FROM group_members gm%"}
    fixtures = []
    for body in ({"id": str(uuid.uuid4()), "name": "Shared ID"},
                 {"external_id": "directory-group", "name": "Shared external ID"}):
        before = calls(patterns["create"])
        results = parallel(peers, 8, lambda peer, _: run.request("POST", base, body, server=peer, statuses=(201,)))
        assert calls(patterns["create"]) - before == 8
        assert all(result == results[0] for result in results), "concurrent identity created different groups"
        group = results[0]
        assert group["created_at"] == group["updated_at"], "identical concurrent upserts rewrote data"
        version = group_version(group["id"])
        for peer in peers:
            before = calls(patterns["create"])
            assert run.request("POST", base, body, server=peer, statuses=(201,)) == group
            assert calls(patterns["create"]) - before == 1
            assert group_version(group["id"]) == version, "no-op group retry changed its row version"
        fixtures.append(group)
    # External identity wins over a caller-provided ID, matching the upsert contract.
    body = {"id": str(uuid.uuid4()), "name": "Renamed directory group", "external_id": "directory-group"}
    updated = run.request("POST", base, body, server=peers[1], statuses=(201,))
    assert updated["id"] == fixtures[1]["id"] and updated["created_at"] == fixtures[1]["created_at"]
    assert updated["updated_at"] != fixtures[1]["updated_at"]
    fixtures[1] = updated
    # An existing ID can gain, replace, or clear its external ID.
    for external in ("attached-directory-id", "replacement-directory-id", ""):
        result = run.request("POST", base, {"id": fixtures[0]["id"], "name": "Shared ID", "external_id": external},
            server=peers[0], statuses=(201,))
        assert result["id"] == fixtures[0]["id"] and result.get("external_id", "") == external
        fixtures[0] = result
    # Group names are unique constraints, not identities that merge distinct IDs.
    results = parallel(peers, 8, lambda peer, _: run.request("POST", base,
        {"name": "Conflicting names"}, server=peer, statuses=(201, 409)))
    assert sum("id" in result for result in results) == 1
    assert run.sql("SELECT count(*) AS n FROM groups WHERE org_id=%s AND name=%s", (org, "Conflicting names"))[0]["n"] == 1
    run.request("POST", base, {"id": fixtures[0]["id"], "name": fixtures[1]["name"]}, statuses=(409,))
    assert group_version(fixtures[0]["id"])[0]["name"] == fixtures[0]["name"], "conflicting update partially persisted"

    group = fixtures[0]["id"]
    members = base + f"/{group}/members"
    member_path = members + "/" + user
    for peer in peers:
        before = calls(patterns["members"])
        assert run.request("GET", members, server=peer) is None
        assert calls(patterns["members"]) - before == 1
    before = calls(patterns["add"])
    results = parallel(peers, 8, lambda peer, _: run.request("PUT", member_path, server=peer))
    assert calls(patterns["add"]) - before == 8 and all(result == results[0] for result in results)
    member = results[0]
    stamp = run.sql("SELECT xmin::text AS revision,* FROM group_members WHERE group_id=%s", (group,))
    for peer in peers:
        before = calls(patterns["add"])
        assert run.request("PUT", member_path, server=peer) == member
        assert calls(patterns["add"]) - before == 1
        assert run.sql("SELECT xmin::text AS revision,* FROM group_members WHERE group_id=%s", (group,)) == stamp
        before = calls(patterns["members"])
        result = run.request("GET", members, server=peer)
        assert calls(patterns["members"]) - before == 1 and len(result) == 1
        assert result[0]["user_id"] == user and result[0]["joined_at"] == member["joined_at"]
        profile = run.sql("SELECT email,name FROM users WHERE id=%s", (user,))[0]
        assert all(result[0][field] == value for field, value in profile.items())
        before = calls(patterns["list"])
        listed = run.request("GET", base, server=peer)
        assert calls(patterns["list"]) - before == 1
        assert [row["name"] for row in listed] == sorted(row["name"] for row in listed)
        for expected in fixtures:
            assert expected in listed
        run.request("GET", "/admin/orgs/missing-org/groups", server=peer, statuses=(404,))
        run.request("POST", "/admin/orgs/missing-org/groups", {"name": "Missing"}, server=peer, statuses=(404,))
        run.request("GET", base + "/missing-group/members", server=peer, statuses=(404,))
        run.request("PUT", members + "/missing-user", server=peer, statuses=(404,))
        for method, path, payload in (("POST", base, {"name": "Forbidden"}), ("GET", base, None),
                ("GET", members, None), ("PUT", member_path, None), ("DELETE", member_path, None)):
            run.request(method, path, payload, key=state["key"], server=peer, statuses=(401,))
    run.request("DELETE", member_path, server=peers[0])
    run.request("DELETE", member_path, server=peers[1], statuses=(404,))
    assert run.request("GET", members, server=peers[1]) is None
    # PUT racing DELETE always leaves the API and durable membership in agreement.
    for _ in range(6):
        parallel(peers, 2, lambda peer, i: run.request(("PUT", "DELETE")[i], member_path,
            server=peer, statuses=(200,) if i == 0 else (200, 404)))
        stored = run.sql("SELECT user_id FROM group_members WHERE group_id=%s", (group,))
        visible = run.request("GET", members, server=peers[1]) or []
        assert [row["user_id"] for row in stored] == [row["user_id"] for row in visible]

    other = run.request("POST", "/admin/orgs", {"name": "Other group org", "slug": "group-" + uuid.uuid4().hex})["id"]
    state.setdefault("other_orgs", []).append(other)
    run.save(state)
    other_base = f"/admin/orgs/{other}/groups"
    assert run.request("GET", other_base) is None
    foreign = run.request("POST", other_base, {"name": "Foreign identity"}, statuses=(201,))
    before = group_version(foreign["id"])
    run.request("POST", base, {"id": foreign["id"], "name": "Unauthorized move"}, statuses=(409,))
    assert group_version(foreign["id"]) == before
    run.request("GET", base + f"/{foreign['id']}/members", statuses=(404,))
    run.request("PUT", base + f"/{foreign['id']}/members/{user}", statuses=(404,))
    run.request("PUT", other_base + f"/{foreign['id']}/members/{user}", statuses=(404,))
    run.request("PUT", f"/admin/orgs/{other}/members/{user}", {"role": "viewer"})
    # Organization deletion must not deadlock against group-member insertion or
    # leave cross-org/orphan memberships. Exercise the real deletion API.
    parallel(peers, 2, lambda peer, i: run.request(("PUT", "DELETE")[i],
        other_base + f"/{foreign['id']}/members/{user}" if i == 0 else f"/admin/orgs/{other}",
        server=peer, statuses=(200, 404) if i == 0 else (200,)))
    assert not run.sql("SELECT 1 FROM groups WHERE org_id=%s UNION ALL SELECT 1 FROM group_members WHERE org_id=%s", (other, other))
    state["group_fixtures"] = fixtures
    run.save(state)
    print("Two API servers passed concurrent group identities, no-op group/member writes, unique-name conflicts, tenant isolation, membership/delete races and one-call listings.", flush=True)


def restarted():
    state = json.loads(run.STATE.read_text())
    for peer in servers():
        groups = run.request("GET", f"/admin/orgs/{state['org']}/groups", server=peer)
        assert all(group in groups for group in state["group_fixtures"])
    print("Group identity/profile state survived both API restarts.", flush=True)


if __name__ == "__main__":
    import sys
    {"verify": verify, "restarted": restarted}[sys.argv[1]]()
