"""Retire obsolete extraction outputs without expiring sources or valid retries."""

import os
import time

from file_runtime import database
from root_cleanup import clear_prefix


def cleanup_obsolete_extractions(s3, bucket, *, connect=database, limit=5, time_budget=15):
    retention = int(os.environ.get("PUFFERFS_OBSOLETE_ARTIFACT_RETENTION_SECONDS", 30 * 86400))
    if retention < 60 or not 1 <= limit <= 25 or not 0 < time_budget <= 30:
        raise ValueError("invalid obsolete artifact retention bounds")
    deadline = time.monotonic() + time_budget
    with connect() as conn:
        # Complete/superseded work cannot be claimed again. A current captured
        # or indexed version is always pinned, including its older revisions.
        # Keep failed/pending/running/provider-waiting work regardless of age.
        retired = conn.execute("""WITH candidates AS (
            SELECT e.id FROM file_extractions e
            JOIN file_versions v ON v.id=e.version_id
            JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
            WHERE e.artifacts_retired_at IS NULL AND e.status IN ('complete','superseded')
              AND e.updated_at<NOW()-make_interval(secs=>%s) AND r.deleting_at IS NULL
              AND v.id IS DISTINCT FROM f.captured_version_id
              AND v.id IS DISTINCT FROM f.indexed_version_id
              AND NOT EXISTS (SELECT 1 FROM file_work w WHERE w.extraction_id=e.id
                  AND (w.status NOT IN ('complete','superseded') OR w.updated_at>=NOW()-make_interval(secs=>%s)))
              AND NOT EXISTS (SELECT 1 FROM provider_batches b
                  WHERE b.extraction_id=e.id AND b.status IN ('preparing','submitted','retry'))
            ORDER BY e.updated_at,e.id LIMIT %s FOR UPDATE OF e SKIP LOCKED)
            UPDATE file_extractions e SET artifacts_retired_at=NOW(),artifact_cleanup_due_at=NOW()
            FROM candidates c WHERE e.id=c.id RETURNING e.id""", (retention, retention, limit)).fetchall()
        pending = conn.execute("""SELECT e.id AS extraction_id,f.root_id,r.org_id
            FROM file_extractions e JOIN file_versions v ON v.id=e.version_id
            JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
            WHERE e.artifacts_retired_at IS NOT NULL AND e.artifact_cleanup_due_at<=NOW()
              AND r.deleting_at IS NULL
            ORDER BY e.artifact_cleanup_due_at,e.id LIMIT %s FOR UPDATE OF e SKIP LOCKED""", (limit,)).fetchall()
        if pending:
            conn.execute("""UPDATE file_extractions SET artifact_cleanup_due_at=NOW()+INTERVAL '5 minutes'
                WHERE id=ANY(%s)""", ([row["extraction_id"] for row in pending],))
    result = {"retired": len(retired), "deleted": 0, "partial": 0, "failed": 0}
    for target in pending:
        if time.monotonic() >= deadline:
            break
        try:
            partial = False
            for kind in ("extractions", "mutations"):
                if time.monotonic() >= deadline:
                    partial = True
                    break
                prefix = f"{kind}/{target['org_id']}/{target['root_id']}/{target['extraction_id']}/"
                partial = clear_prefix(s3, bucket, dict(target, target=prefix), deadline=deadline) or partial
            if partial:
                result["partial"] += 1
                continue
            with connect() as conn:
                conn.execute("""UPDATE file_extractions SET artifacts_deleted_at=COALESCE(artifacts_deleted_at,NOW()),
                    artifact_cleanup_due_at=NOW()+INTERVAL '1 day'
                    WHERE id=%s AND artifacts_retired_at IS NOT NULL""", (target["extraction_id"],))
            result["deleted"] += 1
        except Exception:
            result["failed"] += 1
    return result
