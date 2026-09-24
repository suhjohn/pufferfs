"""Page draining and durable continuation through public capture/deletion APIs."""

from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import sys
import urllib.request
import uuid

from concurrent_uploads import relay
from source_retention import upload
import run


def prefix(state, root):
    return f"sources/{state['org']}/{root}/"


def source_target(root):
    rows = run.sql("""SELECT last_checked_at IS NOT NULL AS checked,due_at<=NOW() AS due,
        due_at>NOW()+INTERVAL '30 seconds' AS backed_off FROM root_cleanup_targets
        WHERE root_id=%s AND kind='prefix' AND target LIKE 'sources/%%'""", (root,))
    assert len(rows) == 1
    return rows[0]


def prepare():
    state = run.provision()
    directory = Path("/state/cleanup-pages")
    directory.mkdir()
    failed = run.new_root(state, "Storage outage during cleanup", directory, True)
    healthy = run.new_root(state, "Multi-page cleanup", directory, True)
    state["cleanup_pages"] = {"failed": failed, "healthy": healthy}
    run.save(state)
    with ThreadPoolExecutor(max_workers=8) as pool:
        list(pool.map(lambda i: upload(state, healthy, f"Synthetic retained pack {i}.\n".encode()), range(1005)))
        list(pool.map(lambda _: run.request("POST", f"/roots/{healthy}/sources/multipart/init",
            {"request_id": str(uuid.uuid4()), "size": 1}, key=state["key"]), range(101)))
    objects = sum(len(page.get("Contents", [])) for page in run.s3.get_paginator("list_objects_v2").paginate(
        Bucket=run.BUCKET, Prefix=prefix(state, healthy)))
    assert objects == 1005
    assert len(run.s3.list_multipart_uploads(Bucket=run.BUCKET, Prefix=prefix(state, healthy))["Uploads"]) == 101
    # API deletion durably records the intent before a real S3 boundary fails.
    relay("POST", "/fail-listings", {"prefixes": [prefix(state, failed), prefix(state, healthy)]})
    for root in (failed, healthy):
        run.request("DELETE", f"/roots/{root}", key=state["key"], statuses=(500,))
        assert source_target(root)["due"]
        assert run.sql("SELECT deleting_at IS NOT NULL AS deleting FROM roots WHERE id=%s", (root,))[0]["deleting"]
        run.request("POST", f"/roots/{root}/sources/init", {"size": 1}, key=state["key"], statuses=(409,))
    relay("POST", "/fail-listings", {"prefixes": [prefix(state, failed)]})
    print("1005 immutable objects and 101 multipart sessions await durable cleanup after API storage failures.", flush=True)


def partial():
    state = json.loads(run.STATE.read_text())
    healthy, failed = (state["cleanup_pages"][k] for k in ("healthy", "failed"))
    owned_prefix = prefix(state, healthy)
    def progressed():
        uploads = run.s3.list_multipart_uploads(Bucket=run.BUCKET, Prefix=owned_prefix).get("Uploads", [])
        target = source_target(healthy)
        return uploads if 0 < len(uploads) < 101 and target["due"] and not target["checked"] else None
    uploads = run.eventually("bounded partial cleanup with prompt durable continuation", progressed, 90)
    assert not run.s3.list_objects_v2(Bucket=run.BUCKET, Prefix=owned_prefix).get("Contents")
    deletes = [e for e in relay("GET", "/status")["events"] if e.get("operation") == "delete"
        and e.get("status") == 200 and any(key.startswith(owned_prefix) for key in e["keys"])]
    assert sorted(len(e["keys"]) for e in deletes) == [5, 1000]
    assert source_target(failed)["backed_off"] and not source_target(failed)["checked"]
    print(f"One pass drained both object pages and left {len(uploads)} multipart sessions due immediately; the failed root did not block progress.", flush=True)


def recovered():
    state = json.loads(run.STATE.read_text())
    root = state["cleanup_pages"]["healthy"]
    run.eventually("cleanup continuation after worker restart", lambda: source_target(root)["checked"], 90)
    assert not run.s3.list_objects_v2(Bucket=run.BUCKET, Prefix=prefix(state, root)).get("Contents")
    assert not run.s3.list_multipart_uploads(Bucket=run.BUCKET, Prefix=prefix(state, root)).get("Uploads")
    assert not source_target(state["cleanup_pages"]["failed"])["checked"]
    relay("POST", "/fail-listings", {"prefixes": []})
    print("Restarted worker resumed partial work without a five-minute delay; objects and multipart sessions are gone.", flush=True)


if __name__ == "__main__":
    {"prepare": prepare, "partial": partial, "recovered": recovered,
     "release": lambda: relay("POST", "/fail-listings", {"prefixes": []})}[sys.argv[1]]()
