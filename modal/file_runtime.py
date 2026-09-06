"""Database/SQS edges for per-file workers. Postgres is not the work queue."""

from __future__ import annotations

import hashlib
import json
import os
import uuid
from contextlib import contextmanager


@contextmanager
def database():
    import psycopg
    from psycopg.rows import dict_row

    # One short-lived connection per operation. Never migrate from a worker.
    with psycopg.connect(os.environ["DATABASE_URL"], row_factory=dict_row, connect_timeout=15) as conn:
        yield conn


def stable_id(*parts: str) -> str:
    digest = hashlib.sha256()
    for part in parts:
        digest.update(part.encode())
        digest.update(b"\0")
    return digest.hexdigest()


def work_id(extraction_id: str, stage: str) -> str:
    # Same UUIDv5 identity as Go's uuid.NewSHA1(NameSpaceOID, ...).
    return str(uuid.uuid5(uuid.NAMESPACE_OID, f"{extraction_id}:{stage}"))


def claim_work(work: str, stage: str, token: str, *, connect=database) -> dict:
    if stage not in {"transform", "index"} or not token:
        raise ValueError("invalid work attempt")
    with connect() as conn:
        job = conn.execute(
            """SELECT w.*, e.version_id, e.revision, e.sequence AS extraction_sequence, e.chunks_ref, e.chunk_count,
                      v.file_id, v.sequence, v.source_manifest_ref, v.content_hash,
                      v.size_bytes, v.deleted AS version_deleted,
                      f.path AS file_path, f.root_id, f.captured_version_id,
                      f.indexed_version_id, published.sequence AS indexed_extraction_sequence,
                      r.org_id, r.source_path, r.vector_disabled,
                      r.deleting_at, (w.lease_until > NOW()) AS lease_active
               FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
               JOIN file_versions v ON v.id=e.version_id
               JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
               LEFT JOIN file_extractions published ON published.id=f.indexed_extraction_id
               WHERE w.id=%s FOR UPDATE OF w""", (work,),
        ).fetchone()
        if job is None:
            return {"status": "superseded"}
        if job["stage"] != stage:
            raise ValueError("work delivered to wrong deployment role")
        if job["status"] in {"complete", "waiting_provider", "superseded", "failed"}:
            return {"status": job["status"]}
        stale_revision = (job["indexed_version_id"] == job["version_id"]
                          and job["indexed_extraction_sequence"] is not None
                          and job["indexed_extraction_sequence"] > job["extraction_sequence"])
        if job["deleting_at"] is not None or job["version_id"] != job["captured_version_id"] or stale_revision:
            conn.execute("UPDATE file_work SET status='superseded', updated_at=NOW() WHERE id=%s", (work,))
            return {"status": "superseded"}
        if job["status"] == "running" and job["lease_active"]:
            return {"status": "busy"}
        conn.execute(
            """UPDATE file_work SET status='running', attempt_token=%s,
               attempt_count=attempt_count+1, lease_until=NOW()+INTERVAL '5 minutes',
               updated_at=NOW() WHERE id=%s""", (token, work),
        )
        job.update(status="running", attempt_token=token)
        return job


def heartbeat(job: dict, *, connect=database) -> None:
    with connect() as conn:
        result = conn.execute(
            """UPDATE file_work SET lease_until=NOW()+INTERVAL '5 minutes',updated_at=NOW()
               WHERE id=%s AND attempt_token=%s AND status='running'
                 AND lease_until>NOW()""", (job["id"], job["attempt_token"]),
        )
        if result.rowcount != 1:
            raise RuntimeError("work attempt lost ownership")


def fail_attempt(job: dict, error: Exception, *, connect=database) -> None:
    with connect() as conn:
        # Leave the message unacknowledged; SQS owns retries and dead letters.
        conn.execute(
            """UPDATE file_work SET status='pending',lease_until=NULL,attempt_token=NULL,
               error=%s,updated_at=NOW() WHERE id=%s AND attempt_token=%s AND status='running'""",
            (type(error).__name__ + ": " + str(error)[:1000], job["id"], job["attempt_token"]),
        )


def complete_extraction(job: dict, chunks_ref: str, count: int, *, connect=database) -> str:
    if count < 0 or not chunks_ref.startswith(f"extractions/{job['org_id']}/{job['root_id']}/{job['extraction_id']}/"):
        raise ValueError("invalid extraction artifact")
    with connect() as conn:
        result = conn.execute(
            """UPDATE file_work SET status='complete',lease_until=NULL,updated_at=NOW()
               WHERE id=%s AND attempt_token=%s AND status='running' AND lease_until>NOW()
               RETURNING id""", (job["id"], job["attempt_token"]),
        ).fetchone()
        if result is None:
            raise RuntimeError("work attempt lost ownership before completion")
        conn.execute(
            """UPDATE file_extractions SET status='complete',chunks_ref=%s,chunk_count=%s,
               error='',updated_at=NOW() WHERE id=%s""", (chunks_ref, count, job["extraction_id"]),
        )
        index_id = work_id(job["extraction_id"], "index")
        conn.execute(
            """INSERT INTO file_work(id,extraction_id,stage) VALUES(%s,%s,'index')
               ON CONFLICT(extraction_id,stage) DO NOTHING""", (index_id, job["extraction_id"]),
        )
    return index_id


def publish_pending(sqs, *, connect=database, limit: int = 100) -> int:
    """Repair committed-but-unpublished SQS handoffs; never claim execution."""
    if not 1 <= limit <= 1000:
        raise ValueError("invalid delivery scan limit")
    with connect() as conn:
        pending = conn.execute(
            """SELECT w.id,w.stage,w.extraction_id,v.id AS version_id,v.file_id,f.root_id,r.org_id
               FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
               JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
               JOIN roots r ON r.id=f.root_id
               WHERE w.status='pending' AND w.enqueued_at IS NULL AND r.deleting_at IS NULL
               ORDER BY w.updated_at,w.id LIMIT %s""", (limit,),
        ).fetchall()
    delivered = 0
    for stage in ("transform", "index"):
        jobs = [job for job in pending if job["stage"] == stage]
        if not jobs:
            continue
        url = os.environ[f"PUFFERFS_SQS_{stage.upper()}_QUEUE_URL"]
        for offset in range(0, len(jobs), 10):
            batch = jobs[offset:offset + 10]
            entries = []
            for i, job in enumerate(batch):
                body = dict(job, job_id=job["id"], work_id=job["id"])
                del body["id"]
                group = job["file_id"] if stage == "index" else job["id"]
                entries.append({
                    "Id": str(i), "MessageBody": json.dumps(body, separators=(",", ":")),
                    "MessageGroupId": stable_id(job["org_id"], job["root_id"], group, stage),
                    "MessageDeduplicationId": stable_id(job["id"]),
                })
            response = sqs.send_message_batch(QueueUrl=url, Entries=entries)
            succeeded = {item["Id"] for item in response.get("Successful", [])}
            with connect() as conn:
                for i, job in enumerate(batch):
                    if str(i) in succeeded:
                        conn.execute("UPDATE file_work SET enqueued_at=COALESCE(enqueued_at,NOW()) WHERE id=%s", (job["id"],))
                        delivered += 1
            if response.get("Failed"):
                raise RuntimeError("SQS rejected part of the delivery batch; unmarked entries remain recoverable")
    return delivered
