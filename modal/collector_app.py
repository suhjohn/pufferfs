"""Deploy independently: cd modal && modal deploy collector_app.py."""

import os

import modal

from worker_image import cpu_image, worker_secret

app = modal.App(os.getenv("PUFFERFS_COLLECTOR_APP_NAME", "pufferfs-batch-collector"))


@app.function(image=cpu_image, secrets=[worker_secret], cpu=2, memory=4096,
              timeout=900, max_containers=1, schedule=modal.Period(minutes=1))
def collect():
    from aws_clients import client as aws_client
    from google import genai

    from batch_collector import assemble_extraction, collect_batch
    from file_runtime import database, publish_pending
    from provider_submission import reconcile_submission, submit_batch
    from provider_retry import reserve_retry
    from provider_refresh import refresh_batch_inputs
    from provider_cleanup import cleanup_provider_files

    s3, sqs = aws_client("s3"), aws_client("sqs")
    bucket = os.environ["AWS_BUCKET_NAME"]
    # No implicit SDK replay of a potentially accepted paid create. Explicit
    # reconciliation owns retries; reads can safely retry on the next schedule.
    with genai.Client(api_key=os.environ["GEMINI_API_KEY"],
                      http_options={"timeout": 60000, "retry_options": {"attempts": 1}}) as client:
        with database() as conn:
            failures = conn.execute("""SELECT b.id FROM provider_batches b WHERE b.status='failed'
                AND EXISTS (SELECT 1 FROM provider_requests p JOIN file_extractions e ON e.id=p.extraction_id
                    WHERE p.batch_id=b.id AND p.status='failed' AND e.status='waiting_provider')
                ORDER BY b.updated_at LIMIT 50""").fetchall()
        for batch in failures:
            try:
                reserve_retry(batch["id"])
            except Exception as error:
                print(f"provider retry reservation deferred: {type(error).__name__}", flush=True)
        with database() as conn:
            batches = conn.execute("""SELECT id,provider_job_id,submission_started_at FROM provider_batches
                WHERE status='submitted' OR (status='preparing' AND (submission_started_at IS NOT NULL OR retry_of IS NOT NULL))
                ORDER BY updated_at LIMIT 50""").fetchall()
        for batch in batches:
            try:
                if not batch["provider_job_id"]:
                    if batch["submission_started_at"] is None:
                        refresh_batch_inputs(batch["id"], client, s3=s3, bucket=bucket)
                        submit_batch(batch["id"], client)
                    else:
                        reconcile_submission(batch["id"], client)
                collect_batch(batch["id"], client, s3, bucket)
            except Exception as error:
                print(f"batch collection deferred: {type(error).__name__}", flush=True)
            finally:
                with database() as conn:
                    conn.execute("UPDATE provider_batches SET updated_at=NOW() WHERE id=%s", (batch["id"],))
        with database() as conn:
            extractions = conn.execute("""SELECT id FROM file_extractions
                WHERE status='waiting_provider' ORDER BY updated_at LIMIT 50""").fetchall()
        for extraction in extractions:
            try:
                assemble_extraction(extraction["id"], s3, bucket)
            except Exception as error:
                print(f"extraction assembly deferred: {type(error).__name__}", flush=True)
            finally:
                with database() as conn:
                    conn.execute("UPDATE file_extractions SET updated_at=NOW() WHERE id=%s", (extraction["id"],))
        print({"provider_cleanup": cleanup_provider_files(client)}, flush=True)
    publish_pending(sqs)
