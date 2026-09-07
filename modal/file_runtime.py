"""Database/SQS edges for per-file workers. Postgres is not the work queue."""

from __future__ import annotations

import hashlib
import json
import os
import atexit
import threading
import time
import uuid
from contextlib import contextmanager

from psycopg import Connection
from psycopg_pool import ConnectionPool
from worker_metrics import timed, count as metric_count


_pool = None
_pool_lock = threading.Lock()


class WorkerConnection(Connection):
    """Count application statements without logging SQL or its parameters."""

    last_used = 0.0

    def execute(self, query, *args, **kwargs):
        metric_count("db_statements")
        if isinstance(query, str) and (query.lstrip().split(None, 1) or [""])[0].upper() in {"INSERT", "UPDATE", "DELETE"}:
            metric_count("db_write_statements")
        with timed("db_execute"):
            return super().execute(query, *args, **kwargs)


def check_connection(conn):
    # Queries themselves verify actively used sockets. Avoid an extra SELECT
    # for every short transaction; probe only a new/idle connection. Failures
    # between checkouts still use the existing durable work/SQS retry path.
    if time.monotonic() - conn.last_used >= 30:
        metric_count("db_health_checks")
        ConnectionPool.check_connection(conn)


def connection_pool():
    from psycopg.rows import dict_row

    global _pool
    with _pool_lock:
        if _pool is None:
            maximum = int(os.getenv("PUFFERFS_WORKER_DB_MAX_CONNS", "2"))
            if not 2 <= maximum <= 16:
                raise ValueError("PUFFERFS_WORKER_DB_MAX_CONNS must be 2..16")
            # Keep capacity for a lease heartbeat while a foreground operation
            # holds its transaction. Idle workers release connections; scaling
            # out does not reserve a minimum pool per container.
            _pool = ConnectionPool(os.environ["DATABASE_URL"], connection_class=WorkerConnection,
                min_size=0, max_size=maximum,
                kwargs={"row_factory": dict_row, "connect_timeout": 15, "prepare_threshold": None},
                timeout=30, max_idle=60, max_lifetime=900, num_workers=1,
                check=check_connection, open=True)
            atexit.register(_pool.close)
        return _pool


@contextmanager
def database():
    # Preserve one transaction per operation. Reuse only the connection, never
    # job state or transactions across requests. Pool exit commits/rolls back;
    # failed connections are discarded and idle stale sockets are checked.
    pool = connection_pool()
    with timed("db_acquire"):
        conn = pool.getconn()
    try:
        with timed("db_transaction"), conn.transaction():
            yield conn
    finally:
        conn.last_used = time.monotonic()
        pool.putconn(conn)


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
                      r.deleting_at, (w.lease_until > NOW()) AS lease_active,
                      CASE WHEN w.stage='index' THEN (
                          SELECT jsonb_agg(jsonb_build_object('namespace',n.namespace,
                              'shard_index',n.shard_index,'shard_count',n.shard_count))
                          FROM root_index_namespaces n
                          WHERE n.root_id=r.id AND n.org_id=r.org_id AND n.retired_at IS NULL
                      ) END AS namespaces
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


def complete_extraction(job: dict, chunks_ref: str, count: int, *, connect=database) -> dict:
    if count < 0 or not chunks_ref.startswith(f"extractions/{job['org_id']}/{job['root_id']}/{job['extraction_id']}/"):
        raise ValueError("invalid extraction artifact")
    index_id = work_id(job["extraction_id"], "index")
    with connect() as conn:
        # Count this compound write explicitly: execute's first-token counter
        # handles ordinary INSERT/UPDATE/DELETE, not data-modifying CTEs.
        metric_count("db_write_statements")
        result = conn.execute(
            """WITH completed AS (
                UPDATE file_work SET status='complete',lease_until=NULL,updated_at=NOW()
                WHERE id=%s AND attempt_token=%s AND status='running' AND lease_until>NOW()
                RETURNING extraction_id
            ), extracted AS (
                UPDATE file_extractions e SET status='complete',chunks_ref=%s,chunk_count=%s,
                    error='',updated_at=NOW() FROM completed c WHERE e.id=c.extraction_id
                RETURNING e.id
            ), queued AS (
                INSERT INTO file_work(id,extraction_id,stage)
                SELECT %s,id,'index' FROM extracted
                ON CONFLICT(extraction_id,stage) DO NOTHING
            ) SELECT id FROM extracted""",
            (job["id"], job["attempt_token"], chunks_ref, count, index_id),
        ).fetchone()
        if result is None:
            raise RuntimeError("work attempt lost ownership before completion")
    return {"id": index_id, "stage": "index", **{key: job[key] for key in
        ("extraction_id", "version_id", "file_id", "root_id", "org_id")}}


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
    return publish_deliveries(sqs, pending, connect=connect)


def publish_deliveries(sqs, pending, *, connect=database) -> int:
    """Send committed work references; mark only SQS-confirmed deliveries."""
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
            delivered_ids = [job["id"] for i, job in enumerate(batch) if str(i) in succeeded]
            with connect() as conn:
                conn.execute("UPDATE file_work SET enqueued_at=NOW() WHERE id=ANY(%s) AND enqueued_at IS NULL", (delivered_ids,))
            delivered += len(delivered_ids)
            if response.get("Failed"):
                raise RuntimeError("SQS rejected part of the delivery batch; unmarked entries remain recoverable")
    return delivered
