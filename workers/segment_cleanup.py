"""Retire unreferenced immutable segments before deleting their index rows.

Membership creation holds FOR SHARE on each segment. Retirement holds FOR
UPDATE, so a segment can never acquire a live reference after retirement. No
database transaction spans provider IO, and recurring deletion catches late
writes from an expired worker.
"""
import time
from concurrent.futures import ThreadPoolExecutor

from file_runtime import database


def cleanup_segments(apply_write, *, connect=database, limit=128, time_budget=15):
    if not 1 <= limit <= 1000 or not 0 < time_budget <= 30:
        raise ValueError("invalid segment cleanup bounds")
    deadline = time.monotonic() + time_budget
    with connect() as conn:
        candidates = conn.execute("""SELECT s.id FROM file_segments s
            JOIN file_catalog f ON f.id=s.file_id JOIN roots r ON r.id=f.root_id
            WHERE s.retired_at IS NULL AND r.deleting_at IS NULL
              AND f.updated_at<NOW()-INTERVAL '1 minute'
              AND NOT EXISTS (
                SELECT 1 FROM extraction_segments m
                JOIN file_extractions e ON e.id=m.extraction_id
                WHERE m.segment_id=s.id AND (
                    e.id=f.indexed_extraction_id OR e.version_id=f.captured_version_id
                    OR EXISTS (SELECT 1 FROM file_work w WHERE w.extraction_id=e.id
                        AND w.status NOT IN ('complete','superseded'))))
            ORDER BY s.id LIMIT %s FOR UPDATE OF s SKIP LOCKED""", (limit,)).fetchall()
        # Recheck references in a new statement after taking the lock. A
        # concurrent append may have committed while the lock was acquired.
        retired = []
        if candidates:
            retired = conn.execute("""UPDATE file_segments s SET retired_at=NOW(),index_cleanup_due_at=NOW()
                FROM file_catalog f WHERE s.file_id=f.id AND s.id=ANY(%s) AND s.retired_at IS NULL
                  AND NOT EXISTS (SELECT 1 FROM extraction_segments m
                    JOIN file_extractions e ON e.id=m.extraction_id
                    WHERE m.segment_id=s.id AND (e.id=f.indexed_extraction_id
                        OR e.version_id=f.captured_version_id OR EXISTS (
                            SELECT 1 FROM file_work w WHERE w.extraction_id=e.id
                                AND w.status NOT IN ('complete','superseded')))) RETURNING s.id""",
                ([row["id"] for row in candidates],)).fetchall()
        pending = conn.execute("""SELECT s.id,f.id AS file_id,f.root_id,n.namespace,r.vector_disabled
            FROM file_segments s JOIN file_catalog f ON f.id=s.file_id JOIN roots r ON r.id=f.root_id
            JOIN root_index_namespaces n ON n.root_id=r.id AND n.org_id=r.org_id AND n.retired_at IS NULL
            WHERE s.retired_at IS NOT NULL AND s.index_cleanup_due_at<=NOW() AND r.deleting_at IS NULL
            ORDER BY s.index_cleanup_due_at,s.id LIMIT %s FOR UPDATE OF s SKIP LOCKED""", (limit,)).fetchall()
        if pending:
            conn.execute("UPDATE file_segments SET index_cleanup_due_at=NOW()+INTERVAL '5 minutes' WHERE id=ANY(%s)",
                ([row["id"] for row in pending],))
    result = {"retired": len(retired), "deleted": 0, "partial": 0, "failed": 0}

    def remove(segment):
        if time.monotonic() >= deadline:
            return segment["id"], "partial"
        try:
            remaining = apply_write(segment["namespace"], {"delete_by_filter": ["And", [
                ["root_id", "Eq", segment["root_id"]], ["file_id", "Eq", segment["file_id"]],
                ["segment_id", "Eq", segment["id"]]]], "delete_by_filter_allow_partial": True}, segment["vector_disabled"])
            if remaining not in (True, False, None):
                raise ValueError("invalid segment cleanup progress")
            return segment["id"], "partial" if remaining else "deleted"
        except Exception:
            return segment["id"], "failed"

    with ThreadPoolExecutor(max_workers=8) as executor:
        outcomes = list(executor.map(remove, pending))
    with connect() as conn:
        for status, interval in (("partial", "0 seconds"), ("deleted", "1 day")):
            ids = [identity for identity, outcome in outcomes if outcome == status]
            if ids:
                conn.execute("UPDATE file_segments SET index_cleanup_due_at=NOW()+%s::interval WHERE id=ANY(%s)", (interval, ids))
        for _, status in outcomes:
            result[status] += 1
    return result
