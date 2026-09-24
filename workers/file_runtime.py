"""Durable file scheduling and ownership; capture registration is the enqueue."""

from __future__ import annotations

import hashlib
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
    # between checkouts still use the existing durable work retry path.
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


MAX_WORK_ATTEMPTS = 5


def claim_due_work(stage, *, connect=database):
    """Claim one due file without holding a transaction across external IO."""
    if stage not in {"transform", "index"}:
        raise ValueError("unknown work phase")
    with connect() as conn:
        tenant = conn.execute("""SELECT t.org_id FROM file_work_tenants t
            WHERE t.stage=%s AND EXISTS (
                SELECT 1 FROM file_work w WHERE w.org_id=t.org_id AND w.stage=t.stage
                  AND w.next_attempt_at<=NOW() AND w.attempt_count<%s
                  AND (w.status='pending' OR (w.status='running' AND w.lease_until<=NOW()))
            ) ORDER BY t.last_claimed,t.org_id LIMIT 1 FOR UPDATE OF t SKIP LOCKED""",
            (stage, MAX_WORK_ATTEMPTS)).fetchone()
        if tenant is None:
            return None
        row = conn.execute("""WITH due AS (
            SELECT w.id FROM file_work w
            WHERE w.stage=%s AND w.org_id=%s AND w.next_attempt_at<=NOW()
              AND (w.status='pending' OR (w.status='running' AND w.lease_until<=NOW()))
              AND w.attempt_count<%s
            ORDER BY w.next_attempt_at,w.id LIMIT 1 FOR UPDATE SKIP LOCKED
        ) UPDATE file_work w SET status='running',attempt_token=%s,
            attempt_count=attempt_count+1,lease_until=NOW()+INTERVAL '5 minutes',updated_at=NOW()
            FROM due WHERE w.id=due.id RETURNING w.id""",
            (stage, tenant["org_id"], MAX_WORK_ATTEMPTS, uuid.uuid4().hex)).fetchone()
        if row is None:
            return None
        conn.execute("UPDATE file_work_tenants SET last_claimed=clock_timestamp() WHERE org_id=%s AND stage=%s",
            (tenant["org_id"], stage))
        job = conn.execute("""SELECT w.*,e.version_id,e.revision,e.row_format,
            e.sequence AS extraction_sequence,e.chunks_ref,e.chunk_count,
            e.status AS extraction_status,e.source_verified,e.append_checkpoint_ref,e.append_chunk_count,
            v.file_id,v.sequence,v.previous_version_id,v.source_manifest_ref,v.content_hash,v.size_bytes,
            v.deleted AS version_deleted,f.path AS file_path,f.root_id,f.captured_version_id,
            f.indexed_version_id,published.sequence AS indexed_extraction_sequence,
            r.org_id,r.source_path,r.vector_disabled,r.deleting_at,
            (SELECT n.namespace FROM root_index_namespaces n WHERE n.root_id=r.id
                AND n.org_id=r.org_id AND n.retired_at IS NULL) AS namespace
            FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
            JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
            JOIN roots r ON r.id=f.root_id
            LEFT JOIN file_extractions published ON published.id=f.indexed_extraction_id
            WHERE w.id=%s""", (row["id"],)).fetchone()
        stale_revision = (job["indexed_version_id"] == job["version_id"]
            and job["indexed_extraction_sequence"] is not None
            and job["indexed_extraction_sequence"] > job["extraction_sequence"])
        if job["deleting_at"] or job["captured_version_id"] != job["version_id"] or stale_revision:
            conn.execute("UPDATE file_work SET status='superseded',lease_until=NULL WHERE id=%s", (row["id"],))
            conn.execute("UPDATE file_extractions SET status='superseded',updated_at=NOW() WHERE id=%s AND status IN ('pending','waiting_provider')",
                (job["extraction_id"],))
            return None
        return job


@contextmanager
def work_lease(job, *, connect=database):
    """Renew ownership while doing IO; every state transition still fences its token."""
    stopped, errors = threading.Event(), []

    def renew():
        while not stopped.wait(60):
            try:
                heartbeat(job, connect=connect)
            except Exception as error:
                errors.append(error)
                return

    thread = threading.Thread(target=renew, daemon=True)
    thread.start()
    def check():
        if errors:
            raise RuntimeError("work lease renewal failed") from errors[0]
    try:
        yield check
        # Every successful return already committed a token/lease-fenced state
        # transition. A heartbeat racing that release must not turn confirmed
        # work into a spurious failure after the operation has finished.
    finally:
        stopped.set()
        thread.join(timeout=20)


