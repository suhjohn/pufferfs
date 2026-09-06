"""One index work ID -> durable preparation -> replay -> catalog publication."""

import threading

from file_runtime import claim_work, database, fail_attempt, heartbeat
from index_prepare import prepare_mutations
from index_publish import publish_mutations
from index_client import SCHEMA, turbopuffer_client


def index_file(work, token, encode, s3, bucket, tp, *, cpu_only=False, connect=database):
    job = claim_work(work, "index", token, connect=connect)
    if job["status"] != "running":
        return {"status": job["status"]}
    stopped = threading.Event()

    def renew():
        while not stopped.wait(60):
            try:
                heartbeat(job, connect=connect)
            except Exception:
                # Every durable transition also checks ownership; this thread
                # must not reset state owned by a replacement attempt.
                return

    thread = threading.Thread(target=renew, daemon=True)
    thread.start()
    try:
        if cpu_only and not job["vector_disabled"]:
            raise ValueError("vector-enabled root delivered to CPU index deployment")
        prepare_mutations(job, encode, s3, bucket, connect=connect)

        def apply(namespace, mutation):
            options = {"schema": SCHEMA}
            if not job["vector_disabled"]:
                options["distance_metric"] = "cosine_distance"
            for _ in range(100):
                heartbeat(job, connect=connect)
                response = tp.namespace(namespace).write(**mutation, **options)
                if "delete_by_filter" not in mutation:
                    return
                remaining = getattr(response, "rows_remaining", None)
                if remaining is False or remaining is None:
                    return
                if remaining is not True:
                    raise ValueError("invalid deletion progress response")
            # Leave the mutation unacknowledged. SQS retry resumes the same
            # persisted filter; already deleted rows stay deleted.
            raise RuntimeError("deletion needs another bounded worker attempt")

        status = publish_mutations(job, apply, s3, bucket, connect=connect)
        return {"status": status}
    except Exception as error:
        fail_attempt(job, error, connect=connect)
        raise
    finally:
        stopped.set()
        thread.join(timeout=20)
