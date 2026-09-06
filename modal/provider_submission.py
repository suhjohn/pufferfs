"""Durable Gemini submission edges. SQS still delivers transformation work."""

import json
from pathlib import Path
import tempfile

from file_runtime import database, stable_id
from gemini_contract import MODEL, batch_request

MAX_BATCH_REQUESTS = 64


def finish_preparation(job: dict, count: int, *, connect=database):
    """Seal the expected page/clip count before handing off to the collector.

    A collector must not mistake the first submitted batch of a large document
    for the entire document. Zero-input files use ordinary empty extraction.
    """
    if type(count) is not int or count < 1:
        raise ValueError("invalid prepared request count")
    with connect() as conn:
        owner = conn.execute(
            """SELECT id FROM file_work WHERE id=%s AND attempt_token=%s
               AND status='running' AND lease_until>NOW() FOR UPDATE""",
            (job["id"], job["attempt_token"]),
        ).fetchone()
        if owner is None:
            raise RuntimeError("work attempt lost ownership")
        summary = conn.execute(
            """SELECT COUNT(*) AS count, MIN(p.ordinal) AS first, MAX(p.ordinal) AS last,
               BOOL_AND(b.provider_job_id IS NOT NULL) AS submitted
               FROM provider_requests p LEFT JOIN provider_batches b ON b.id=p.batch_id
               WHERE p.extraction_id=%s""", (job["extraction_id"],),
        ).fetchone()
        if (summary["count"] != count or summary["first"] != 0
                or summary["last"] != count - 1 or not summary["submitted"]):
            raise RuntimeError("provider preparation is incomplete")
        conn.execute("""UPDATE file_extractions SET prepared_request_count=%s,
                        status='waiting_provider',updated_at=NOW() WHERE id=%s""", (count, job["extraction_id"]))
        conn.execute("""UPDATE file_work SET status='waiting_provider',lease_until=NULL,
                        updated_at=NOW() WHERE id=%s""", (job["id"],))


def reserve_batch(job: dict, inputs: list[dict], *, connect=database) -> str:
    """Persist bounded request mappings before creating any paid provider job.

    Inputs contain provider file references from temporary uploads, not source
    bytes. Retry identity is the extraction plus ordered page/clip ordinals.
    """
    if not 1 <= len(inputs) <= MAX_BATCH_REQUESTS:
        raise ValueError("invalid provider batch size")
    ordinals = [item["ordinal"] for item in inputs]
    if any(type(n) is not int or n < 0 for n in ordinals) or ordinals != sorted(set(ordinals)):
        raise ValueError("invalid provider request ordinals")
    batch_id = stable_id(job["extraction_id"], *map(str, ordinals))
    with connect() as conn:
        owner = conn.execute(
            """SELECT id FROM file_work WHERE id=%s AND attempt_token=%s
               AND status='running' AND lease_until>NOW() FOR UPDATE""",
            (job["id"], job["attempt_token"]),
        ).fetchone()
        if owner is None:
            raise RuntimeError("work attempt lost ownership")
        conn.execute("""INSERT INTO provider_batches(id,status,model) VALUES(%s,'preparing',%s)
                        ON CONFLICT(id) DO NOTHING""", (batch_id, MODEL))
        for item in inputs:
            key = stable_id(job["extraction_id"], str(item["ordinal"]))
            batch_request(key, item["mime_type"], item["input_uri"], item["location"])
            conn.execute(
                """INSERT INTO provider_requests(request_key,extraction_id,batch_id,ordinal,
                   location,input_file_id,mime_type,input_uri)
                   VALUES(%s,%s,%s,%s,%s::jsonb,%s,%s,%s) ON CONFLICT(request_key) DO NOTHING""",
                (key, job["extraction_id"], batch_id, item["ordinal"], json.dumps(item["location"]),
                 item["input_file_id"], item["mime_type"], item["input_uri"]),
            )
            existing = conn.execute("SELECT * FROM provider_requests WHERE request_key=%s", (key,)).fetchone()
            if existing["batch_id"] != batch_id or existing["location"] != item["location"] or existing["mime_type"] != item["mime_type"]:
                raise ValueError("provider request identity changed on retry")
    return batch_id


def record_submission(batch_id: str, provider_job_id: str, *, connect=database):
    if not provider_job_id:
        raise ValueError("empty provider job ID")
    with connect() as conn:
        result = conn.execute(
            """UPDATE provider_batches SET provider_job_id=%s,status='submitted',updated_at=NOW()
               WHERE id=%s AND status='preparing' AND submission_started_at IS NOT NULL
               AND provider_job_id IS NULL RETURNING id""", (provider_job_id, batch_id),
        ).fetchone()
        if result:
            conn.execute("UPDATE provider_requests SET status='submitted' WHERE batch_id=%s AND status='pending'", (batch_id,))
        else:
            row = conn.execute("SELECT provider_job_id FROM provider_batches WHERE id=%s", (batch_id,)).fetchone()
            if row is None or row["provider_job_id"] != provider_job_id:
                raise RuntimeError("conflicting provider submission")


