"""Deploy CPU/no-vector index role: modal deploy index_cpu_app.py."""

import os
import modal
from index_image import index_image, worker_secret, endpoint_secret

app = modal.App("pufferfs-index-cpu")
MAX_INPUTS = int(os.getenv("PUFFERFS_INDEX_INPUTS_PER_CONTAINER", "1"))
if not 1 <= MAX_INPUTS <= 16:
    raise ValueError("PUFFERFS_INDEX_INPUTS_PER_CONTAINER must be 1..16")


@app.function(image=index_image.env({"PUFFERFS_INDEX_INPUTS_PER_CONTAINER": str(MAX_INPUTS)}),
              secrets=[worker_secret, endpoint_secret], cpu=2, memory=4096,
              timeout=3600, max_containers=int(os.getenv("PUFFERFS_MODAL_INDEX_MAX_CONTAINERS", "16")))
@modal.concurrent(max_inputs=MAX_INPUTS)
@modal.fastapi_endpoint(method="POST", label="pufferfs-file-index-cpu")
def index(item: dict):
    from contextlib import closing
    from aws_clients import client
    from index_worker import index_file, turbopuffer_client
    from role_auth import require_work_request

    work, token = require_work_request(item)
    with closing(client("s3")) as s3, turbopuffer_client() as tp:
        return index_file(work, token, None, s3, os.environ["AWS_BUCKET_NAME"], tp, cpu_only=True)
