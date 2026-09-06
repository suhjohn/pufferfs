"""Revocations while real S3 manifest responses are held at the TCP boundary."""

from concurrent.futures import ThreadPoolExecutor
import hashlib
import uuid

import run


def capture_across_revocation(state, root, key, revoke, restore, *, resume_key=None):
    from source_retention import upload
    payload = b"Orchid observatory permission boundary measurements.\n"
    source = upload(dict(state, key=key), root, payload)
    capture = {"capture_id": str(uuid.uuid4()), "files": [{"path": "protected/measurements.txt", "source": {
        "format": 1, "content_hash": "sha256:" + hashlib.sha256(payload).hexdigest(), "size": len(payload),
        "extents": [{"object_key": source["object_key"], "offset": 0, "length": len(payload)}]}}]}
    name = "hold-permission-response"
    run.fault("POST", "/proxies/source-upload/toxics", {"name": name, "type": "latency",
        "stream": "downstream", "attributes": {"latency": 60000, "jitter": 0}})
    held = True
    try:
        with ThreadPoolExecutor(max_workers=1) as executor:
            response = executor.submit(run.request, "POST", f"/roots/{root}/versions", capture,
                key=key, statuses=(403,))
            try:
                run.eventually("manifest persisted before permission revocation", lambda:
                    run.s3.list_objects_v2(Bucket=run.BUCKET,
                        Prefix=f"sources/{state['org']}/{root}/manifests/").get("Contents"), 30)
                assert not response.done(), "capture escaped fault before revocation"
                revoke()
            finally:
                run.fault("DELETE", f"/proxies/source-upload/toxics/{name}")
                held = False
            response.result(timeout=90)
        assert not run.sql("SELECT id FROM file_catalog WHERE root_id=%s", (root,)), "revoked capture was accepted"
        assert not run.sql("SELECT path FROM file_content_proofs WHERE root_id=%s", (root,)), "revoked capture issued proofs"
        assert not run.sql("SELECT capture_id FROM source_objects WHERE root_id=%s AND capture_id IS NOT NULL", (root,)), "revoked capture bound its source pack"
    finally:
        if held:
            run.fault("DELETE", f"/proxies/source-upload/toxics/{name}")
        restore()
    # Restoring permission must allow this exact capture/spool to finish. A
    # revoked credential gets a new key for the same uploader, not new bytes.
    key = resume_key or key
    run.request("POST", f"/roots/{root}/versions", capture, key=key, statuses=(202,))
    file, = run.wait_indexed(dict(state, key=key), root).values()
    run.assert_source_retained(file)
    result = run.request("POST", f"/roots/{root}/read", {"path": "protected/measurements.txt",
        "lines": {"start": 1, "end": 1}}, key=key)
    assert result["lines"][0]["content"] == payload.decode().strip()


def check_capture_revocations(state):
    org, owner, user = state["org"], *state["users"][:2]
    management_key = run.request("POST", f"/admin/orgs/{org}/users/{owner}/api-keys",
        {"name": "permission-management", "scopes": ["org:admin", "api_keys:write", "acl:write"]}, statuses=(201,))["key"]

    def root_for(scope, name):
        root = run.request("POST", f"/admin/orgs/{org}/roots", {"name": name,
            "source_path": "/state/permission-capture", "scope": scope, "vector_disabled": True}, statuses=(201,))["id"]
        state["roots"].append(root)
        run.save(state)
        return root

    def set_role(role):
        run.request("PUT", f"/admin/orgs/{org}/members/{user}", {"role": role})

    def grant(root, kind, principal, permissions):
        return run.request("POST", f"/admin/orgs/{org}/roots/{root}/grants",
            {"principal_type": kind, "principal_id": principal, "permissions": permissions}, statuses=(201,))

    def delete_grant(root, value):
        run.request("DELETE", f"/admin/orgs/{org}/roots/{root}/grants/{value['id']}")

    for kind, principal in (("user", user), ("org", org), ("group", None)):
        root = root_for("restricted", kind + " grant revocation")
        if kind == "group":
            principal = run.request("POST", f"/admin/orgs/{org}/groups", {"name": "capture writers"}, statuses=(201,))["id"]
            run.request("PUT", f"/admin/orgs/{org}/groups/{principal}/members/{user}")
        value = grant(root, kind, principal, ["sync"])
        capture_across_revocation(state, root, state["outsider_key"],
            lambda: delete_grant(root, value), lambda: grant(root, kind, principal, ["sync"]))
        print(f"{kind} grant deletion: in-flight capture rejected; restored capture indexed/read.", flush=True)

    root = root_for("restricted", "grant permission downgrade")
    grant(root, "user", user, ["sync"])
    capture_across_revocation(state, root, state["outsider_key"],
        lambda: grant(root, "user", user, ["read"]), lambda: grant(root, "user", user, ["sync"]))
    print("Grant permission downgrade: in-flight capture rejected; restored capture indexed/read.", flush=True)

    root = root_for("restricted", "group membership revocation")
    group = run.request("POST", f"/admin/orgs/{org}/groups", {"name": "temporary capture writers"}, statuses=(201,))["id"]
    member_path = f"/admin/orgs/{org}/groups/{group}/members/{user}"
    run.request("PUT", member_path)
    grant(root, "group", group, ["sync"])
    capture_across_revocation(state, root, state["outsider_key"],
        lambda: run.request("DELETE", member_path), lambda: run.request("PUT", member_path))
    print("Group membership removal: in-flight capture rejected; restored capture indexed/read.", flush=True)

    root = root_for("org", "role downgrade")
    set_role("editor")
    try:
        capture_across_revocation(state, root, state["outsider_key"], lambda: set_role("viewer"), lambda: set_role("editor"))
    finally:
        set_role("viewer")
    print("Editor role downgrade: in-flight capture rejected; restored capture indexed/read.", flush=True)

    root = root_for("restricted", "role-dependent folder deny")
    grant(root, "user", user, ["sync"])
    run.request("POST", f"/roots/{root}/acls", {"path_prefix": "/protected/", "grant_to": "role:viewer",
        "permission": "none"}, key=management_key, statuses=(201,))
    set_role("editor")
    try:
        capture_across_revocation(state, root, state["outsider_key"], lambda: set_role("viewer"), lambda: set_role("editor"))
    finally:
        set_role("viewer")
    print("Fresh role-dependent folder deny: in-flight capture rejected; restored capture indexed/read.", flush=True)

    root = root_for("restricted", "organization membership revocation")
    grant(root, "user", user, ["sync"])
    capture_across_revocation(state, root, state["outsider_key"],
        lambda: run.request("DELETE", f"/org/members/{user}", key=management_key), lambda: set_role("viewer"))
    print("Organization membership removal: in-flight capture rejected; restored capture indexed/read.", flush=True)

    root = root_for("restricted", "API key revocation")
    grant(root, "user", user, ["sync"])
    key = run.request("POST", f"/admin/orgs/{org}/users/{user}/api-keys",
        {"name": "revoked-upload-credential", "scopes": ["sync", "query", "api_keys:read"]}, statuses=(201,))["key"]
    key_id, = [entry["id"] for entry in run.request("GET", "/auth/api-keys", key=key) if entry["name"] == "revoked-upload-credential"]
    capture_across_revocation(state, root, key,
        lambda: run.request("DELETE", f"/auth/api-keys/{key_id}", key=management_key), lambda: None,
        resume_key=state["outsider_key"])
    print("API key revocation: in-flight capture rejected; new credential resumed the same capture and indexed/read.", flush=True)
