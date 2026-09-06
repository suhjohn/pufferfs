"""Delete exact provider uploads only after every durable user releases them.

Generated batch results have a separate, provider-managed retention lifecycle;
Files.delete does not delete them. A batch's disappearance is not proof that
its result download is gone.
"""

import re
import time
from datetime import datetime

from file_runtime import database

TERMINAL = {"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED"}


def record_provider_files(files, *, batch_id=None, connect=database):
    if not 1 <= len(files) <= 65:
        raise ValueError("invalid provider file registration size")
    for file_id, extraction_id, expires_at in files:
        if not isinstance(file_id, str) or not re.fullmatch(r"files/[A-Za-z0-9_-]+", file_id):
            raise ValueError("invalid provider file identity")
        if not extraction_id and not batch_id:
            raise ValueError("provider file must have a durable owner")
        if expires_at is not None and (not isinstance(expires_at, datetime) or expires_at.utcoffset() is None):
            raise ValueError("provider expiry must be a timezone-aware timestamp")
    ids, extractions, expirations = map(list, zip(*files))
    with connect() as conn:
        # Inputs referenced by another batch are already registered. Never
        # extend their deadline on retry. If an upload response has no expiry,
        # the documented 48-hour upload policy provides a conservative bound.
        conn.execute("""INSERT INTO provider_files(file_id,extraction_id,expires_at)
            SELECT id,extraction,COALESCE(expiration,NOW()+INTERVAL '48 hours')
            FROM unnest(%s::text[],%s::text[],%s::timestamptz[]) AS f(id,extraction,expiration)
            ON CONFLICT(file_id) DO NOTHING""", (ids, extractions, expirations))
        if batch_id:
            conn.execute("""INSERT INTO provider_batch_files(batch_id,file_id)
                SELECT %s,unnest(%s::text[]) ON CONFLICT DO NOTHING""", (batch_id, ids))


def cleanup_provider_files(client, *, connect=database, limit=64, time_budget=30):
    if not 1 <= limit <= 256 or not 0 < time_budget <= 60:
        raise ValueError("invalid provider cleanup bounds")
    deadline = time.monotonic() + time_budget
    result = {"deleted": 0, "expired": 0, "failed": 0, "cancelled": 0}
    with connect() as conn:
        # Expiry is provider-controlled, even while a batch is outstanding.
        # This records passage of the retention deadline, not a successful
        # DELETE or independently verified physical erasure. No source, request
        # mapping or batch recovery state is removed. Keep any last error.
        expired = conn.execute("""WITH due AS (
            SELECT file_id FROM provider_files WHERE deleted_at IS NULL
            AND expired_at IS NULL AND expires_at<=NOW()
            ORDER BY expires_at,file_id LIMIT %s FOR UPDATE SKIP LOCKED
        ) UPDATE provider_files f SET expired_at=NOW() FROM due
            WHERE f.file_id=due.file_id RETURNING f.file_id""", (limit,)).fetchall()
        result["expired"] = len(expired)
    # Root deletion removes request rows, but must not erase the provider job
    # or uploaded-file identities needed to cancel and clean up afterward.
    with connect() as conn:
        abandoned = conn.execute("""SELECT b.id,b.provider_job_id FROM provider_batches b
            WHERE b.status='submitted' AND b.provider_job_id IS NOT NULL
            AND NOT EXISTS (SELECT 1 FROM provider_requests p WHERE p.batch_id=b.id)
            ORDER BY b.updated_at LIMIT %s""", (limit,)).fetchall()
    for batch in abandoned:
        if time.monotonic() >= deadline:
            return result
        try:
            remote = client.batches.get(name=batch["provider_job_id"])
            if remote.state.name not in TERMINAL:
                client.batches.cancel(name=batch["provider_job_id"])
                result["cancelled"] += 1
                continue  # A cancellation request does not prove terminality.
            with connect() as conn:
                conn.execute("""UPDATE provider_batches SET status='failed',error='source removed',updated_at=NOW()
                    WHERE id=%s AND status='submitted'
                    AND NOT EXISTS (SELECT 1 FROM provider_requests WHERE batch_id=%s)""", (batch["id"], batch["id"]))
        except Exception:
            result["failed"] += 1
    with connect() as conn:
        files = conn.execute("""SELECT f.file_id FROM provider_files f
            WHERE f.deleted_at IS NULL AND f.expired_at IS NULL
            AND f.expires_at>NOW() AND f.next_check_at<=NOW()
            AND NOT EXISTS (SELECT 1 FROM file_work w WHERE w.extraction_id=f.extraction_id
                AND w.stage='transform' AND w.status IN ('pending','running','waiting_provider'))
            AND NOT EXISTS (SELECT 1 FROM provider_batch_files bf JOIN provider_batches b ON b.id=bf.batch_id
                WHERE bf.file_id=f.file_id AND b.status IN ('preparing','submitted'))
            AND NOT EXISTS (SELECT 1 FROM provider_requests p JOIN file_extractions e ON e.id=p.extraction_id
                WHERE p.input_file_id=f.file_id AND p.status<>'complete' AND e.status='waiting_provider')
            ORDER BY f.next_check_at,f.file_id LIMIT %s FOR UPDATE OF f SKIP LOCKED""", (limit,)).fetchall()
        for file in files:
            conn.execute("UPDATE provider_files SET next_check_at=NOW()+INTERVAL '5 minutes' WHERE file_id=%s", (file["file_id"],))
    for file in files:
        if time.monotonic() >= deadline:
            break
        try:
            try:
                client.files.delete(name=file["file_id"])
            except Exception as error:
                if getattr(error, "code", None) != 404:
                    raise  # Auth/transport failures never mean already deleted.
            with connect() as conn:
                conn.execute("UPDATE provider_files SET deleted_at=NOW(),error='' WHERE file_id=%s", (file["file_id"],))
            result["deleted"] += 1
        except Exception as error:
            code = getattr(error, "code", None)
            detail = f"{type(error).__name__} status={code if type(code) is int else 'unknown'}"
            with connect() as conn:
                conn.execute("UPDATE provider_files SET error=%s WHERE file_id=%s", (detail, file["file_id"]))
            result["failed"] += 1
    return result
