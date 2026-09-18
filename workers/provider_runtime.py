"""Short batch coordination transactions and renewable worker ownership."""

import threading
import uuid
from contextlib import contextmanager

from file_runtime import database


@contextmanager
def batch_lease(batch, *, connect=database):
    stopped, errors = threading.Event(), []

    def renew():
        while not stopped.wait(60):
            try:
                with connect() as conn:
                    changed = conn.execute("""UPDATE provider_batches SET lease_until=NOW()+INTERVAL '5 minutes'
                        WHERE id=%s AND lease_token=%s AND lease_until>NOW()""",
                        (batch["id"], batch["lease_token"])).rowcount
                if changed != 1:
                    raise RuntimeError("provider batch lease lost")
            except Exception as error:
                errors.append(error)
                return

    thread = threading.Thread(target=renew, daemon=True)
    thread.start()
    try:
        yield
        if errors:
            raise errors[0]
    finally:
        stopped.set()
        thread.join(timeout=20)
        with connect() as conn:
            conn.execute("""UPDATE provider_batches SET lease_token=NULL,lease_until=NULL,
                next_check_at=NOW()+INTERVAL '1 minute',updated_at=NOW()
                WHERE id=%s AND lease_token=%s""", (batch["id"], batch["lease_token"]))


def claim_batch(batch_id, *, connect=database):
    with connect() as conn:
        return conn.execute("""UPDATE provider_batches SET lease_token=%s,lease_until=NOW()+INTERVAL '5 minutes'
            WHERE id=%s AND (lease_until IS NULL OR lease_until<=NOW()) RETURNING *""",
            (uuid.uuid4().hex, batch_id)).fetchone()


def claim_due_batch(*, connect=database):
    with connect() as conn:
        return conn.execute("""WITH due AS (
            SELECT b.id FROM provider_batches b
            WHERE b.next_check_at<=NOW() AND (b.lease_until IS NULL OR b.lease_until<=NOW())
              AND (b.status='submitted' OR (b.status='preparing' AND
                (b.submission_started_at IS NOT NULL OR b.attempt_count>1
                 OR NOT EXISTS (SELECT 1 FROM file_work w WHERE w.extraction_id=b.extraction_id
                     AND w.stage='transform' AND w.status IN ('pending','running'))))
                OR (b.status='retry' AND NOT EXISTS (SELECT 1 FROM file_work w
                    WHERE w.extraction_id=b.extraction_id AND w.stage='transform'
                    AND w.status IN ('pending','running'))))
            ORDER BY b.next_check_at,b.id LIMIT 1 FOR UPDATE OF b SKIP LOCKED
        ) UPDATE provider_batches b SET lease_token=%s,lease_until=NOW()+INTERVAL '5 minutes'
            FROM due WHERE b.id=due.id RETURNING b.*""", (uuid.uuid4().hex,)).fetchone()


def claim_assembly(*, connect=database):
    with connect() as conn:
        return conn.execute("""WITH due AS (
            SELECT id FROM file_work WHERE stage='transform' AND status='waiting_provider'
                AND (lease_until IS NULL OR lease_until<=NOW())
            ORDER BY updated_at,id LIMIT 1 FOR UPDATE SKIP LOCKED
        ) UPDATE file_work w SET attempt_token=%s,lease_until=NOW()+INTERVAL '5 minutes'
            FROM due WHERE w.id=due.id RETURNING w.*""", (uuid.uuid4().hex,)).fetchone()


@contextmanager
def assembly_lease(work, *, connect=database):
    stopped, errors = threading.Event(), []

    def renew():
        while not stopped.wait(60):
            try:
                with connect() as conn:
                    changed = conn.execute("""UPDATE file_work SET lease_until=NOW()+INTERVAL '5 minutes'
                        WHERE id=%s AND attempt_token=%s AND status='waiting_provider' AND lease_until>NOW()""",
                        (work["id"], work["attempt_token"])).rowcount
                if changed != 1:
                    raise RuntimeError("provider assembly lease lost")
            except Exception as error:
                errors.append(error)
                return

    thread = threading.Thread(target=renew, daemon=True)
    thread.start()
    try:
        yield
        if errors:
            raise errors[0]
    finally:
        stopped.set()
        thread.join(timeout=20)
        with connect() as conn:
            conn.execute("""UPDATE file_work SET lease_until=NOW()+INTERVAL '1 minute',
                attempt_token=NULL,updated_at=NOW() WHERE id=%s AND attempt_token=%s AND status='waiting_provider'""",
                (work["id"], work["attempt_token"]))
