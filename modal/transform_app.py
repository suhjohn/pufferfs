"""Deploy role: `cd modal && modal deploy transform_app.py`."""

import os
from contextlib import closing

import modal

from worker_image import cpu_image, endpoint_secret, worker_secret

app = modal.App(os.getenv("PUFFERFS_TRANSFORM_APP_NAME", "pufferfs-transform"))
MAX_INPUTS = int(os.getenv("PUFFERFS_TRANSFORM_INPUTS_PER_CONTAINER", "1"))
if not 1 <= MAX_INPUTS <= 16:
    raise ValueError("PUFFERFS_TRANSFORM_INPUTS_PER_CONTAINER must be 1..16")


@app.function(
    image=cpu_image.env({"PUFFERFS_TRANSFORM_INPUTS_PER_CONTAINER": str(MAX_INPUTS)}),
    secrets=[worker_secret, endpoint_secret],
    cpu=2, memory=4096, timeout=3600,
    region=os.getenv("PUFFERFS_MODAL_WORKER_REGION") or None,
    cloud=os.getenv("PUFFERFS_MODAL_WORKER_CLOUD") or None,
    max_containers=int(os.getenv("PUFFERFS_TRANSFORM_MAX_CONTAINERS", "32")),
)
@modal.concurrent(max_inputs=MAX_INPUTS)
@modal.fastapi_endpoint(method="POST", label="pufferfs-transform-file")
def transform_file(item: dict) -> dict:
    from aws_clients import client

    from role_auth import require_work_request
    from transform_worker import transform

    work, token = require_work_request(item)
    with closing(client("s3")) as s3, closing(client("sqs")) as sqs:
        return transform(work, token, s3, sqs)
