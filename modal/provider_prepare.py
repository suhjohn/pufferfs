"""Prepare contiguous input batches, publishing one S3 manifest per batch."""

from contextlib import closing
from itertools import islice

from file_runtime import database
from provider_manifests import MAX_BATCH_REQUESTS
from provider_refresh import prepared_inputs, upload_inputs, persist_inputs, refresh_batch_inputs
from provider_runtime import claim_batch, batch_lease
from provider_submission import batch_identity, finish_preparation, reserve_batch, submit_batch


def prepare_provider(job, path, client, s3, bucket, *, connect=database):
    count = 0
    while True:
        with connect() as conn:
            recorded = conn.execute("""SELECT * FROM provider_batches
                WHERE extraction_id=%s AND ordinal_start>=%s ORDER BY ordinal_start LIMIT 64""",
                (job["extraction_id"], count)).fetchall()
        if not recorded:
            break
        for batch in recorded:
            if batch["ordinal_start"] != count:
                raise ValueError("persisted provider ranges are not a contiguous prefix")
            if not batch["provider_job_id"]:
                batch = claim_batch(batch["id"], connect=connect)
                if batch is None:
                    raise RuntimeError("provider batch is owned by another worker")
                with batch_lease(batch, connect=connect):
                    refresh_batch_inputs(batch, client, s3, bucket, path=path, connect=connect)
                    submit_batch(batch, client, s3, bucket, connect=connect)
            count += batch["request_count"]
    with closing(prepared_inputs(path, job["revision"], range(count, 1 << 63))) as inputs:
        while True:
            requests, uploads = upload_inputs(client, islice(inputs, MAX_BATCH_REQUESTS), job["extraction_id"], 1)
            if not requests:
                break
            if [item["ordinal"] for item in requests] != list(range(count, count + len(requests))):
                raise ValueError("prepared provider inputs are not contiguous")
            batch = batch_identity(job, count, len(requests))
            ref = persist_inputs(batch, requests, uploads, client, s3, bucket)
            batch = reserve_batch(job, batch, ref, connect=connect)
            with batch_lease(batch, connect=connect):
                submit_batch(batch, client, s3, bucket, connect=connect)
            count += len(requests)
    if count:
        finish_preparation(job, count, connect=connect)
    return count
