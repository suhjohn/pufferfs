"""Batch upload cleanup: exact identities and outcomes live in S3, not SQL rows."""

from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import uuid

from file_runtime import database
from provider_manifests import read_manifest, write_manifest

TERMINAL = {"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED"}


def claim_cleanup(*, connect=database):
    with connect() as conn:
        return conn.execute("""WITH due AS (
            SELECT id FROM provider_batches WHERE NOT cleanup_complete
                AND status IN ('complete','failed') AND cleanup_after<=NOW()
                AND (lease_until IS NULL OR lease_until<=NOW())
                AND (submission_started_at IS NULL OR provider_job_id IS NOT NULL)
            ORDER BY cleanup_after,id LIMIT 1 FOR UPDATE SKIP LOCKED
        ) UPDATE provider_batches b SET lease_token=%s,lease_until=NOW()+INTERVAL '5 minutes',
            cleanup_after=NOW()+INTERVAL '5 minutes'
            FROM due WHERE b.id=due.id RETURNING b.*""", (uuid.uuid4().hex,)).fetchone()


def cleanup_provider_files(batch, client, s3, bucket, *, connect=database):
    checkpoint = (read_manifest(s3, bucket, batch, batch["cleanup_ref"], "cleanup")
                  if batch["cleanup_ref"] else None)
    cursor = checkpoint["next_input_ref"] if checkpoint else batch["input_ref"]
    if not cursor:
        raise ValueError("provider cleanup cursor is already complete")
    manifest = read_manifest(s3, bucket, batch, cursor, "input")
    files = (checkpoint["files"] if checkpoint and checkpoint["input_ref"] == cursor
             else [dict(item, deleted_at=None, expired_at=None, error="") for item in manifest["uploads"]])
    if ([item["file_id"] for item in files] != [item["file_id"] for item in manifest["uploads"]]
            or len(files) > 65):
        raise ValueError("provider cleanup checkpoint identities changed")

    def remove(file):
        if file["deleted_at"] or file["expired_at"]:
            return file
        now = datetime.now(timezone.utc)
        if datetime.fromisoformat(file["expires_at"]) <= now:
            return dict(file, expired_at=now.isoformat())
        try:
            try:
                client.files.delete(name=file["file_id"])
            except Exception as error:
                if getattr(error, "code", None) != 404:
                    raise  # A 403 is not deletion evidence.
            return dict(file, deleted_at=datetime.now(timezone.utc).isoformat(), error="")
        except Exception as error:
            code = getattr(error, "code", None)
            return dict(file, error=f"{type(error).__name__} status={code if type(code) is int else 'unknown'}")

    # All 65 outcomes share one S3 checkpoint and one database CAS. If a
    # deletion's response/checkpoint is lost, retry deletion or await expiry.
    with ThreadPoolExecutor(max_workers=8) as pool:
        files = list(pool.map(remove, files))
    settled = all(item["deleted_at"] or item["expired_at"] for item in files)
    next_ref = manifest["previous"] if settled else cursor
    ref = write_manifest(s3, bucket, batch, "cleanup", {"input_ref": cursor, "next_input_ref": next_ref,
        "files": files, "previous": batch["cleanup_ref"]})
    with connect() as conn:
        updated = conn.execute("""UPDATE provider_batches SET cleanup_ref=%s,cleanup_complete=%s,
            cleanup_after=NOW()+make_interval(secs=>%s),updated_at=NOW()
            WHERE id=%s AND lease_token=%s AND lease_until>NOW() AND cleanup_ref=%s
              AND input_ref=%s AND status IN ('complete','failed') RETURNING *""",
            (ref, not next_ref, 0 if settled and next_ref else 300, batch["id"], batch["lease_token"],
             batch["cleanup_ref"], batch["input_ref"])).fetchone()
        if updated is None:
            raise RuntimeError("provider cleanup lost ownership")
    batch.update(updated)
    return {"deleted": sum(bool(f["deleted_at"]) for f in files),
            "expired": sum(bool(f["expired_at"]) for f in files),
            "pending": sum(not f["deleted_at"] and not f["expired_at"] for f in files)}
