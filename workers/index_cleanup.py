"""Bounded, recurring cleanup below the published version/revision cutoff.

A captured head is NOT a cleanup cutoff: its predecessor may still be visible.
No current/future version or revision matches, even if publication advances
while a delete is in flight. Rechecking also catches late stale upserts. Source
objects and extracted chunks have separate retention; this removes index rows.
"""

from concurrent.futures import ThreadPoolExecutor
import time

from file_runtime import database

MAX_FILES = 1000
MAX_CONCURRENCY = 8


def cleanup_record(file):
    sequence = file["sequence"]
    if type(sequence) is not int or sequence < 1 or type(file["version_deleted"]) is not bool:
        raise ValueError("invalid indexed cleanup cutoff")
    older = [["version_sequence", "Lte" if file["version_deleted"] else "Lt", sequence]]
    if not file["version_deleted"]:
        if type(file["extraction_sequence"]) is not int or file["extraction_sequence"] < 1:
            raise ValueError("invalid indexed extraction cutoff")
        older.append(["And", [["version_sequence", "Eq", sequence],
                              ["extraction_id", "NotEq", file["indexed_extraction_id"]],
                              ["extraction_sequence", "Lt", file["extraction_sequence"]]]])
    return {"namespace": file["namespace"], "write": {
        "delete_by_filter": ["And", [
            ["root_id", "Eq", file["root_id"]],
            ["file_path", "Eq", file["file_path"]],
            ["file_id", "Eq", file["file_id"]],
            ["Or", older],
        ]], "delete_by_filter_allow_partial": True,
    }}


def cleanup_index(apply_write, *, connect=database, limit=MAX_FILES,
                  concurrency=MAX_CONCURRENCY, time_budget=90, clock=time.monotonic):
    if (type(limit) is not int or not 1 <= limit <= MAX_FILES
            or type(concurrency) is not int or not 1 <= concurrency <= MAX_CONCURRENCY
            or not 0 < time_budget <= 120):
        raise ValueError("invalid cleanup bounds")
    deadline = clock() + time_budget
    result = {"checked": 0, "partial": 0, "failed": 0}
    with connect() as conn:
        files = conn.execute("""SELECT f.id AS file_id,f.root_id,f.path AS file_path,
                f.indexed_version_id,f.indexed_extraction_id,
                e.sequence AS extraction_sequence,
                v.sequence,v.deleted AS version_deleted,r.org_id,r.vector_disabled,n.namespace
            FROM file_catalog f JOIN file_versions v ON v.id=f.indexed_version_id
            JOIN file_extractions e ON e.id=f.indexed_extraction_id
            JOIN roots r ON r.id=f.root_id
            JOIN root_index_namespaces n ON n.root_id=r.id AND n.org_id=r.org_id AND n.retired_at IS NULL
            WHERE f.indexed_version_id IS NOT NULL
                AND f.index_cleanup_due_at<=NOW() AND r.deleting_at IS NULL
            ORDER BY f.index_cleanup_due_at,f.id LIMIT %s
            FOR UPDATE OF f SKIP LOCKED""", (limit,)).fetchall()
        # A bounded retry delay also rotates malformed/failed files out of the
        # first page. No transaction remains open over S3 or index requests.
        if files:
            conn.execute("UPDATE file_catalog SET index_cleanup_due_at=NOW()+INTERVAL '5 minutes' WHERE id=ANY(%s)",
                         ([file["file_id"] for file in files],))

    pending = [(file, cleanup_record(file)) for file in files]

    # Only the index IO runs concurrently. The cutoff is a snapshot of durable publication state,
    # validated first; no thread holds a database connection or mutates the
    # captured cleanup data. The local task list is bounded by MAX_FILES.
    def remove_rows(item):
        file, record = item
        if clock() >= deadline:
            return file, None
        try:
            remaining = apply_write(record["namespace"], record["write"], file["vector_disabled"])
            if remaining is True:
                return file, "partial"
            if remaining is not False and remaining is not None:
                raise ValueError("invalid cleanup progress response")
            return file, "checked"
        except Exception:
            return file, "failed"

    if pending:
        succeeded = []
        with ThreadPoolExecutor(max_workers=concurrency) as executor:
            for file, status in executor.map(remove_rows, pending):
                if status == "checked":
                    succeeded.append(file)
                elif status:
                    result[status] += 1
        if succeeded:
            try:
                with connect() as conn:
                    conn.execute("""UPDATE file_catalog f SET index_cleanup_due_at=NOW()+INTERVAL '1 day'
                        FROM unnest(%s::text[],%s::text[],%s::text[]) AS c(id,version,extraction)
                        WHERE f.id=c.id AND f.indexed_version_id=c.version
                            AND f.indexed_extraction_id IS NOT DISTINCT FROM c.extraction""",
                        ([f["file_id"] for f in succeeded], [f["indexed_version_id"] for f in succeeded],
                         [f["indexed_extraction_id"] for f in succeeded]))
                result["checked"] += len(succeeded)
            except Exception:
                # Deletions may have committed. Keep the durable cutoff
                # and retry delay; never report durable acknowledgment.
                result["failed"] += len(succeeded)
    return result
