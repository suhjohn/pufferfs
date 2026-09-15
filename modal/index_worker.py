"""One index work ID -> durable preparation -> replay -> catalog publication."""

import threading

from file_runtime import claim_work, database, fail_attempt, heartbeat
from index_prepare import prepare_mutations
from index_routing import namespace_for_path
from index_publish import publish_mutations
from index_client import write_options, turbopuffer_client
from worker_metrics import profile_work, timed, count


@profile_work("index")
def index_file(work, token, s3, bucket, tp, *, connect=database):
    job = claim_work(work, "index", token, connect=connect)
    if job["status"] != "running":
        return {"status": job["status"]}
    count("source_bytes", job["size_bytes"])
    count("chunks", job["chunk_count"])
    stopped = threading.Event()

    def renew():
        while not stopped.wait(60):
            try:
                heartbeat(job, connect=connect)
            except Exception:
                # Every durable transition also checks ownership; this thread
                # must not reset state owned by a replacement attempt.
                stopped.set()
                return

    thread = threading.Thread(target=renew, daemon=True)
    thread.start()
    try:
        # The claim supplies routing and replay progress for this attempt.
        namespace = namespace_for_path(job["namespaces"], job["file_path"])
        with timed("prepare_mutations"):
            ref, batches = prepare_mutations(job, namespace, s3, bucket, connect=connect)

        def apply(namespace, mutation):
            options = write_options(job["vector_disabled"])
            for attempt in range(100):
                if stopped.is_set():
                    raise RuntimeError("index lease renewal failed")
                with timed("search_write"):
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

        with timed("publish_mutations"):
            status = publish_mutations(job, namespace, ref, batches, apply, s3, bucket, connect=connect)
        return {"status": status}
    except Exception as error:
        fail_attempt(job, error, connect=connect)
        raise
    finally:
        stopped.set()
        thread.join(timeout=20)
