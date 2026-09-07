"""Membership changes through two actual API servers; no SQL writes or hooks."""

from concurrent.futures import ThreadPoolExecutor
from functools import partial
import json
import threading

from api_access import servers
import run


def verify():
    state = json.loads(run.STATE.read_text())
    org, (first, second) = state["org"], state["users"][:2]
    peers = servers()
    admin = partial(run.request, server=peers[0])
    key = admin("POST", f"/admin/orgs/{org}/users/{second}/api-keys",
        {"name": "member-races", "scopes": ["org:admin"]}, statuses=(201,))["key"]
    keys = [state["key"], key]

    def role(user, value):
        return admin("PUT", f"/admin/orgs/{org}/members/{user}", {"role": value})

    def member(user):
        return run.sql("SELECT role,xmin::text AS revision,joined_at FROM org_members WHERE org_id=%s AND user_id=%s", (org, user))

    def calls():
        return run.sql("SELECT COALESCE(sum(calls),0)::bigint AS n FROM pg_stat_statements WHERE query LIKE %s",
            ("SELECT * FROM change_org_member%",))[0]["n"]

    def change(peer, credential, mode, target, value="viewer", statuses=(200,)):
        path = "/org/members" if mode == "POST" else "/org/members/" + target
        body = None if mode == "DELETE" else {"user_id": target, "role": value}
        return run.request(mode, path, body, key=credential, server=peer, statuses=statuses)

    # Provisioning uses one function call and returns profile/role/joined_at.
    before = calls()
    updated = role(second, "editor")
    assert calls() - before == 1 and updated["user_id"] == second and updated["role"] == "editor"
    stamp = member(second)
    for peer in peers:
        before = calls()
        result = change(peer, keys[0], "PUT", second, "editor")
        assert calls() - before == 1 and result == updated
        change(peer, keys[0], "POST", second, "editor")
        assert member(second) == stamp, "unchanged membership performed a data update"
        for mode in ("POST", "PUT", "DELETE"):
            change(peer, keys[0], mode, first, statuses=(403,))
        change(peer, keys[0], "POST", first, "owner")  # idempotent self-add
    role(second, "admin")
    for peer in peers:
        for mode in ("POST", "PUT", "DELETE"):
            change(peer, keys[1], mode, first, statuses=(403,))
        change(peer, keys[0], "PUT", "missing-member", statuses=(404,))
        change(peer, keys[0], "POST", "missing-user", statuses=(404,))
        change(peer, keys[0], "POST", second, "invalid-role", statuses=(400,))
    print("One-call member mutations, unchanged-row retries, missing users and POST/PUT/DELETE privilege parity passed.", flush=True)

    # Different servers receive simultaneous mutually destructive owner changes.
    # Loser may be rejected during authentication or during locked revalidation.
    try:
        for mode in ("POST", "PUT", "DELETE"):
            for _ in range(6):
                role(first, "owner")
                role(second, "owner")
                barrier = threading.Barrier(2)
                def race(index):
                    barrier.wait(timeout=10)
                    return change(peers[index], keys[index], mode, (second, first)[index], statuses=(200, 401, 403))
                with ThreadPoolExecutor(max_workers=2) as executor:
                    results = list(executor.map(race, (0, 1)))
                assert sum("error" not in result for result in results) == 1, (mode, results)
                assert run.sql("SELECT count(*) AS n FROM org_members WHERE org_id=%s AND role='owner'", (org,))[0]["n"] == 1
            print(f"{mode}: six simultaneous cross-server owner changes preserved exactly one owner.", flush=True)
    finally:
        role(first, "owner")
        role(second, "viewer")
    print("Membership concurrency scenarios passed; original memberships restored through the admin API.", flush=True)


if __name__ == "__main__":
    verify()
