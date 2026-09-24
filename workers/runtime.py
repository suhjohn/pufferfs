"""Production worker entrypoint: ingestion or background, with bounded concurrency."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
import os
from pathlib import Path
import signal
import threading
import time

from aws_clients import client
from file_runtime import claim_due_work, fail_attempt, work_lease, yield_index
from index_client import turbopuffer_client
from provider_capacity import ProviderDeferred, limits as embedding_limits


def process_files(stage, stopped):
    from transform_worker import transform
    from index_worker import publish_extraction

    while not stopped.is_set():
        job = None
        try:
            job = claim_due_work(stage)
            if job is None:
                stopped.wait(2)
                continue
            with work_lease(job) as check_lease, closing(client("s3")) as s3:
                if stage == "transform":
                    transform(job, s3, check_lease, stopping=stopped)
                else:
                    # Each actual native embedding attempt must be admitted;
                    # hidden SDK retries would spend unreserved capacity.
                    with turbopuffer_client(max_retries=0) as tp:
                        publish_extraction(job, s3, os.environ["AWS_BUCKET_NAME"], tp, check_lease, stopped)
        except ProviderDeferred as error:
            if job is not None:
                try:
                    yield_index(job, delay=error.delay)
                except Exception as failure:
                    print(f"capacity deferral recovery deferred: {type(failure).__name__}", flush=True)
        except Exception as error:
            print(f"{stage} attempt failed: {type(error).__name__}", flush=True)
            if job is not None:
                try:
                    fail_attempt(job, error)
                except Exception as failure:
                    print(f"attempt recovery deferred: {type(failure).__name__}", flush=True)
            stopped.wait(2)


def periodic(operation, stopped):
    while not stopped.is_set():
        started = time.monotonic()
        try:
            operation()
        except Exception as error:
            print(f"{operation.__name__} deferred: {type(error).__name__}", flush=True)
        stopped.wait(max(1, 60 - (time.monotonic() - started)))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("role", choices=("ingestion", "background"))
    args = parser.parse_args()
    concurrency = int(os.environ.get("PUFFERFS_WORKER_CONCURRENCY", "4"))
    if not 1 <= concurrency <= 64:
        raise ValueError("worker concurrency must be 1..64")
    embedding_limits()
    for name in ("DATABASE_URL", "AWS_BUCKET_NAME", "GEMINI_API_KEY", "TURBOPUFFER_API_KEY"):
        if not os.environ.get(name):
            raise ValueError(f"{name} is required")
    stopped = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stopped.set())
    signal.signal(signal.SIGINT, lambda *_: stopped.set())
    with ThreadPoolExecutor(max_workers=concurrency + 2) as executor:
        stage = "transform" if args.role == "ingestion" else "index"
        futures = [executor.submit(process_files, stage, stopped) for _ in range(concurrency)]
        if args.role == "background":
            from collection import collect
            from maintenance import reconcile
            futures.extend(executor.submit(periodic, task, stopped) for task in (collect, reconcile))
        while not stopped.is_set():
            if any(future.done() for future in futures):
                stopped.set()
                raise RuntimeError("worker loop exited unexpectedly")
            Path("/tmp/role-heartbeat").touch()
            stopped.wait(10)


if __name__ == "__main__":
    main()
