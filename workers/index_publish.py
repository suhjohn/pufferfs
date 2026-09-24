"""Publish a complete extraction only while its captured version and lease are current."""

from file_runtime import database
from worker_metrics import count


def publish_head(job, *, connect=database):
    with connect() as conn:
        # Keep the root lock in a separate statement: after waiting for a
        # capture/publication, the next statement must see its committed head.
        root = conn.execute("SELECT deleting_at FROM roots WHERE id=%s FOR UPDATE", (job["root_id"],)).fetchone()
        # Materialization preserves root -> file -> work lock order. Return
        # only publication inputs, not the entire catalog/work rows.
        current = conn.execute("""WITH file AS MATERIALIZED (
                SELECT f.captured_version_id,f.indexed_version_id,e.sequence AS indexed_extraction_sequence
                FROM file_catalog f LEFT JOIN file_extractions e ON e.id=f.indexed_extraction_id
                WHERE f.id=%s FOR UPDATE OF f
            ) SELECT file.*
            FROM file CROSS JOIN file_work w JOIN file_extractions prepared ON prepared.id=w.extraction_id
            WHERE w.id=%s AND w.attempt_token=%s
                AND w.index_cursor=prepared.chunk_count
                AND prepared.status='complete' AND (prepared.row_format=1 OR prepared.source_verified)
                AND w.status='running' AND w.lease_until>NOW() FOR UPDATE OF w""",
            (job["file_id"], job["id"], job["attempt_token"])).fetchone()
        if current is None:
            raise RuntimeError("index publication lost ownership")
        stale_revision = (current["indexed_version_id"] == job["version_id"]
                          and current["indexed_extraction_sequence"] is not None
                          and current["indexed_extraction_sequence"] > job["extraction_sequence"])
        if root is None or root["deleting_at"] or current["captured_version_id"] != job["version_id"] or stale_revision:
            conn.execute("""UPDATE file_work SET status='superseded',
                lease_until=NULL,updated_at=NOW() WHERE id=%s""", (job["id"],))
            return "superseded"
        count("db_write_statements")  # The first-token counter excludes CTEs.
        conn.execute("""WITH published AS (
                UPDATE file_catalog SET indexed_version_id=%s,indexed_extraction_id=%s,
                    index_cleanup_due_at=NOW(),
                    updated_at=NOW() WHERE id=%s RETURNING id
            ) UPDATE file_work SET status='complete',lease_until=NULL,updated_at=NOW()
                WHERE id=%s AND EXISTS (SELECT 1 FROM published)""",
            (job["version_id"], job["extraction_id"], job["file_id"], job["id"]))
    return "complete"
