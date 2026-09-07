"""Per-file transformation entrypoint, independent of the API server."""

from __future__ import annotations

import os
import threading
import tempfile
from pathlib import Path

from extraction import file_family, text_chunks
from file_runtime import claim_work, complete_extraction, fail_attempt, heartbeat, publish_deliveries
from source_io import iter_source_bytes, materialize_source, read_manifest, write_chunks
from spreadsheet_extract import spreadsheet_chunks
from structured_extract import structured_chunks
from provider_prepare import prepare_provider
from worker_metrics import profile_work, timed, count as metric_count


@profile_work("transform")
def transform(work_id: str, token: str, s3, sqs, *, provider=None) -> dict:
    job = claim_work(work_id, "transform", token)
    if job["status"] != "running":
        return {"status": job["status"]}
    stopped = threading.Event()
    errors = []

    def renew():
        while not stopped.wait(60):
            try:
                heartbeat(job)
            except Exception as error:
                errors.append(error)
                return

    thread = threading.Thread(target=renew, daemon=True)
    thread.start()
    try:
        bucket = os.environ["AWS_BUCKET_NAME"]
        with timed("source_manifest"):
            manifest = read_manifest(s3, bucket, job)
        metric_count("source_bytes", job["size_bytes"])
        family = file_family(job["file_path"])
        prefix = f"extractions/{job['org_id']}/{job['root_id']}/{job['extraction_id']}/chunks"
        if family not in {"text", "jsonl", "unknown"}:
            with tempfile.TemporaryDirectory() as directory:
                path = os.path.join(directory, "source" + Path(job["file_path"]).suffix.lower())
                materialize_source(s3, bucket, manifest, path)
                if family == "spreadsheet":
                    key, count = write_chunks(s3, bucket, prefix, spreadsheet_chunks(path))
                elif family == "structured":
                    key, count = write_chunks(s3, bucket, prefix, structured_chunks(path))
                else:
                    if provider is None:
                        from google import genai

                        with genai.Client(api_key=os.environ["GEMINI_API_KEY"],
                                http_options={"timeout": 60000, "retry_options": {"attempts": 1}}) as client:
                            requests = prepare_provider(job, path, client, s3, bucket)
                    else:
                        requests = prepare_provider(job, path, provider, s3, bucket)
                    if requests:
                        return {"status": "waiting_provider"}
                    key, count = write_chunks(s3, bucket, prefix, [])
        else:
            with timed("text_extract"):
                key, count = write_chunks(s3, bucket, prefix, text_chunks(iter_source_bytes(s3, bucket, manifest)))
        metric_count("chunks", count)
        if errors:
            raise errors[0]
        # The completion CAS checks the live lease itself. The background
        # heartbeat handles long extraction; no extra write before completion.
        delivery = complete_extraction(job, key, count)
    except Exception as error:
        fail_attempt(job, error)
        raise
    finally:
        stopped.set()
        thread.join(timeout=20)
    # The extraction is already durable. A failed enqueue must not roll it
    # back: the collector/reconciler will publish the index delivery ledger.
    try:
        with timed("queue_publish"):
            publish_deliveries(sqs, [delivery])
    except Exception as error:
        print(f"index delivery requires reconciliation: {type(error).__name__}", flush=True)
    return {"status": "complete"}
