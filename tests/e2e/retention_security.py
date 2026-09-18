"""CLI capture retention and tenant boundaries against the production processes."""

from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import subprocess
import urllib.error
import urllib.request
import uuid

import run


def local_spools(root):
    caches = list(Path("/root/.tpfs/roots", root).glob("file-capture-*"))
    assert len(caches) == 1
    return caches[0]


def capture(state, directory, root, *, success=True, force=False):
    command = ["pufferfs", "sync", str(directory), "--id", root, "--no-vector"]
    if force:
        command.append("--force")
    process = subprocess.run(command,
        env=dict(os.environ, PUFFERFS_API_KEY=state["key"], PUFFERFS_CAPTURE_SPOOL_BYTES=str(8 << 20)),
        text=True, capture_output=True, timeout=180)
    assert (process.returncode == 0) == success, process.stderr[-2000:]
    return process


def check_local_retention(state):
    directory = Path("/state/spool-retention")
    directory.mkdir()
    text = "Orchid observatory telescope.\n"
    (directory / "a.txt").write_text(text)
    with (directory / "z.txt").open("wb") as output:
        output.write(b"x" * (5 << 20))
    root = run.new_root(state, "spool retention", directory, True)
    result = capture(state, directory, root, success=False)
    assert "spool limit" in result.stderr
    assert "needs 5242880 new bytes" in result.stderr
    cache = local_spools(root)
    assert not list((cache / "pending").glob("*/journal.json")), "failed creation published a retry journal"
    assert not list((cache / "pending").glob("*/pack-*")), "oversized file was partially copied"
    incomplete, = (cache / "pending").iterdir()
    accepted = run.catalog(state, root)
    assert set(accepted) == {"a.txt"}, "completed prefix was not accepted before the oversized file"
    run.assert_source_retained(accepted["a.txt"])

    (directory / "z.txt").write_text("Violet garden report.\n")
    capture(state, directory, root)
    assert not incomplete.exists(), "incomplete unsubmitted capture was not cleaned"
    assert run.catalog(state, root)["a.txt"]["version_id"] == accepted["a.txt"]["version_id"]
    assert not list((cache / "completed").glob("*/pack-*")), "accepted source bytes remained locally"
    files = run.wait_indexed(state, root)
    first = run.assert_source_retained(files["a.txt"])
    with (directory / "a.txt").open("a") as output:
        output.write("Appended violet measurements.\n")
    capture(state, directory, root)
    files = run.wait_indexed(state, root)
    appended = run.assert_source_retained(files["a.txt"])
    assert appended["extents"][:len(first["extents"])] == first["extents"], "cleanup broke append reuse"
    assert not list((cache / "completed").glob("*/pack-*"))
    for path, file in files.items():
        run.assert_source_retained(file)
        run.request("POST", f"/roots/{root}/read", {"path": path, "lines": {"start": 1, "end": 1}}, key=state["key"])
    result = run.request("POST", "/query", {"root_id": root, "query": "violet", "mode": "fts", "top_k": 5}, key=state["key"])
    assert result["results"]
    print("Spool limit, incomplete-capture cleanup, accepted-pack removal, retained originals and append reuse passed.")


