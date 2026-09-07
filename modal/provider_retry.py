"""Advance a failed batch attempt; successful request results remain in S3."""

from file_runtime import database

MAX_REQUEST_ATTEMPTS = 3


def reserve_retry(batch, *, connect=database):
    with connect() as conn:
        source = conn.execute("""SELECT e.status,e.version_id,f.captured_version_id,f.deleted,r.deleting_at
            FROM file_extractions e JOIN file_versions v ON v.id=e.version_id
            JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
            WHERE e.id=%s FOR UPDATE OF r""", (batch["extraction_id"],)).fetchone()
        if source is not None and source["status"] == "pending":
            return False  # Initial transform delivery must seal its range first.
        if (source is None or source["status"] != "waiting_provider" or source["deleted"]
                or source["deleting_at"] or source["captured_version_id"] != source["version_id"]):
            conn.execute("""UPDATE provider_batches SET status='failed',error='retry no longer current'
                WHERE id=%s AND lease_token=%s AND lease_until>NOW() AND status='retry'""",
                (batch["id"], batch["lease_token"]))
            return False
        row = conn.execute("""UPDATE provider_batches SET status='preparing',attempt_count=attempt_count+1,
            provider_job_id=NULL,submission_started_at=NULL,reconciliation_cursor='',error='',updated_at=NOW()
            WHERE id=%s AND status='retry' AND attempt_count<%s AND lease_token=%s AND lease_until>NOW()
            RETURNING *""", (batch["id"], MAX_REQUEST_ATTEMPTS, batch["lease_token"])).fetchone()
        if row is None:
            raise RuntimeError("provider retry lost ownership")
    batch.update(row)
    return True