def heartbeat(job: dict, *, connect=database) -> None:
    with connect() as conn:
        result = conn.execute(
            """UPDATE file_work SET lease_until=NOW()+INTERVAL '5 minutes',updated_at=NOW()
               WHERE id=%s AND attempt_token=%s AND status='running'
                 AND lease_until>NOW()""", (job["id"], job["attempt_token"]),
        )
        if result.rowcount != 1:
            raise RuntimeError("work attempt lost ownership")


def fail_attempt(job, error, *, connect=database):
    with connect() as conn:
        conn.execute("""UPDATE file_work SET
            status=CASE WHEN attempt_count>=%s THEN 'failed' ELSE 'pending' END,
            next_attempt_at=NOW()+make_interval(secs=>LEAST(300,30*attempt_count)),
            lease_until=NULL,attempt_token=NULL,error=%s,updated_at=NOW()
            WHERE id=%s AND attempt_token=%s AND status='running'""",
            (MAX_WORK_ATTEMPTS, type(error).__name__, job["id"], job["attempt_token"]))


def checkpoint_index(job, start, end, *, connect=database):
    """Acknowledge only a confirmed contiguous write of this immutable artifact."""
    if not 0 <= start < end <= job["chunk_count"]:
        raise ValueError("invalid index checkpoint")
    with connect() as conn:
        changed = conn.execute("""UPDATE file_work w SET index_cursor=%s,updated_at=NOW()
            FROM file_extractions e WHERE w.extraction_id=e.id AND w.id=%s
              AND w.stage='index' AND w.status='running' AND w.attempt_token=%s
              AND w.lease_until>NOW() AND w.index_cursor=%s
              AND e.chunks_ref=%s AND e.row_format=%s AND e.chunk_count=%s""",
            (end, job["id"], job["attempt_token"], start, job["chunks_ref"],
             job["row_format"], job["chunk_count"])).rowcount
        if changed != 1:
            raise RuntimeError("index checkpoint lost ownership or artifact identity")
        if job["row_format"] == 2:
            conn.execute("""UPDATE file_segments s SET indexed_at=COALESCE(indexed_at,NOW())
                FROM extraction_segments m WHERE m.segment_id=s.id AND m.extraction_id=%s
                  AND m.ordinal_start>=%s-63 AND m.ordinal_start<%s
                  AND m.ordinal_start+s.chunk_count<=%s AND s.retired_at IS NULL""",
                (job["extraction_id"], start, end, end))


def yield_index(job, *, delay=0, connect=database):
    """Release a checkpointed job without spending a failure attempt."""
    with connect() as conn:
        changed = conn.execute("""UPDATE file_work SET status='pending',
            attempt_count=GREATEST(0,attempt_count-1),attempt_token=NULL,lease_until=NULL,
            next_attempt_at=NOW()+make_interval(secs=>%s),updated_at=NOW()
            WHERE id=%s AND stage='index' AND status='running' AND attempt_token=%s
              AND lease_until>NOW()""", (delay, job["id"], job["attempt_token"])).rowcount
        if changed != 1:
            raise RuntimeError("index shutdown lost ownership")
    return "yielded"


def complete_extraction(job, chunks_ref, count, *, connect=database):
    if count < 0 or not chunks_ref.startswith(f"extractions/{job['org_id']}/{job['root_id']}/{job['extraction_id']}/"):
        raise ValueError("invalid extraction artifact")
    with connect() as conn:
        metric_count("db_write_statements")
        result = conn.execute("""WITH advanced AS (
            UPDATE file_work SET stage='index',status='pending',attempt_count=0,
                attempt_token=NULL,lease_until=NULL,next_attempt_at=NOW(),error='',updated_at=NOW()
            WHERE id=%s AND attempt_token=%s AND status='running' AND lease_until>NOW()
            RETURNING extraction_id
        ) UPDATE file_extractions e SET status='complete',chunks_ref=%s,chunk_count=%s,source_verified=TRUE,
            error='',updated_at=NOW() FROM advanced a WHERE e.id=a.extraction_id RETURNING e.id""",
            (job["id"], job["attempt_token"], chunks_ref, count)).fetchone()
        if result is None:
            raise RuntimeError("work attempt lost ownership before completion")


def retire_exhausted_work(*, connect=database):
    with connect() as conn:
        return conn.execute("""UPDATE file_work SET status='failed',lease_until=NULL,
            attempt_token=NULL,error='worker attempt limit exhausted',updated_at=NOW()
            WHERE status IN ('pending','running') AND attempt_count>=%s
              AND (lease_until IS NULL OR lease_until<=NOW())""", (MAX_WORK_ATTEMPTS,)).rowcount
