"""Independent scheduled maintenance: modal deploy reconciliation_app.py."""

import os

import modal

app = modal.App(os.getenv("PUFFERFS_RECONCILIATION_APP_NAME", "pufferfs-reconciliation"))
image = (
    modal.Image.from_registry("python:3.12-slim-trixie")
    .apt_install("ca-certificates")
    .pip_install("boto3>=1.34.0", "psycopg[binary]>=3.2,<4", "psycopg-pool>=3.2,<4", "turbopuffer>=2.9,<3")
    .env({"SSL_CERT_FILE": "/etc/ssl/certs/ca-certificates.crt"})
    .add_local_file("file_runtime.py", "/root/file_runtime.py", copy=True)
    .add_local_file("worker_metrics.py", "/root/worker_metrics.py", copy=True)
    .add_local_file("file_reconciliation.py", "/root/file_reconciliation.py", copy=True)
)
for module in ("aws_clients", "root_cleanup", "index_cleanup", "index_client", "index_routing", "source_io", "embedding_cleanup", "artifact_cleanup", "source_cleanup"):
    image = image.add_local_file(f"{module}.py", f"/root/{module}.py", copy=True)
secret = modal.Secret.from_name(os.getenv("PUFFERFS_WORKER_SECRET_NAME", "pufferfs-workers"))


@app.function(image=image, secrets=[secret], cpu=1, memory=512, timeout=180,
              max_containers=1, schedule=modal.Period(minutes=1))
def reconcile():
    from contextlib import closing
    from aws_clients import client
    from botocore.config import Config
    from file_reconciliation import reconcile_file_work
    from index_cleanup import cleanup_index
    from root_cleanup import cleanup_deleted_roots
    from embedding_cleanup import cleanup_embeddings
    from artifact_cleanup import cleanup_obsolete_extractions
    from source_cleanup import cleanup_source_packs
    from index_client import SCHEMA, turbopuffer_client

    # Bounded network operations; the next schedule repairs an interrupted send.
    sqs = client("sqs", config=Config(connect_timeout=10, read_timeout=20,
                                          retries={"mode": "standard", "total_max_attempts": 2}))
    try:
        result = reconcile_file_work(sqs)
        with closing(client("s3", config=Config(connect_timeout=10, read_timeout=20,
                                                      retries={"total_max_attempts": 2}))) as s3:
            with turbopuffer_client(timeout=20, max_retries=0) as tp:
                def apply(namespace, mutation, vector_disabled):
                    options = {"schema": SCHEMA}
                    if not vector_disabled:
                        options["distance_metric"] = "cosine_distance"
                    response = tp.namespace(namespace).write(**mutation, **options)
                    return getattr(response, "rows_remaining", None)

                result["root_cleanup"] = cleanup_deleted_roots(s3, os.environ["AWS_BUCKET_NAME"], apply)
                result["index_cleanup"] = cleanup_index(s3, os.environ["AWS_BUCKET_NAME"], apply)
                result["embedding_cleanup"] = cleanup_embeddings(s3, os.environ["AWS_BUCKET_NAME"])
                result["artifact_cleanup"] = cleanup_obsolete_extractions(s3, os.environ["AWS_BUCKET_NAME"])
                result["source_cleanup"] = cleanup_source_packs(s3, os.environ["AWS_BUCKET_NAME"])
        print(result, flush=True)
        return result
    finally:
        sqs.close()
