"""Recurring cleanup that survives deletion of root/catalog/organization rows.

Tombstones are retained indefinitely: a successful pass cannot prove there is
no older network write still in flight. Only explicit root deletion creates
these targets. Ordinary sync never deletes retained source/output artifacts.
"""

import re
import time
from itertools import islice

from file_runtime import database
from source_io import iter_chunks, write_chunks

MAX_TARGETS = 25
RECORD_BYTES = 4096


def root_mutation(target):
    return {"namespace": target["target"], "write": {
        "delete_by_filter": ["root_id", "Eq", target["root_id"]],
        "delete_by_filter_allow_partial": True,
    }}


def clear_prefix(s3, bucket, target, *, deadline=float("inf"), clock=time.monotonic):
    """One object page and at most ten abandoned multipart uploads per pass."""
    root, org, prefix = target["root_id"], target["org_id"], target["target"]
    extraction = target.get("extraction_id")
    if extraction is not None:
        if any(not isinstance(part, str) or not re.fullmatch(r"[A-Za-z0-9_-]+", part) for part in (org, root, extraction)):
            raise ValueError("invalid extraction cleanup identity")
        allowed = {f"{kind}/{org}/{root}/{extraction}/" for kind in ("extractions", "mutations")}
    else:
        if any(not isinstance(part, str) or not re.fullmatch(r"[A-Za-z0-9_-]+", part) for part in (org, root)):
            raise ValueError("invalid root cleanup identity")
        allowed = {f"{kind}/{org}/{root}/" for kind in ("sources", "extractions", "mutations")}
    if prefix not in allowed:
        raise ValueError("invalid root cleanup prefix")
    page = s3.list_objects_v2(Bucket=bucket, Prefix=prefix, MaxKeys=1000)
    keys = [item["Key"] for item in page.get("Contents", [])]
    if len(keys) > 1000 or any(not key.startswith(prefix) for key in keys):
        raise ValueError("cleanup listing contains foreign objects")
    if keys:
        if clock() >= deadline:
            return True
        result = s3.delete_objects(Bucket=bucket, Delete={"Objects": [{"Key": key} for key in keys], "Quiet": True})
        if result.get("Errors"):
            raise RuntimeError("root object deletion was incomplete")
    if page.get("IsTruncated"):
        return True  # Start at the remaining first page on the next pass.
    if clock() >= deadline:
        return True
    page = s3.list_multipart_uploads(Bucket=bucket, Prefix=prefix, MaxUploads=10)
    uploads = page.get("Uploads", [])
    if len(uploads) > 10 or any(not item["Key"].startswith(prefix) or not item["UploadId"] for item in uploads):
        raise ValueError("cleanup listing contains foreign multipart uploads")
    for item in uploads:
        if clock() >= deadline:
            return True
        try:
            s3.abort_multipart_upload(Bucket=bucket, Key=item["Key"], UploadId=item["UploadId"])
        except Exception as error:
            if getattr(error, "response", {}).get("Error", {}).get("Code") != "NoSuchUpload":
                raise
    return bool(page.get("IsTruncated"))


def cleanup_deleted_roots(s3, bucket, apply_write, *, connect=database,
                          limit=MAX_TARGETS, time_budget=30, clock=time.monotonic):
    if not 1 <= limit <= MAX_TARGETS or not 0 < time_budget <= 60:
        raise ValueError("invalid root cleanup bounds")
    deadline = clock() + time_budget
    result = {"checked": 0, "partial": 0, "failed": 0}
    with connect() as conn:
        targets = conn.execute("""SELECT * FROM root_cleanup_targets WHERE due_at<=NOW()
            ORDER BY due_at,root_id,kind,target LIMIT %s FOR UPDATE SKIP LOCKED""", (limit,)).fetchall()
        for target in targets:
            conn.execute("""UPDATE root_cleanup_targets SET due_at=NOW()+INTERVAL '5 minutes'
                WHERE root_id=%s AND kind=%s AND target=%s""", (target["root_id"], target["kind"], target["target"]))
    missing = [target for target in targets if target["kind"] == "namespace" and not target["mutation_ref"]]
    if missing and clock() < deadline:
        try:
            # Pack metadata-only erasure records outside the source prefixes
            # being erased. Persist once, then reuse the exact artifact on retry.
            ref, _ = write_chunks(s3, bucket, "maintenance/root-deletions", map(root_mutation, missing), max_record_bytes=RECORD_BYTES)
            with connect() as conn:
                for ordinal, target in enumerate(missing):
                    saved = conn.execute("""UPDATE root_cleanup_targets SET mutation_ref=%s,mutation_record=%s
                        WHERE root_id=%s AND kind=%s AND target=%s AND mutation_ref='' RETURNING root_id""",
                        (ref, ordinal, target["root_id"], target["kind"], target["target"])).fetchone()
                    if saved:
                        target["mutation_ref"], target["mutation_record"] = ref, ordinal
        except Exception:
            # Prefix erasure can proceed independently; namespace targets with
            # no durable artifact remain due for recovery, never applied below.
            for target in missing:
                target["mutation_ref"] = ""
    artifacts = {}
    for target in targets:
        if clock() >= deadline:
            break
        try:
            with connect() as conn:
                live = conn.execute("SELECT deleting_at FROM roots WHERE id=%s", (target["root_id"],)).fetchone()
            if live is not None and live["deleting_at"] is None:
                raise ValueError("cleanup target belongs to a live root")
            if target["kind"] == "namespace":
                # A namespace may have been remapped/shared. Delete only this
                # immutable root identity, never an entire namespace on replay.
                ref = target["mutation_ref"]
                if not ref:
                    raise RuntimeError("no durable root deletion mutation")
                if ref not in artifacts:
                    records = iter_chunks(s3, bucket, ref, max_record_bytes=RECORD_BYTES)
                    try:
                        artifacts[ref] = list(islice(records, MAX_TARGETS + 1))
                    finally:
                        records.close()
                    if len(artifacts[ref]) > MAX_TARGETS:
                        raise ValueError("root deletion mutation pack exceeds target bound")
                record = artifacts[ref][target["mutation_record"]]
                if record != root_mutation(target):
                    raise ValueError("root deletion mutation does not match tombstone")
                try:
                    remaining = apply_write(record["namespace"], record["write"], target["vector_disabled"])
                except Exception as error:
                    if getattr(error, "status_code", None) != 404:
                        raise
                    remaining = False
                if remaining is not None and type(remaining) is not bool:
                    raise ValueError("invalid root index cleanup response")
            else:
                remaining = clear_prefix(s3, bucket, target, deadline=deadline, clock=clock)
            if remaining:
                result["partial"] += 1
                continue
            with connect() as conn:
                conn.execute("""UPDATE root_cleanup_targets SET last_checked_at=NOW(),due_at=NOW()+INTERVAL '1 day'
                    WHERE root_id=%s AND kind=%s AND target=%s""", (target["root_id"], target["kind"], target["target"]))
            result["checked"] += 1
        except Exception:
            result["failed"] += 1
    return result
