"""Real source GC, capture retries and packed-file access boundaries."""

import hashlib
import json
from pathlib import Path
import urllib.request
import uuid

import run
from retention_security import capture, local_spools


def upload(state, root, payload):
    result = run.request("POST", f"/roots/{root}/sources/init", {"size": len(payload)}, key=state["key"])
    headers = {name: values[0] for name, values in result["headers"].items()}
    with urllib.request.urlopen(urllib.request.Request(result["url"], data=payload, method="PUT", headers=headers), timeout=30) as response:
        assert response.status == 200
    run.request("POST", f"/roots/{root}/sources/complete", {"object_key": result["object_key"]}, key=state["key"])
    return result


def check_packed_isolation(state):
    directory = Path("/state/packed-isolation")
    (directory / "protected").mkdir(parents=True)
    (directory / "public").mkdir()
    (directory / "protected" / "a.txt").write_text("Private sapphire telescope coordinates.\n")
    (directory / "public" / "b.txt").write_text("Public orchid observatory description.\n")
    root = run.new_root(state, "packed isolation", directory, True)
    capture(state, directory, root)
    files = run.wait_indexed(state, root)
    private = run.assert_source_retained(files["protected/a.txt"])
    public = run.assert_source_retained(files["public/b.txt"])
    assert {x["object_key"] for x in private["extents"]} == {x["object_key"] for x in public["extents"]}
    row, = run.sql("SELECT capture_id FROM file_versions WHERE id=%s", (files["protected/a.txt"]["version_id"],))
    acl_key = run.request("POST", f"/admin/orgs/{state['org']}/users/{state['users'][0]}/api-keys",
        {"name": "packed-acl", "scopes": ["acl:read", "acl:write"]}, statuses=(201,))["key"]
    run.request("POST", f"/roots/{root}/acls", {"path_prefix": "/protected/",
        "grant_to": "user:" + state["users"][0], "permission": "none"}, key=acl_key, statuses=(201,))
    # Both a fresh capture ID and the original ID must fail. A receipt is not
    # permission to bind additional paths to its packed bytes after acceptance.
    for capture_id in (str(uuid.uuid4()), row["capture_id"]):
        run.request("POST", f"/roots/{root}/versions", {"capture_id": capture_id,
            "files": [{"path": "public/stolen.txt", "source": private}]}, key=state["key"], statuses=(400,))
    run.request("POST", f"/roots/{root}/versions", {"capture_id": str(uuid.uuid4()), "files": [{
        "path": "public/b.txt", "previous_version_id": files["public/b.txt"]["version_id"], "source": private}]}, key=state["key"], statuses=(400,))
    assert not run.sql("SELECT id FROM file_catalog WHERE root_id=%s AND path=%s", (root, "public/stolen.txt"))
    assert run.catalog(state, root)["public/b.txt"]["version_id"] == files["public/b.txt"]["version_id"]

    # A second authorized root writer may reuse THIS file's public range, but
    # must not sign/complete another uploader's unbound or completed pack.
    run.request("POST", f"/admin/orgs/{state['org']}/roots/{root}/grants",
        {"principal_type": "user", "principal_id": state["users"][1], "permissions": ["sync"]}, statuses=(201,))
    second = dict(state, key=state["outsider_key"])
    unbound = upload(state, root, b"Private unfinished capture bytes.\n")
    run.request("POST", f"/roots/{root}/sources/init", {"size": len(b"Private unfinished capture bytes.\n"),
        "object_key": unbound["object_key"]}, key=second["key"], statuses=(409,))
    run.request("POST", f"/roots/{root}/sources/complete", {"object_key": unbound["object_key"]}, key=second["key"], statuses=(404,))
    stolen = {"format": 1, "size": len(b"Private unfinished capture bytes.\n"),
        "content_hash": "sha256:" + hashlib.sha256(b"Private unfinished capture bytes.\n").hexdigest(),
        "extents": [{"object_key": unbound["object_key"], "offset": 0, "length": len(b"Private unfinished capture bytes.\n")}]}
    run.request("POST", f"/roots/{root}/versions", {"capture_id": str(uuid.uuid4()),
        "files": [{"path": "public/stolen.txt", "source": stolen}]}, key=second["key"], statuses=(400,))
    run.request("POST", f"/roots/{root}/versions", {"capture_id": str(uuid.uuid4()), "files": [{
        "path": "public/b.txt", "previous_version_id": files["public/b.txt"]["version_id"], "source": public}]}, key=second["key"], statuses=(202,))
    def indexed():
        file = run.catalog(second, root)["public/b.txt"]
        return file if file["version_id"] == file["indexed_version_id"] else None
    run.eventually("authorized second writer's reused range to index", indexed)
    result = run.request("POST", f"/roots/{root}/read", {"path": "public/b.txt", "lines": {"start": 1, "end": 1}}, key=second["key"])
    assert result["lines"][0]["content"] == (directory / "public/b.txt").read_text().strip()
    print("Packed siblings, capture-ID replay grafts and another uploader's unbound bytes were isolated; authorized same-file range reuse indexed and read successfully.", flush=True)


