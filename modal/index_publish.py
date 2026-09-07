"""Replay immutable-version rows, acknowledge writes, then publish catalog head.

Rows from different extractions never overwrite each other. Search/read MUST
filter against the catalog indexed extraction. Tombstones delete only rows at
or below their version sequence; superseded/late-write cleanup is separate.
A lease cannot fence a network write already in flight, so safety must not
depend on cancelling it or on a pre-write ownership check alone.
"""

from file_runtime import database, stable_id
from index_prepare import MUTATION_RECORD_BYTES
from index_mutations import deletion_mutation
from source_io import iter_chunks
from worker_metrics import count


def publish_mutations(job, namespace, mutation_ref, total, apply_write, s3, bucket, *, connect=database):
    if not mutation_ref or total is None:
        raise ValueError("no durable mutation artifact")
    # Publication records acknowledgment atomically. Interrupted attempts replay
    # the entire immutable artifact; deterministic row IDs make replay idempotent.
    seen = 0
    records = iter_chunks(s3, bucket, mutation_ref, max_record_bytes=MUTATION_RECORD_BYTES)
    try:
        for record in records:
            seen += 1
            if seen > total:
                raise ValueError("mutation artifact has unexpected extra batches")
            # Permit only this extraction's rows or the exact sequence-bounded
            # tombstone. Arbitrary delete filters cannot enter replay artifacts.
            mutation = record["write"]
            if record["namespace"] != namespace:
                raise ValueError("mutation targets a foreign namespace")
            if job["version_deleted"]:
                if mutation != deletion_mutation(job):
                    raise ValueError("invalid version-isolated deletion")
            elif set(mutation) != {"upsert_rows"}:
                raise ValueError("invalid version-isolated mutation")
            for row in mutation.get("upsert_rows", []):
                expected = stable_id(job["org_id"], job["root_id"], job["file_id"], job["extraction_id"], str(row["chunk_index"]))
                if (row["id"] != expected or row["extraction_id"] != job["extraction_id"]
                        or row["version_id"] != job["version_id"] or row["file_id"] != job["file_id"]
                        or row["root_id"] != job["root_id"]
                        or type(row["version_sequence"]) is not int or row["version_sequence"] != job["sequence"]
                        or type(row["extraction_sequence"]) is not int
                        or row["extraction_sequence"] != job["extraction_sequence"]):
                    raise ValueError("mutation contains foreign rows")
            apply_write(record["namespace"], mutation)
    finally:
        records.close()
    if seen != total:
        raise ValueError("mutation artifact batch count mismatch")
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
            ) SELECT file.*,w.mutation_ref,w.mutation_batch_count
            FROM file CROSS JOIN file_work w WHERE w.id=%s AND w.attempt_token=%s
                AND w.status='running' AND w.lease_until>NOW() FOR UPDATE OF w""",
            (job["file_id"], job["id"], job["attempt_token"])).fetchone()
        if current is None:
            raise RuntimeError("index publication lost ownership")
        if current["mutation_ref"] != mutation_ref or current["mutation_batch_count"] != total:
            raise RuntimeError("index publication artifact changed")
        stale_revision = (current["indexed_version_id"] == job["version_id"]
                          and current["indexed_extraction_sequence"] is not None
                          and current["indexed_extraction_sequence"] > job["extraction_sequence"])
        if root is None or root["deleting_at"] or current["captured_version_id"] != job["version_id"] or stale_revision:
            conn.execute("""UPDATE file_work SET status='superseded',acknowledged_batches=%s,
                lease_until=NULL,updated_at=NOW() WHERE id=%s""", (total, job["id"]))
            return "superseded"
        count("db_write_statements")  # The first-token counter excludes CTEs.
        conn.execute("""WITH published AS (
                UPDATE file_catalog SET indexed_version_id=%s,indexed_extraction_id=%s,
                    index_cleanup_ref='',index_cleanup_record=0,index_cleanup_due_at=NOW(),
                    updated_at=NOW() WHERE id=%s RETURNING id
            ) UPDATE file_work SET status='complete',acknowledged_batches=%s,lease_until=NULL,updated_at=NOW()
                WHERE id=%s AND EXISTS (SELECT 1 FROM published)""",
            (job["version_id"], job["extraction_id"], job["file_id"], total, job["id"]))
    return "complete"
