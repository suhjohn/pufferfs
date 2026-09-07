"""Publish one input manifest, then fence the non-idempotent paid submission."""

import uuid

from file_runtime import database, stable_id
from gemini_contract import MODEL
from provider_manifests import MAX_BATCH_REQUESTS, read_manifest


def finish_preparation(job, count, *, connect=database):
    if type(count) is not int or count < 1:
        raise ValueError("invalid prepared request count")
    with connect() as conn:
        owner = conn.execute("""SELECT id FROM file_work WHERE id=%s AND attempt_token=%s
            AND status='running' AND lease_until>NOW() FOR UPDATE""",
            (job["id"], job["attempt_token"])).fetchone()
        if owner is None:
            raise RuntimeError("work attempt lost ownership")
        summary = conn.execute("""SELECT SUM(request_count) AS count,
            BOOL_AND(provider_job_id IS NOT NULL AND
                (request_count=64 OR ordinal_start+request_count=%s)) AS submitted,
            MIN(ordinal_start) AS first,MAX(ordinal_start+request_count) AS last
            FROM provider_batches WHERE extraction_id=%s""", (count, job["extraction_id"])).fetchone()
        if (summary["count"] != count or summary["first"] != 0 or summary["last"] != count
                or not summary["submitted"]):
            raise RuntimeError("provider preparation is incomplete")
        conn.execute("""UPDATE file_extractions SET prepared_request_count=%s,
            status='waiting_provider',updated_at=NOW() WHERE id=%s""", (count, job["extraction_id"]))
        conn.execute("""UPDATE file_work SET status='waiting_provider',lease_until=NULL,
            updated_at=NOW() WHERE id=%s""", (job["id"],))


def batch_identity(job, start, count):
    if start < 0 or start % MAX_BATCH_REQUESTS or not 1 <= count <= MAX_BATCH_REQUESTS:
        raise ValueError("invalid provider batch range")
    return {"id": stable_id(job["extraction_id"], str(start)), "ordinal_start": start,
            "request_count": count, "attempt_count": 1, "model": MODEL,
            **{key: job[key] for key in ("extraction_id", "root_id", "org_id")}}


def reserve_batch(job, batch, input_ref, *, connect=database):
    # The S3 PUT has completed. A crashed or stale preparer can leave disposable
    # uploads/objects, but cannot publish state under another work attempt.
    with connect() as conn:
        owner = conn.execute("""SELECT id FROM file_work WHERE id=%s AND attempt_token=%s
            AND status='running' AND lease_until>NOW() FOR UPDATE""",
            (job["id"], job["attempt_token"])).fetchone()
        if owner is None:
            raise RuntimeError("work attempt lost ownership")
        saved = conn.execute("""INSERT INTO provider_batches
            (id,extraction_id,org_id,root_id,ordinal_start,request_count,model,status,input_ref,lease_token,lease_until)
            VALUES(%s,%s,%s,%s,%s,%s,%s,'preparing',%s,%s,NOW()+INTERVAL '5 minutes')
            ON CONFLICT(id) DO NOTHING RETURNING *""",
            (batch["id"], batch["extraction_id"], batch["org_id"], batch["root_id"],
             batch["ordinal_start"], batch["request_count"], batch["model"], input_ref, uuid.uuid4().hex)).fetchone()
        if saved is None:
            raise RuntimeError("provider batch already reserved; resume committed manifest")
        return saved


def submission_name(batch):
    return batch["id"] if batch["attempt_count"] == 1 else stable_id(batch["id"], str(batch["attempt_count"]))


def record_submission(batch, provider_job_id, *, connect=database):
    if not provider_job_id:
        raise ValueError("empty provider job ID")
    with connect() as conn:
        row = conn.execute("""UPDATE provider_batches SET provider_job_id=%s,status='submitted',reconciliation_cursor='',error='',updated_at=NOW()
            WHERE id=%s AND attempt_count=%s AND status='preparing' AND submission_started_at IS NOT NULL
              AND provider_job_id IS NULL AND lease_token=%s AND lease_until>NOW() RETURNING *""",
            (provider_job_id, batch["id"], batch["attempt_count"], batch["lease_token"])).fetchone()
        if row is None:
            raise RuntimeError("provider submission lost batch ownership")
    batch.update(row)


