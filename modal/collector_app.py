"""Independent, horizontally scalable provider collection/assembly workers."""

import os
import time

import modal

from worker_image import cpu_image, worker_secret

app = modal.App(os.getenv("PUFFERFS_COLLECTOR_APP_NAME", "pufferfs-batch-collector"))
# Deployment setting: one worker also means at most one live container, even
# when a slow invocation overlaps the next dispatcher tick.
COLLECTOR_WORKERS = int(os.environ.get("PUFFERFS_COLLECTOR_WORKERS", "1"))
if not 1 <= COLLECTOR_WORKERS <= 16:
    raise ValueError("PUFFERFS_COLLECTOR_WORKERS must be 1..16")
collector_image = cpu_image.env({"PUFFERFS_COLLECTOR_WORKERS": str(COLLECTOR_WORKERS)})


def advance_batch(batch, client, s3, bucket):
    from batch_collector import collect_batch
    from file_runtime import database
    from provider_cleanup import TERMINAL
    from provider_refresh import refresh_batch_inputs
    from provider_retry import reserve_retry
    from provider_submission import reconcile_submission, submit_batch

    if batch["submission_started_at"] and not batch["provider_job_id"]:
        if not reconcile_submission(batch, client):
            return
    with database() as conn:
        source = conn.execute("""SELECT e.id FROM file_extractions e
            JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
            JOIN roots r ON r.id=f.root_id WHERE e.id=%s AND r.deleting_at IS NULL
              AND NOT f.deleted AND f.captured_version_id=e.version_id
              AND e.status NOT IN ('failed','superseded')""", (batch["extraction_id"],)).fetchone()
    if source is None:
        if batch["status"] == "submitted":
            remote = client.batches.get(name=batch["provider_job_id"])
            if remote.state.name not in TERMINAL:
                client.batches.cancel(name=batch["provider_job_id"])
                return
        with database() as conn:
            conn.execute("""UPDATE provider_batches SET status='failed',error='source no longer current'
                WHERE id=%s AND lease_token=%s AND lease_until>NOW()""", (batch["id"], batch["lease_token"]))
        return
    if batch["status"] == "retry" and not reserve_retry(batch):
        return
    if batch["status"] == "preparing":
        refresh_batch_inputs(batch, client, s3, bucket)
        submit_batch(batch, client, s3, bucket)
    collect_batch(batch, client, s3, bucket)


@app.function(image=collector_image, secrets=[worker_secret], cpu=2, memory=4096,
              timeout=900, max_containers=COLLECTOR_WORKERS)
def collect():
    from aws_clients import client as aws_client
    from google import genai
    from batch_collector import assemble_extraction
    from file_runtime import publish_pending
    from provider_cleanup import claim_cleanup, cleanup_provider_files
    from provider_runtime import batch_lease, claim_due_batch, claim_assembly, assembly_lease

    s3, sqs = aws_client("s3"), aws_client("sqs")
    bucket = os.environ["AWS_BUCKET_NAME"]
    deadline = time.monotonic() + 50
    with genai.Client(api_key=os.environ["GEMINI_API_KEY"],
                     http_options={"timeout": 60000, "retry_options": {"attempts": 1}}) as client:
        # Fair turns between collection, assembly and cleanup. Database leases
        # distribute disjoint batches across processes; no per-page scans and
        # no fixed 50-batch ceiling per minute. Long individual tasks renew.
        while time.monotonic() < deadline:
            progressed = False
            batch = claim_due_batch()
            if batch:
                progressed = True
                try:
                    with batch_lease(batch):
                        advance_batch(batch, client, s3, bucket)
                except Exception as error:
                    print(f"provider batch deferred: {type(error).__name__}", flush=True)
            work = claim_assembly()
            if work:
                progressed = True
                try:
                    with assembly_lease(work):
                        assemble_extraction(work["extraction_id"], s3, bucket, work["attempt_token"])
                except Exception as error:
                    print(f"provider assembly deferred: {type(error).__name__}", flush=True)
            batch = claim_cleanup()
            if batch:
                progressed = True
                try:
                    with batch_lease(batch):
                        cleanup_provider_files(batch, client, s3, bucket)
                except Exception as error:
                    print(f"provider cleanup deferred: {type(error).__name__}", flush=True)
            if not progressed:
                break
    publish_pending(sqs)


@app.function(image=collector_image, secrets=[worker_secret], timeout=60,
              max_containers=1, schedule=modal.Period(minutes=1))
def dispatch():
    for _ in range(COLLECTOR_WORKERS):
        collect.spawn()