def check_unaccepted_retention(state):
    directory = Path("/state/unaccepted-retention")
    directory.mkdir()
    path = directory / "measurements.txt"
    payload = b"Orchid telescope measurements.\n".ljust(3 << 20, b" ")
    path.write_bytes(payload)
    root = run.new_root(state, "unaccepted retention", directory, True)
    run.fault("POST", "/proxies/source-upload", {"enabled": False})
    try:
        result = capture(state, directory, root, success=False)
        assert "upload transport failed" in result.stderr
    finally:
        run.fault("POST", "/proxies/source-upload", {"enabled": True})
    cache = local_spools(root)
    pending, = (cache / "pending").iterdir()
    journal = json.loads((pending / "journal.json").read_text())
    pack, = journal["packs"]
    assert not journal.get("accepted") and (pending / pack["name"]).read_bytes() == payload
    assert not run.catalog(state, root)

    # A competing writer uses only the public upload and registration APIs.
    competing = b"Violet remote measurement.\n"
    upload = run.request("POST", f"/roots/{root}/sources/init", {"size": len(competing)}, key=state["key"])
    headers = {name: values[0] for name, values in upload["headers"].items()}
    with urllib.request.urlopen(urllib.request.Request(upload["url"], data=competing, method="PUT", headers=headers), timeout=30) as response:
        assert response.status == 200
    run.request("POST", f"/roots/{root}/sources/complete", {"object_key": upload["object_key"]}, key=state["key"])
    run.request("POST", f"/roots/{root}/versions", {"capture_id": str(uuid.uuid4()), "files": [{
        "path": path.name, "source": {"format": 1, "content_hash": "sha256:" + hashlib.sha256(competing).hexdigest(),
        "size": len(competing), "extents": [{"object_key": upload["object_key"], "offset": 0, "length": len(competing)}]}}]}, key=state["key"], statuses=(202,))
    remote = run.catalog(state, root)[path.name]
    result = capture(state, directory, root, success=False)
    assert "conflicts with a newer remote version" in result.stderr
    retried = json.loads((pending / "journal.json").read_text())
    assert retried["request"] == journal["request"] and not retried.get("accepted")
    assert (pending / pack["name"]).read_bytes() == payload

    # Explicit force archives the conflict; it must not evict its bytes to make
    # room for another 3 MiB capture inside the 8 MiB budget (4 MiB reserve).
    result = capture(state, directory, root, success=False, force=True)
    assert "spool limit" in result.stderr
    conflict, = (cache / "conflicts").iterdir()
    archived = json.loads((conflict / "journal.json").read_text())
    assert archived["request"] == journal["request"] and not archived.get("accepted")
    assert (conflict / pack["name"]).read_bytes() == payload
    assert run.catalog(state, root)[path.name]["version_id"] == remote["version_id"]
    assert sum(item.stat().st_size for category in ("pending", "completed", "conflicts")
        for item in (cache / category).rglob("*") if item.is_file()) <= 8 << 20

    path.write_text("Sapphire telescope measurements.\n")
    capture(state, directory, root, force=True)
    file = run.wait_indexed(state, root)[path.name]
    assert file["version_id"] != remote["version_id"]
    run.assert_source_retained(file)
    assert file["content_hash"] == "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()
    assert (conflict / pack["name"]).read_bytes() == payload
    assert not list((cache / "completed").glob("*/pack-*"))
    assert not list((cache / "pending").iterdir())
    result = run.request("POST", f"/roots/{root}/read", {"path": path.name, "lines": {"start": 1, "end": 1}}, key=state["key"])
    assert [line["content"] for line in result["lines"]] == path.read_text().splitlines()
    print("Pending bytes survived upload failure and conflicting retries; spool pressure preserved the archived capture while a smaller replacement remained publishable.")


def check_tenant_uploads(state):
    # Provision another organization via public admin workflows; no direct DB
    # writes or guessed identity tokens. Preserve cleanup identity immediately.
    org = run.request("POST", "/admin/orgs", {"name": "Other synthetic tenant", "slug": "e2e-" + uuid.uuid4().hex})
    state.setdefault("other_orgs", []).append(org["id"])
    run.save(state)
    owner = state["users"][0]
    run.request("PUT", f"/admin/orgs/{org['id']}/members/{owner}", {"role": "owner"})
    foreign_key = run.request("POST", f"/admin/orgs/{org['id']}/users/{owner}/api-keys",
        {"name": "e2e-foreign", "scopes": ["sync", "query", "root:delete"]}, statuses=(201,))["key"]
    foreign = run.request("POST", "/roots", {"name": "Other tenant root", "scope": "user",
        "source_path": "/state/other-tenant", "vector_disabled": True}, key=foreign_key, statuses=(201,))["id"]
    directory = Path("/state/upload-isolation")
    directory.mkdir()
    root = run.new_root(state, "upload isolation", directory, True)
    payload = b"Captured tenant-private source.\n"
    upload = run.request("POST", f"/roots/{root}/sources/init", {"size": len(payload)}, key=state["key"])
    key = upload["object_key"]
    assert key.startswith(f"sources/{state['org']}/{root}/")
    run.request("POST", f"/roots/{foreign}/sources/init", {"size": len(payload)}, key=state["key"], statuses=(404,))
    run.request("POST", f"/roots/{foreign}/sources/init", {"size": len(payload), "object_key": key},
                key=foreign_key, statuses=(409,))
    headers = {name: values[0] for name, values in upload["headers"].items()}
    with urllib.request.urlopen(urllib.request.Request(upload["url"], data=payload, method="PUT", headers=headers), timeout=30) as response:
        assert response.status == 200
    run.request("POST", f"/roots/{root}/sources/complete", {"object_key": key}, key=state["key"])
    run.request("POST", f"/roots/{foreign}/sources/complete", {"object_key": key}, key=foreign_key, statuses=(404,))
    request = {"capture_id": str(uuid.uuid4()), "files": [{"path": "stolen.txt", "source": {
        "format": 1, "content_hash": "sha256:" + hashlib.sha256(payload).hexdigest(), "size": len(payload),
        "extents": [{"object_key": key, "offset": 0, "length": len(payload)}]}}]}
    run.request("POST", f"/roots/{foreign}/versions", request, key=foreign_key, statuses=(400,))
    assert not run.sql("SELECT * FROM file_catalog WHERE root_id=%s", (foreign,))
    print("Cross-tenant root signing, key renewal, completion and source-reference forgery were rejected.")