def begin_retention(state):
    directory = Path("/state/source-retention")
    directory.mkdir()
    path = directory / "replace.txt"
    path.write_text("Obsolete violet telescope record.\n")
    root = run.new_root(state, "source retention", directory, True)
    capture(state, directory, root)
    first = run.wait_indexed(state, root)[path.name]
    obsolete = run.assert_source_retained(first)
    receipt, = (local_spools(root) / "completed").iterdir()
    original_request = json.loads((receipt / "journal.json").read_text())
    # Save the exact API payload for idempotent replay after physical object GC.
    replay = {"capture_id": original_request["request"]["capture_id"], "files": [{"path": path.name, "source": obsolete}]}
    path.write_text("Current orchid telescope record.\n")
    (directory / "append.txt").write_text("Sapphire observatory measurement one.\n")
    capture(state, directory, root)
    files = run.wait_indexed(state, root)
    mixed = run.assert_source_retained(files["append.txt"])
    path.write_text("Replacement coral telescope record.\n")
    with (directory / "append.txt").open("a") as output:
        output.write("Sapphire observatory measurement two.\n")
    capture(state, directory, root)
    run.wait_indexed(state, root)

    # Interrupt a real CLI's upload before catalog commit.
    pending_dir = Path("/state/source-retention-pending")
    pending_dir.mkdir()
    pending_path = pending_dir / "retry.txt"
    payload = b"Retained original orchid retry bytes.\n"
    pending_path.write_bytes(payload)
    pending_root = run.new_root(state, "retired pending capture", pending_dir, True)
    # The failed CLI leaves its real pending journal and allocated key. No
    # application tables or journals are edited to manufacture pending state.
    run.fault("POST", "/proxies/source-upload", {"enabled": False})
    try:
        capture(state, pending_dir, pending_root, success=False)
    finally:
        run.fault("POST", "/proxies/source-upload", {"enabled": True})
    pending, = (local_spools(pending_root) / "pending").iterdir()
    journal = json.loads((pending / "journal.json").read_text())
    pack, = journal["packs"]
    # This is an allocated but unaccepted upload; GC must retain its object
    # identity through the real signed deadline, even though no bytes arrived.
    assert pack["object_key"] and not pack["complete"] and not run.catalog(state, pending_root)
    pending_path.write_text("Changed live filesystem bytes must not replace the pending capture.\n")
    multipart = run.request("POST", f"/roots/{root}/sources/multipart/init", {"request_id": str(uuid.uuid4()), "size": 20 << 20}, key=state["key"])
    snapshot = {"root": root, "directory": str(directory), "first": first, "replay": replay,
        "obsolete": obsolete, "mixed": mixed, "pending_root": pending_root, "pending_dir": str(pending_dir),
        "pending_journal": str(pending / "journal.json"), "pending_capture": journal["request"]["capture_id"],
        "pending_hash": "sha256:" + hashlib.sha256(payload).hexdigest(), "pending_pack": pack["object_key"],
        "multipart": multipart}
    state["source_retention"] = snapshot
    run.save(state)
    keys = [pack["object_key"], multipart["object_key"], *[x["object_key"] for x in obsolete["extents"]]]
    assert all(not row["retired"] and row["pinned"] for row in run.sql(
        "SELECT retired_at IS NOT NULL AS retired,authorized_until>NOW() AS pinned FROM source_objects WHERE object_key=ANY(%s)", (keys,)))
    print("Source retention clocks started; pending upload, obsolete pack and abandoned multipart await actual authorization expiry.", flush=True)


