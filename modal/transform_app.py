"""Deploy role: `cd modal && modal deploy transform_app.py`."""

import os

import modal

from worker_image import cpu_image, endpoint_secret, worker_secret

app = modal.App(os.getenv("PUFFERFS_TRANSFORM_APP_NAME", "pufferfs-transform"))


@app.function(
    image=cpu_image, secrets=[worker_secret, endpoint_secret],
    cpu=2, memory=4096, timeout=3600,
    max_containers=int(os.getenv("PUFFERFS_TRANSFORM_MAX_CONTAINERS", "32")),
)
@modal.fastapi_endpoint(method="POST", label="pufferfs-transform-file")
def transform_file(item: dict) -> dict:
    import boto3
    from google import genai

    from role_auth import require_work_request
    from transform_worker import transform

    work, token = require_work_request(item)
    with genai.Client(api_key=os.environ["GEMINI_API_KEY"],
                      http_options={"timeout": 60000, "retry_options": {"attempts": 1}}) as provider:
        return transform(work, token, boto3.client("s3"), boto3.client("sqs"), provider=provider)
