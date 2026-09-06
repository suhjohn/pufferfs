"""Retry terminal provider failures without resubmitting successful pages."""

from file_runtime import database, stable_id

MAX_REQUEST_ATTEMPTS = 3


def reserve_retry(batch_id, *, connect=database):
    # Only sealed provider handoffs can retry here. Initial preparation remains
    # owned by the transform SQS delivery and its attempt lease.
    with connect() as conn:
        source = conn.execute("""SELECT e.id AS extraction_id,e.version_id,f.id AS file_id,f.root_id
            FROM provider_requests p JOIN file_extractions e ON e.id=p.extraction_id
            JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
            WHERE p.batch_id=%s AND p.status='failed' LIMIT 1""", (batch_id,)).fetchone()
        if source is None:
            return None
        root = conn.execute("SELECT deleting_at FROM roots WHERE id=%s FOR UPDATE", (source["root_id"],)).fetchone()
        extraction = conn.execute("SELECT status FROM file_extractions WHERE id=%s FOR UPDATE", (source["extraction_id"],)).fetchone()
        if root is None or extraction is None or extraction["status"] != "waiting_provider":
            return None
        file = conn.execute("SELECT captured_version_id,deleted FROM file_catalog WHERE id=%s", (source["file_id"],)).fetchone()
        if root["deleting_at"] or file["deleted"] or file["captured_version_id"] != source["version_id"]:
            conn.execute("UPDATE file_extractions SET status='superseded',updated_at=NOW() WHERE id=%s", (source["extraction_id"],))
            conn.execute("UPDATE file_work SET status='superseded',updated_at=NOW() WHERE extraction_id=%s AND stage='transform' AND status='waiting_provider'", (source["extraction_id"],))
            return None
        batch = conn.execute("SELECT * FROM provider_batches WHERE id=%s FOR UPDATE", (batch_id,)).fetchone()
        if batch is None or batch["status"] != "failed":
            return None
        failed = conn.execute("SELECT * FROM provider_requests WHERE batch_id=%s AND status='failed' ORDER BY ordinal FOR UPDATE", (batch_id,)).fetchall()
        if not failed:
            return None
        if any(request["attempt_count"] >= MAX_REQUEST_ATTEMPTS for request in failed):
            error = "provider request retry limit reached"
            conn.execute("UPDATE file_extractions SET status='failed',error=%s,updated_at=NOW() WHERE id=%s", (error, source["extraction_id"]))
            conn.execute("UPDATE file_work SET status='failed',error=%s,updated_at=NOW() WHERE extraction_id=%s AND stage='transform' AND status='waiting_provider'", (error, source["extraction_id"]))
            return None
        retry_id = stable_id(batch_id, "retry")
        conn.execute("INSERT INTO provider_batches(id,status,model,retry_of) VALUES(%s,'preparing',%s,%s)", (retry_id, batch["model"], batch_id))
        conn.execute("""UPDATE provider_requests SET batch_id=%s,status='pending',error='',attempt_count=attempt_count+1
            WHERE batch_id=%s AND status='failed'""", (retry_id, batch_id))
    return retry_id
