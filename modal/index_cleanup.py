"""Bounded, recurring cleanup below the published version/revision cutoff.

A captured head is NOT a cleanup cutoff: its predecessor may still be visible.
No current/future version or revision matches, even if publication advances
while a delete is in flight. Rechecking also catches late stale upserts. Source
packs, extracted chunks and vectors are retained; this only removes index rows.
"""

from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor
import time

from file_runtime import database
from index_routing import namespace_for_path
from source_io import iter_chunks, write_chunks

MAX_FILES = 1000
MAX_CONCURRENCY = 8
RECORD_BYTES = 64 * 1024


def cleanup_record(file):
    sequence = file["sequence"]
    if type(sequence) is not int or sequence < 1 or type(file["version_deleted"]) is not bool:
        raise ValueError("invalid indexed cleanup cutoff")
    older = [["version_sequence", "Lte" if file["version_deleted"] else "Lt", sequence]]
    if not file["version_deleted"] and file["extraction_sequence"] is not None:
        if type(file["extraction_sequence"]) is not int or file["extraction_sequence"] < 1:
            raise ValueError("invalid indexed extraction cutoff")
        older.append(["And", [["version_sequence", "Eq", sequence],
                              ["extraction_id", "NotEq", file["indexed_extraction_id"]],
                              ["extraction_sequence", "NotEq", None],
                              ["extraction_sequence", "Lt", file["extraction_sequence"]]]])
    return {"namespace": file["namespace"], "write": {
        "delete_by_filter": ["And", [
            ["root_id", "Eq", file["root_id"]],
            ["file_path", "Eq", file["file_path"]],
            ["Or", [
                ["And", [["file_id", "Eq", file["file_id"]],
                         ["version_sequence", "NotEq", None], ["Or", older]]],
                ["file_id", "Eq", None],
            ]],
        ]], "delete_by_filter_allow_partial": True,
    }}