def check_capture_acl_race(state):
    root = run.new_root(state, "capture permission race", "/state/capture-permission-race", True)
    payload = b"Orchid protected observatory measurements.\n"
    upload = run.request("POST", f"/roots/{root}/sources/init", {"size": len(payload)}, key=state["key"])
    headers = {name: values[0] for name, values in upload["headers"].items()}
    with urllib.request.urlopen(urllib.request.Request(upload["url"], data=payload, method="PUT", headers=headers), timeout=30) as response:
        assert response.status == 200
    run.request("POST", f"/roots/{root}/sources/complete", {"object_key": upload["object_key"]}, key=state["key"])
    owner = state["users"][0]
    acl_key = run.request("POST", f"/admin/orgs/{state['org']}/users/{owner}/api-keys",
        {"name": "capture-race-acl", "scopes": ["acl:read", "acl:write"]}, statuses=(201,))["key"]
    capture_request = {"capture_id": str(uuid.uuid4()), "files": [{"path": "protected/measurements.txt", "source": {
        "format": 1, "content_hash": "sha256:" + hashlib.sha256(payload).hexdigest(), "size": len(payload),
        "extents": [{"object_key": upload["object_key"], "offset": 0, "length": len(payload)}]}}]}
    toxic = "/proxies/source-upload/toxics/hold-capture-response"
    run.fault("POST", "/proxies/source-upload/toxics", {"name": "hold-capture-response", "type": "latency",
        "stream": "downstream", "attributes": {"latency": 60000, "jitter": 0}})
    held, acl = True, None
    try:
        with ThreadPoolExecutor(max_workers=1) as executor:
            response = executor.submit(run.request, "POST", f"/roots/{root}/versions", capture_request,
                key=state["key"], statuses=(403,))
            try:
                # Observe the real S3 write through the direct inspection
                # endpoint while its response to the API is held by TCP proxy.
                prefix = f"sources/{state['org']}/{root}/manifests/"
                run.eventually("capture manifest persisted before catalog acceptance",
                    lambda: run.s3.list_objects_v2(Bucket=run.BUCKET, Prefix=prefix).get("Contents"), 30)
                assert not response.done(), "capture escaped the network fault before revocation"
                acl = run.request("POST", f"/roots/{root}/acls", {"path_prefix": "/protected/",
                    "grant_to": "user:" + owner, "permission": "none"}, key=acl_key, statuses=(201,))
            finally:
                run.fault("DELETE", toxic)
                held = False
            response.result(timeout=90)
        assert not run.sql("SELECT id FROM file_catalog WHERE root_id=%s", (root,)), "revoked capture reached the catalog"
        assert not run.sql("SELECT path FROM file_content_proofs WHERE root_id=%s", (root,)), "revoked capture issued a content proof"
        run.request("POST", f"/roots/{root}/read", {"path": "protected/measurements.txt",
            "lines": {"start": 1, "end": 1}}, key=state["key"], statuses=(404,))
    finally:
        if held:
            run.fault("DELETE", toxic)
        if acl is not None:
            run.request("DELETE", f"/roots/{root}/acls/{acl['id']}", key=acl_key)
    print("A folder deny committed during the manifest PUT prevented catalog/proof acceptance and public reads.")