def finish_retention(state):
    snapshot = state["source_retention"]
    keys = [snapshot["pending_pack"], snapshot["multipart"]["object_key"], *[x["object_key"] for x in snapshot["obsolete"]["extents"]]]
    def deleted():
        rows = run.sql("SELECT object_key,retired_at,deleted_at FROM source_objects WHERE object_key=ANY(%s)", (keys,))
        return len(rows) == len(set(keys)) and all(row["retired_at"] is not None and row["deleted_at"] is not None for row in rows)
    run.eventually("scheduled source GC after real 15-minute authorization expiry", deleted, 1200)
    for key in keys:
        try:
            run.s3.head_object(Bucket=run.BUCKET, Key=key)
        except run.s3.exceptions.ClientError as error:
            assert error.response["ResponseMetadata"]["HTTPStatusCode"] == 404
        else:
            raise AssertionError("retired source pack is still present")
    assert not run.s3.list_multipart_uploads(Bucket=run.BUCKET, Prefix=snapshot["multipart"]["object_key"]).get("Uploads")
    before = run.catalog(state, snapshot["root"])
    response = run.request("POST", f"/roots/{snapshot['root']}/versions", snapshot["replay"], key=state["key"], statuses=(202,))
    assert response["versions"][0]["version_id"] == snapshot["first"]["version_id"]
    assert run.catalog(state, snapshot["root"])["replace.txt"]["version_id"] == before["replace.txt"]["version_id"]
    path = Path(snapshot["directory"]) / "append.txt"
    with path.open("a") as output:
        output.write("Sapphire observatory measurement three.\n")
    capture(state, path.parent, snapshot["root"])
    file = run.wait_indexed(state, snapshot["root"])[path.name]
    manifest = run.assert_source_retained(file)
    assert manifest["extents"][:len(snapshot["mixed"]["extents"])] == snapshot["mixed"]["extents"]
    result = run.request("POST", f"/roots/{snapshot['root']}/read", {"path": path.name, "lines": {"start": 1, "end": 3}}, key=state["key"])
    assert [line["content"] for line in result["lines"]] == path.read_text().splitlines()
    # Ordinary sync first resumes the frozen spool, then scans the live path.
    # Inspect both versions to prove the original bytes were accepted first.
    capture(state, Path(snapshot["pending_dir"]), snapshot["pending_root"])
    files = run.wait_indexed(state, snapshot["pending_root"])
    original, = run.sql("""SELECT v.id,v.content_hash,v.source_manifest_ref FROM file_versions v JOIN file_catalog f ON f.id=v.file_id
        WHERE f.root_id=%s AND v.capture_id=%s""", (snapshot["pending_root"], snapshot["pending_capture"]))
    assert original["content_hash"] == snapshot["pending_hash"]
    renewed = run.object_json(original["source_manifest_ref"])
    assert all(x["object_key"] != snapshot["pending_pack"] for x in renewed["extents"])
    assert not list((local_spools(snapshot["pending_root"]) / "pending").iterdir())
    for file in files.values():
        run.assert_source_retained(file)
    print("Obsolete/unaccepted packs and abandoned multipart uploads were cleaned after real expiry; mixed-pack append reuse, old receipt replay and re-upload of original pending bytes passed.", flush=True)