def cleanup_index(s3, bucket, apply_write, *, connect=database, limit=MAX_FILES,
                  concurrency=MAX_CONCURRENCY, time_budget=90, clock=time.monotonic):
    if (type(limit) is not int or not 1 <= limit <= MAX_FILES
            or type(concurrency) is not int or not 1 <= concurrency <= MAX_CONCURRENCY
            or not 0 < time_budget <= 120):
        raise ValueError("invalid cleanup bounds")
    deadline = clock() + time_budget
    result = {"checked": 0, "partial": 0, "failed": 0}
    with connect() as conn:
        files = conn.execute("""SELECT f.id AS file_id,f.root_id,f.path AS file_path,
                f.indexed_version_id,f.indexed_extraction_id,f.index_cleanup_ref,f.index_cleanup_record,
                e.sequence AS extraction_sequence,
                v.sequence,v.deleted AS version_deleted,r.org_id,r.vector_disabled
            FROM file_catalog f JOIN file_versions v ON v.id=f.indexed_version_id
            LEFT JOIN file_extractions e ON e.id=f.indexed_extraction_id
            JOIN roots r ON r.id=f.root_id
            WHERE f.indexed_version_id IS NOT NULL
                AND f.index_cleanup_due_at<=NOW() AND r.deleting_at IS NULL
            ORDER BY f.index_cleanup_due_at,f.id LIMIT %s
            FOR UPDATE OF f SKIP LOCKED""", (limit,)).fetchall()
        # A bounded retry delay also rotates malformed/failed files out of the
        # first page. No transaction remains open over S3 or index requests.
        if files:
            conn.execute("UPDATE file_catalog SET index_cleanup_due_at=NOW()+INTERVAL '5 minutes' WHERE id=ANY(%s)",
                         ([file["file_id"] for file in files],))

    groups = defaultdict(list)
    for file in files:
        groups[(file["org_id"], file["root_id"])].append(file)
    for (org, root), group in groups.items():
        if clock() >= deadline:
            break
        try:
            with connect() as conn:
                namespaces = conn.execute("""SELECT namespace,shard_index,shard_count
                    FROM root_index_namespaces WHERE org_id=%s AND root_id=%s AND retired_at IS NULL""", (org, root)).fetchall()
            for file in group:
                file["namespace"] = namespace_for_path(namespaces, file["file_path"])
            missing = [file for file in group if not file["index_cleanup_ref"]]
            if missing:
                ref, _ = write_chunks(s3, bucket, f"mutations/{org}/{root}/cleanup",
                                      map(cleanup_record, missing), max_record_bytes=RECORD_BYTES)
                with connect() as conn:
                    saved = conn.execute("""UPDATE file_catalog f
                        SET index_cleanup_ref=%s,index_cleanup_record=c.ordinal-1
                        FROM unnest(%s::text[],%s::text[],%s::text[]) WITH ORDINALITY
                            AS c(id,version,extraction,ordinal)
                        WHERE f.id=c.id AND f.indexed_version_id=c.version
                            AND f.indexed_extraction_id IS NOT DISTINCT FROM c.extraction AND f.index_cleanup_ref=''
                        RETURNING f.id,f.index_cleanup_record""",
                        (ref, [f["file_id"] for f in missing], [f["indexed_version_id"] for f in missing],
                         [f["indexed_extraction_id"] for f in missing])).fetchall()
                ordinals = {row["id"]: row["index_cleanup_record"] for row in saved}
                for file in missing:
                    if file["file_id"] in ordinals:
                        file["index_cleanup_ref"], file["index_cleanup_record"] = ref, ordinals[file["file_id"]]
        except Exception:
            result["failed"] += len(group)
            continue

        by_ref, pending = defaultdict(list), []
        for file in group:
            if file["index_cleanup_ref"]:
                by_ref[file["index_cleanup_ref"]].append(file)
        for ref, referenced in by_ref.items():
            if clock() >= deadline:
                return result
            # A sweep can reference a different historical pack for every file.
            # Read each pack once, retaining only this sweep's records, not up
            # to MAX_FILES whole packs of unrelated records.
            wanted = {file["index_cleanup_record"] for file in referenced}
            selected = {}
            try:
                records = iter_chunks(s3, bucket, ref, max_record_bytes=RECORD_BYTES)
                try:
                    for ordinal, record in enumerate(records):
                        if clock() >= deadline:
                            return result
                        if ordinal >= MAX_FILES:
                            raise ValueError("cleanup pack exceeds file limit")
                        if ordinal in wanted:
                            selected[ordinal] = record
                finally:
                    records.close()
            except Exception:
                result["failed"] += len(referenced)
                continue
            for file in referenced:
                try:
                    record = selected[file["index_cleanup_record"]]
                    if record != cleanup_record(file):
                        raise ValueError("cleanup artifact does not match indexed cutoff/namespace")
                    pending.append((file, record))
                except Exception:
                    result["failed"] += 1

        # Only the index IO runs concurrently. All records are durable and
        # validated first; no thread holds a database connection or mutates the
        # shared artifact data. The local task list is bounded by MAX_FILES.
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
                            FROM unnest(%s::text[],%s::text[],%s::text[],%s::text[]) AS c(id,version,extraction,ref)
                            WHERE f.id=c.id AND f.indexed_version_id=c.version
                                AND f.indexed_extraction_id IS NOT DISTINCT FROM c.extraction AND f.index_cleanup_ref=c.ref""",
                            ([f["file_id"] for f in succeeded], [f["indexed_version_id"] for f in succeeded],
                             [f["indexed_extraction_id"] for f in succeeded], [f["index_cleanup_ref"] for f in succeeded]))
                    result["checked"] += len(succeeded)
                except Exception:
                    # Deletions may have committed. Keep their replay locators
                    # and retry delay; never report durable acknowledgment.
                    result["failed"] += len(succeeded)
    return result