def check_receipt_pruning(state):
    directory = Path("/state/receipt-retention")
    directory.mkdir()
    path = directory / "measurements.txt"
    path.write_text("Orchid observatory measurement 0.\n")
    root = run.new_root(state, "receipt retention", directory, True)
    capture(state, directory, root)
    cache = local_spools(root)
    first_receipt, = (cache / "completed").iterdir()
    first = run.assert_source_retained(run.catalog(state, root)[path.name])
    for ordinal in range(1, 70):
        with path.open("a") as output:
            output.write(f"Orchid observatory measurement {ordinal}.\n")
        capture(state, directory, root)
    receipts = list((cache / "completed").iterdir())
    assert len(receipts) == 64 and not first_receipt.exists(), "accepted receipt history was not bounded"
    assert all((receipt / "journal.json").is_file() for receipt in receipts)
    assert not list((cache / "completed").glob("*/pack-*")), "pruning retained accepted source bytes"
    assert not list((cache / "pending").iterdir()), "successful capture left pending journals"
    file = run.wait_indexed(state, root)[path.name]
    latest = run.assert_source_retained(file)
    assert latest["extents"][:len(first["extents"])] == first["extents"], "receipt pruning broke old-pack append reuse"
    assert file["content_hash"] == "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()
    result = run.request("POST", f"/roots/{root}/read",
        {"path": path.name, "lines": {"start": 1, "end": 70}}, key=state["key"])
    assert [line["content"] for line in result["lines"]] == path.read_text().splitlines()
    print("Seventy CLI captures retained exactly 64 receipts, removed accepted pack bytes, and preserved earlier S3 extents and public reads.")


def check_obsolete_artifacts(state):
    directory = Path("/state/artifact-retention")
    directory.mkdir()
    path = directory / "measurements.txt"
    path.write_text("Orchid observatory telescope.\n")
    root = run.new_root(state, "artifact retention", directory, True)
    capture(state, directory, root)
    first = run.wait_indexed(state, root)[path.name]
    original = run.assert_source_retained(first)
    old, = run.sql("""SELECT e.id,e.chunks_ref FROM file_extractions e
        JOIN file_work w ON w.extraction_id=e.id AND w.stage='index' WHERE e.version_id=%s""", (first["version_id"],))
    with path.open("a") as output:
        output.write("Violet telescope measurements.\n")
    capture(state, directory, root)
    current = run.wait_indexed(state, root)[path.name]
    live, = run.sql("""SELECT e.id,e.chunks_ref FROM file_extractions e
        JOIN file_work w ON w.extraction_id=e.id AND w.stage='index' WHERE e.version_id=%s""", (current["version_id"],))
    def deleted():
        row, = run.sql("SELECT artifacts_retired_at,artifacts_deleted_at FROM file_extractions WHERE id=%s", (old["id"],))
        return row["artifacts_retired_at"] is not None and row["artifacts_deleted_at"] is not None
    run.eventually("scheduled cleanup of the superseded chunk artifacts", deleted, 300)
    for ref in (old["chunks_ref"],):
        try:
            run.s3.head_object(Bucket=run.BUCKET, Key=ref)
        except run.s3.exceptions.ClientError as error:
            assert error.response["ResponseMetadata"]["HTTPStatusCode"] == 404
        else:
            raise AssertionError("obsolete artifact still exists")
    row, = run.sql("SELECT artifacts_retired_at FROM file_extractions WHERE id=%s", (live["id"],))
    assert row["artifacts_retired_at"] is None
    for ref in (live["chunks_ref"],):
        run.s3.head_object(Bucket=run.BUCKET, Key=ref)
    # Old source bytes may still be live through append reuse. Chunk retirement
    # must not remove those packs, manifests, or their catalog/proof metadata.
    assert run.assert_source_retained(first) == original
    assert run.assert_source_retained(current)["extents"][:len(original["extents"])] == original["extents"]
    with path.open("a") as output:
        output.write("Sapphire telescope measurements.\n")
    capture(state, directory, root)
    newest = run.wait_indexed(state, root)[path.name]
    assert run.assert_source_retained(newest)["extents"][:len(original["extents"])] == original["extents"]
    result = run.request("POST", f"/roots/{root}/read", {"path": path.name, "lines": {"start": 1, "end": 3}}, key=state["key"])
    assert [line["content"] for line in result["lines"]] == path.read_text().splitlines()
    print("Obsolete chunks were removed after real retention elapsed; current artifacts, historical sources, append reuse and public reads remained valid.")


def verify():
    from source_retention import begin_retention, check_packed_isolation, finish_retention
    from capture_permissions import check_capture_revocations
    state = json.loads(run.STATE.read_text()) if run.STATE.exists() else run.provision()
    begin_retention(state)
    check_local_retention(state)
    check_unaccepted_retention(state)
    check_obsolete_artifacts(state)
    check_receipt_pruning(state)
    check_tenant_uploads(state)
    check_capture_acl_race(state)
    check_capture_revocations(state)
    check_packed_isolation(state)
    finish_retention(state)
