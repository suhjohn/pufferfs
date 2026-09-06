"""Replay immutable-version rows, acknowledge writes, then publish catalog head.

Rows from different extractions never overwrite each other. Search/read MUST
filter against the catalog indexed extraction. Tombstones delete only rows at
or below their version sequence; superseded/late-write cleanup is separate.
A lease cannot fence a network write already in flight, so safety must not
depend on cancelling it or on a pre-write ownership check alone.
"""

from file_runtime import database, heartbeat, stable_id
from index_prepare import MUTATION_RECORD_BYTES
from index_routing import namespace_for_path
from index_mutations import deletion_mutation
from source_io import iter_chunks


def publish_mutations(job, apply_write, s3, bucket, *, connect=database):
    with connect() as conn:
        state = conn.execute("SELECT * FROM file_work WHERE id=%s", (job["id"],)).fetchone()
        namespaces = conn.execute("SELECT namespace,shard_index,shard_count FROM root_index_namespaces WHERE root_id=%s AND org_id=%s AND retired_at IS NULL", (job["root_id"], job["org_id"])).fetchall()
    namespace = namespace_for_path(namespaces, job["file_path"])
    if not state or not state["mutation_ref"] or state["mutation_batch_count"] is None:
        raise ValueError("no durable mutation artifact")
    acknowledged = state["acknowledged_batches"]
    total = state["mutation_batch_count"]
    if not 0 <= acknowledged <= total:
        raise ValueError("invalid mutation progress")
    seen = 0
    records = iter_chunks(s3, bucket, state["mutation_ref"], max_record_bytes=MUTATION_RECORD_BYTES)
    try:
        for index, record in enumerate(records):
            seen += 1
            if seen > total:
                raise ValueError("mutation artifact has unexpected extra batches")
            if index < acknowledged:
                continue
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
                        or ("extraction_sequence" in row and (type(row["extraction_sequence"]) is not int
                            or row["extraction_sequence"] != job["extraction_sequence"]))):
                    raise ValueError("mutation contains foreign rows")
            heartbeat(job, connect=connect)
            apply_write(record["namespace"], mutation)
            # A lost response replays the same immutable upserts. Advance only
            # after successful external acknowledgment, under current ownership.
            with connect() as conn:
                updated = conn.execute("""UPDATE file_work SET acknowledged_batches=%s,updated_at=NOW()
                    WHERE id=%s AND attempt_token=%s AND status='running' AND lease_until>NOW()
                    AND mutation_ref=%s AND acknowledged_batches=%s RETURNING id""",
                    (index + 1, job["id"], job["attempt_token"], state["mutation_ref"], index)).fetchone()
                if updated is None:
                    raise RuntimeError("index acknowledgment lost ownership")
    finally:
        records.close()
    if seen != total:
        raise ValueError("mutation artifact batch count mismatch")
    with connect() as conn:
        root = conn.execute("SELECT deleting_at FROM roots WHERE id=%s FOR UPDATE", (job["root_id"],)).fetchone()
        file = conn.execute("""SELECT f.*,e.sequence AS indexed_extraction_sequence
            FROM file_catalog f LEFT JOIN file_extractions e ON e.id=f.indexed_extraction_id
            WHERE f.id=%s FOR UPDATE OF f""", (job["file_id"],)).fetchone()
        current = conn.execute("""SELECT * FROM file_work WHERE id=%s AND attempt_token=%s
            AND status='running' AND lease_until>NOW() FOR UPDATE""", (job["id"], job["attempt_token"])).fetchone()
        if current is None:
            raise RuntimeError("index publication lost ownership")
        if current["acknowledged_batches"] != total or current["mutation_ref"] != state["mutation_ref"]:
            raise RuntimeError("index publication is incomplete")
        stale_revision = (file is not None and file["indexed_version_id"] == job["version_id"]
                          and file["indexed_extraction_sequence"] is not None
                          and file["indexed_extraction_sequence"] > job["extraction_sequence"])
        if root is None or root["deleting_at"] or file is None or file["captured_version_id"] != job["version_id"] or stale_revision:
            conn.execute("UPDATE file_work SET status='superseded',lease_until=NULL,updated_at=NOW() WHERE id=%s", (job["id"],))
            return "superseded"
        conn.execute("""UPDATE file_catalog SET indexed_version_id=%s,indexed_extraction_id=%s,
            index_cleanup_ref='',index_cleanup_record=0,index_cleanup_due_at=NOW(),
            updated_at=NOW() WHERE id=%s""", (job["version_id"], job["extraction_id"], job["file_id"]))
        conn.execute("UPDATE file_work SET status='complete',lease_until=NULL,updated_at=NOW() WHERE id=%s", (job["id"],))
    return "complete"