def submit_batch(batch, client, s3, bucket, *, connect=database):
    if batch["provider_job_id"]:
        return batch["provider_job_id"]
    if batch["submission_started_at"] is not None:
        if not reconcile_submission(batch, client, connect=connect):
            raise RuntimeError("ambiguous submission requires provider reconciliation")
        return batch["provider_job_id"]
    manifest = read_manifest(s3, bucket, batch, batch["input_ref"], "input")
    if manifest["attempt"] != batch["attempt_count"]:
        raise RuntimeError("provider batch inputs require preparation")
    # Check the same root lock as capture/deletion, then the batch CAS. Neither
    # lock spans S3 or Gemini IO. The marker remains set across ambiguous errors.
    with connect() as conn:
        source = conn.execute("""SELECT e.status,e.version_id,f.captured_version_id,f.deleted,r.deleting_at
            FROM file_extractions e JOIN file_versions v ON v.id=e.version_id
            JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
            WHERE e.id=%s FOR UPDATE OF r""", (batch["extraction_id"],)).fetchone()
        if (source is None or source["deleted"] or source["deleting_at"]
                or source["version_id"] != source["captured_version_id"]
                or source["status"] in {"failed", "superseded"}):
            conn.execute("""UPDATE provider_batches SET status='failed',error='source no longer current'
                WHERE id=%s AND lease_token=%s AND lease_until>NOW() AND submission_started_at IS NULL""",
                (batch["id"], batch["lease_token"]))
            raise RuntimeError("provider source no longer current")
        row = conn.execute("""UPDATE provider_batches SET submission_started_at=NOW(),updated_at=NOW()
            WHERE id=%s AND lease_token=%s AND lease_until>NOW() AND status='preparing'
              AND attempt_count=%s AND input_ref=%s AND submission_started_at IS NULL RETURNING *""",
            (batch["id"], batch["lease_token"], batch["attempt_count"], batch["input_ref"])).fetchone()
        if row is None:
            raise RuntimeError("provider submission lost ownership")
    batch.update(row)
    try:
        remote = client.batches.create(model=batch["model"], src=manifest["input_file_id"],
                                       config={"display_name": submission_name(batch)})
    except Exception as error:
        # An explicit validation/auth/quota rejection did not create a job.
        # Transport errors, timeouts and server errors remain ambiguous.
        if getattr(error, "code", None) in {400, 401, 403, 404, 422, 429}:
            with connect() as conn:
                conn.execute("""UPDATE provider_batches SET submission_started_at=NULL,error=%s
                    WHERE id=%s AND lease_token=%s AND lease_until>NOW() AND provider_job_id IS NULL
                      AND attempt_count=%s""", (f"submission rejected: {error.code}", batch["id"],
                                               batch["lease_token"], batch["attempt_count"]))
        raise
    record_submission(batch, remote.name, connect=connect)
    return remote.name


def reconcile_submission(batch, client, *, connect=database):
    # Bound history traversal and also revisit the newest page. A job accepted
    # or made visible after the previous head read must not wait for a complete
    # million-job history scan. At most two pages / 200 jobs per invocation.
    cursor = batch["reconciliation_cursor"]
    next_cursor = ""
    for index, token in enumerate((cursor, "") if cursor else ("",)):
        try:
            page = client.batches.list(config={"page_size": 100, "page_token": token or None})
        except Exception as error:
            if not token or getattr(error, "code", None) != 400:
                raise
            page = None  # Expired cursor: restart discovery, never paid creation.
        jobs = page.page if page is not None else []
        if len(jobs) > 100:
            raise ValueError("provider listing exceeded its page bound")
        name = submission_name(batch)
        matches = {job.name for job in jobs if job.display_name == name}
        if len(matches) > 1:
            raise RuntimeError("multiple provider jobs have the same durable identity")
        if matches:
            record_submission(batch, matches.pop(), connect=connect)
            return True
        if index == 0:
            next_cursor = (page.config.get("page_token") or "") if page is not None else ""
    if not isinstance(next_cursor, str) or len(next_cursor) > 8192:
        raise ValueError("invalid provider reconciliation cursor")
    with connect() as conn:
        updated = conn.execute("""UPDATE provider_batches SET reconciliation_cursor=%s,updated_at=NOW()
            WHERE id=%s AND lease_token=%s AND lease_until>NOW() AND status='preparing'
              AND attempt_count=%s AND provider_job_id IS NULL AND submission_started_at IS NOT NULL
              AND reconciliation_cursor=%s RETURNING *""",
            (next_cursor, batch["id"], batch["lease_token"], batch["attempt_count"], cursor)).fetchone()
        if updated is None:
            raise RuntimeError("provider reconciliation lost ownership")
    batch.update(updated)
    # A negative listing is never permission to replay a paid create. At the
    # end of a scan, a later invocation starts again to catch delayed visibility.
    return False
