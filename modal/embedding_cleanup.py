"""Retire cold vector-cache packs; published mutations already contain vectors.

Readers/writers hold a pack row lock through S3 IO. Retirement removes cache
locators atomically and permanently fences that object identity. No publication,
source, chunk, work lease or retry record is removed. A cache miss can be encoded
again; an existing mutation can still be replayed without the cache.
"""

import os
import re

from file_runtime import database


def cleanup_embeddings(s3, bucket, *, connect=database, limit=100):
    retention = int(os.environ.get("PUFFERFS_EMBEDDING_CACHE_RETENTION_SECONDS", 30 * 86400))
    if retention < 60 or not 1 <= limit <= 100:
        raise ValueError("embedding retention must be at least 60 seconds; cleanup limit must be 1..100")
    with connect() as conn:
        cold = conn.execute("""SELECT object_key FROM embedding_packs
            WHERE retired_at IS NULL AND last_used_at < NOW() - make_interval(secs=>%s)
            ORDER BY last_used_at,object_key LIMIT %s FOR UPDATE SKIP LOCKED""", (retention, limit)).fetchall()
        keys = [row["object_key"] for row in cold]
        if keys:
            conn.execute("""UPDATE embedding_packs SET retired_at=NOW(),cleanup_due_at=NOW(),content_hashes='{}'
                WHERE object_key=ANY(%s)""", (keys,))
        pending = conn.execute("""SELECT object_key,org_id FROM embedding_packs
            WHERE retired_at IS NOT NULL AND cleanup_due_at<=NOW()
            ORDER BY cleanup_due_at,object_key LIMIT %s FOR UPDATE SKIP LOCKED""", (limit,)).fetchall()
        for row in pending:
            # Reject unexpected targets before any deletion.
            prefix = f"embeddings/{row['org_id']}/"
            if not re.fullmatch(re.escape(prefix) + r"[0-9a-f]{64}/[0-9a-f]{64}-[0-9a-f]{32}\.f32", row["object_key"]):
                raise ValueError("invalid embedding cleanup target")
        pending_keys = [row["object_key"] for row in pending]
        if pending_keys:
            conn.execute("""UPDATE embedding_packs SET cleanup_due_at=NOW()+INTERVAL '5 minutes'
                WHERE object_key=ANY(%s)""", (pending_keys,))
    result = {"retired": len(keys), "deleted": 0, "failed": 0}
    if not pending_keys:
        return result
    # One bounded batch delete, not one request per vector or cache locator.
    response = s3.delete_objects(Bucket=bucket, Delete={"Objects": [{"Key": key} for key in pending_keys]})
    failed = {item["Key"] for item in response.get("Errors", [])}
    deleted = {item["Key"] for item in response.get("Deleted", [])} - failed
    if not deleted.issubset(pending_keys) or not failed.issubset(pending_keys):
        raise ValueError("foreign embedding deletion response")
    if deleted:
        with connect() as conn:
            conn.execute("""UPDATE embedding_packs SET deleted_at=COALESCE(deleted_at,NOW()),
                cleanup_due_at=NOW()+INTERVAL '1 day'
                WHERE object_key=ANY(%s) AND retired_at IS NOT NULL""", (list(deleted),))
    result.update(deleted=len(deleted), failed=len(pending_keys) - len(deleted))
    return result