def submit_batch(batch_id: str, client, *, connect=database) -> str:
    with connect() as conn:
        batch = conn.execute("SELECT * FROM provider_batches WHERE id=%s", (batch_id,)).fetchone()
        if batch is None:
            raise ValueError("unknown provider batch")
        if batch["provider_job_id"]:
            return batch["provider_job_id"]
        if batch["submission_started_at"] is not None:
            raise RuntimeError("ambiguous submission requires provider reconciliation")
        requests = conn.execute("SELECT * FROM provider_requests WHERE batch_id=%s ORDER BY ordinal", (batch_id,)).fetchall()
    if not 1 <= len(requests) <= MAX_BATCH_REQUESTS:
        raise ValueError("invalid persisted batch size")
    # This small request envelope is disposable until the submission marker.
    # Always build it from the snapshot: a historical cached envelope may have
    # expired or still reference replaced temporary page/clip uploads.
    with tempfile.TemporaryDirectory(prefix="pufferfs-batch-") as directory:
        path = Path(directory) / "requests.jsonl"
        with path.open("w", encoding="utf-8") as output:
            for request in requests:
                output.write(json.dumps(batch_request(request["request_key"], request["mime_type"], request["input_uri"], request["location"])) + "\n")
        uploaded = client.files.upload(file=str(path), config={"mime_type": "jsonl", "display_name": batch_id})
    input_id = uploaded.name
    from provider_cleanup import record_provider_files
    record_provider_files([(input_id, None, uploaded.expiration_time),
                          *((r["input_file_id"], r["extraction_id"], None) for r in requests)],
                          batch_id=batch_id, connect=connect)
    # Commit this marker BEFORE the external call. Never reset it on a network
    # error: the provider may have accepted the paid request despite a timeout.
    with connect() as conn:
        if batch["retry_of"]:
            # Reservation and submission may straddle a newer capture or
            # deletion. Recheck before creating more paid work, using the root
            # lock shared by capture registration. Never hold it over network IO.
            source = conn.execute("""SELECT e.id AS extraction_id,e.status,e.version_id,f.captured_version_id,f.deleted,r.deleting_at
                FROM provider_requests p JOIN file_extractions e ON e.id=p.extraction_id
                JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
                JOIN roots r ON r.id=f.root_id WHERE p.batch_id=%s LIMIT 1 FOR UPDATE OF r""", (batch_id,)).fetchone()
            if (source is None or source["status"] != "waiting_provider" or source["deleted"]
                    or source["deleting_at"] or source["captured_version_id"] != source["version_id"]):
                conn.execute("""UPDATE provider_batches SET status='failed',error='retry no longer current',updated_at=NOW()
                    WHERE id=%s AND submission_started_at IS NULL""", (batch_id,))
                if source and source["status"] == "waiting_provider":
                    conn.execute("UPDATE file_extractions SET status='superseded',updated_at=NOW() WHERE id=%s AND status='waiting_provider'", (source["extraction_id"],))
                    conn.execute("UPDATE file_work SET status='superseded',updated_at=NOW() WHERE extraction_id=%s AND stage='transform' AND status='waiting_provider'", (source["extraction_id"],))
                return ""
        # Refresh can race the JSONL upload. Lock the batch and compare the
        # exact input snapshot before committing the paid-submission marker.
        conn.execute("SELECT id FROM provider_batches WHERE id=%s FOR UPDATE", (batch_id,)).fetchone()
        current_requests = conn.execute("SELECT * FROM provider_requests WHERE batch_id=%s ORDER BY ordinal", (batch_id,)).fetchall()
        snapshot = lambda rows: [(r["request_key"], r["input_file_id"], r["input_uri"]) for r in rows]
        if snapshot(current_requests) != snapshot(requests):
            raise RuntimeError("provider inputs changed before submission; retry with refreshed inputs")
        claimed = conn.execute(
            """UPDATE provider_batches SET input_file_id=%s,submission_started_at=NOW(),updated_at=NOW()
               WHERE id=%s AND status='preparing' AND submission_started_at IS NULL RETURNING id""",
            (input_id, batch_id),
        ).fetchone()
    if claimed is None:
        raise RuntimeError("provider submission already started")
    remote = client.batches.create(model=batch["model"], src=input_id, config={"display_name": batch_id})
    record_submission(batch_id, remote.name, connect=connect)
    return remote.name


def reconcile_submission(batch_id: str, client, *, connect=database) -> bool:
    """Resolve an accepted-but-unrecorded create without resubmitting inference.

    Absence from a listing is not proof of rejection. Leave it unresolved for
    subsequent reconciliation rather than assuming a retry is safe.
    """
    matches = [job.name for job in client.batches.list() if job.display_name == batch_id]
    if len(set(matches)) > 1:
        raise RuntimeError("multiple provider jobs have the same durable identity")
    if not matches:
        return False
    record_submission(batch_id, matches[0], connect=connect)
    return True
