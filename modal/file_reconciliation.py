"""Repair delivery state; SQS remains responsible for execution and retries."""

from file_runtime import database, publish_pending


def reconcile_file_work(sqs, *, connect=database, limit: int = 100) -> dict:
    if not 1 <= limit <= 1000:
        raise ValueError("invalid reconciliation scan limit")
    with connect() as conn:
        expired = conn.execute(
            """WITH expired AS (
                 SELECT id FROM file_work
                 WHERE status='running' AND lease_until<=NOW()
                 ORDER BY lease_until,id LIMIT %s FOR UPDATE SKIP LOCKED
               )
               UPDATE file_work w SET status='pending',attempt_token=NULL,
                   lease_until=NULL,updated_at=NOW(),
                   error='attempt lease expired; awaiting SQS redelivery'
               FROM expired WHERE w.id=expired.id RETURNING w.id""", (limit,),
        ).fetchall()
    # Preserve enqueued_at and all durable outputs/progress. A delivered
    # receipt may be invisible, queued or in its DLQ; an old timestamp does
    # not prove it was lost. Do not send a fresh message/reset receive counts.
    # Null enqueued_at is the separate committed-but-unconfirmed handoff case.
    delivered = publish_pending(sqs, connect=connect, limit=limit)
    return {"expired_attempts": len(expired), "published_handoffs": delivered}
