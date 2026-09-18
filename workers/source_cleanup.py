"""Bounded source-pack retirement, fenced against capture and upload renewal.

Current heads and all nonterminal work pin sources, regardless of age. Mixed
packs remain whole while any retained version references any of their bytes.
Historical manifests/metadata remain receipts; they are not erased here.
"""

import os
import re
import time

from file_runtime import database
ELIGIBLE_VERSION = """v.source_retired_at IS NULL
    AND v.created_at<NOW()-make_interval(secs=>%s) AND r.deleting_at IS NULL
    AND v.id IS DISTINCT FROM f.captured_version_id AND v.id IS DISTINCT FROM f.indexed_version_id
    AND NOT EXISTS (SELECT 1 FROM file_extractions e WHERE e.version_id=v.id
        AND (e.status IN ('failed','waiting_provider') OR e.updated_at>=NOW()-make_interval(secs=>%s)))
    AND NOT EXISTS (SELECT 1 FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
        WHERE e.version_id=v.id AND (w.status NOT IN ('complete','superseded')
            OR w.updated_at>=NOW()-make_interval(secs=>%s)))
    AND NOT EXISTS (SELECT 1 FROM provider_batches b
        JOIN file_extractions e ON e.id=b.extraction_id WHERE e.version_id=v.id
        AND b.status IN ('preparing','submitted','retry'))"""

ELIGIBLE_PACK = """o.retired_at IS NULL AND o.authorized_until<NOW()
    AND o.created_at<NOW()-make_interval(secs=>%s) AND r.deleting_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM file_version_extents x JOIN file_versions v ON v.id=x.version_id
        WHERE x.object_key=o.object_key AND v.source_retired_at IS NULL)"""


def cleanup_source_packs(s3, bucket, *, connect=database, limit=100, time_budget=15):
    retention = int(os.environ.get("PUFFERFS_SOURCE_RETENTION_SECONDS", 30 * 86400))
    if retention < 60 or not 1 <= limit <= 100 or not 0 < time_budget <= 30:
        raise ValueError("invalid source retention bounds")
    deadline = time.monotonic() + time_budget
    result = {"versions_retired": 0, "packs_retired": 0, "deleted": 0, "failed": 0}
    with connect() as conn:
        versions = conn.execute(f"""SELECT v.id,f.root_id FROM file_versions v
            JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
            WHERE {ELIGIBLE_VERSION} ORDER BY v.created_at,v.id LIMIT %s""",
            (retention, retention, retention, limit)).fetchall()
    # Candidate discovery does not authorize deletion. Recheck in a fresh
    # statement after taking the same root lock as capture registration.
    for version in versions:
        if time.monotonic() >= deadline:
            break
        with connect() as conn:
            conn.execute("SELECT id FROM roots WHERE id=%s FOR UPDATE", (version["root_id"],)).fetchone()
            changed = conn.execute(f"""UPDATE file_versions v SET source_retired_at=NOW()
                FROM file_catalog f,roots r WHERE v.id=%s AND f.id=v.file_id AND r.id=f.root_id
                AND {ELIGIBLE_VERSION} RETURNING v.id""", (version["id"], retention, retention, retention)).fetchone()
            result["versions_retired"] += bool(changed)
    with connect() as conn:
        packs = conn.execute(f"""SELECT o.object_key,o.root_id FROM source_objects o JOIN roots r ON r.id=o.root_id
            WHERE {ELIGIBLE_PACK} ORDER BY o.created_at,o.object_key LIMIT %s""", (retention, limit)).fetchall()
    for pack in packs:
        if time.monotonic() >= deadline:
            break
        with connect() as conn:
            conn.execute("SELECT id FROM roots WHERE id=%s FOR UPDATE", (pack["root_id"],)).fetchone()
            changed = conn.execute(f"""UPDATE source_objects o SET retired_at=NOW(),cleanup_due_at=NOW()
                FROM roots r WHERE o.object_key=%s AND r.id=o.root_id AND {ELIGIBLE_PACK}
                RETURNING o.object_key""", (pack["object_key"], retention)).fetchone()
            result["packs_retired"] += bool(changed)
    with connect() as conn:
        pending = conn.execute("""SELECT o.* FROM source_objects o JOIN roots r ON r.id=o.root_id
            WHERE o.retired_at IS NOT NULL AND o.cleanup_due_at<=NOW() AND r.deleting_at IS NULL
            ORDER BY o.cleanup_due_at,o.object_key LIMIT %s FOR UPDATE OF o SKIP LOCKED""", (limit,)).fetchall()
        if pending:
            conn.execute("UPDATE source_objects SET cleanup_due_at=NOW()+INTERVAL '5 minutes' WHERE object_key=ANY(%s)",
                         ([row["object_key"] for row in pending],))
    keys = []
    for pack in pending:
        if time.monotonic() >= deadline:
            break
        key = pack["object_key"]
        prefix = f"sources/{pack['org_id']}/{pack['root_id']}/"
        if not re.fullmatch(re.escape(prefix) + r"(?:packs|multipart)/[0-9a-f-]{36}", key):
            result["failed"] += 1
            continue
        try:
            if key.startswith(prefix + "multipart/"):
                # Include uploads whose creation response never reached our
                # DB; exact-key filtering prevents a prefix-neighbor abort.
                page = s3.list_multipart_uploads(Bucket=bucket, Prefix=key, MaxUploads=10)
                for upload in page.get("Uploads", []):
                    if upload["Key"] != key:
                        raise ValueError("foreign source multipart cleanup target")
                    try:
                        s3.abort_multipart_upload(Bucket=bucket, Key=key, UploadId=upload["UploadId"])
                    except Exception as error:
                        if getattr(error, "response", {}).get("Error", {}).get("Code") != "NoSuchUpload":
                            raise
                if page.get("IsTruncated"):
                    continue
            keys.append(key)
        except Exception:
            result["failed"] += 1
    if keys:
        response = s3.delete_objects(Bucket=bucket, Delete={"Objects": [{"Key": key} for key in keys]})
        failed = {item["Key"] for item in response.get("Errors", [])}
        deleted = {item["Key"] for item in response.get("Deleted", [])} - failed
        if not deleted.issubset(keys) or not failed.issubset(keys):
            raise ValueError("foreign source deletion response")
        if deleted:
            with connect() as conn:
                # Never resurrect a retired object identity. Repeat erasure
                # daily for late network writes; no physical-erasure claim.
                conn.execute("""UPDATE source_objects SET deleted_at=COALESCE(deleted_at,NOW()),
                    cleanup_due_at=NOW()+INTERVAL '1 day' WHERE object_key=ANY(%s) AND retired_at IS NOT NULL""", (list(deleted),))
        result["deleted"] = len(deleted)
        result["failed"] += len(keys) - len(deleted)
    return result
