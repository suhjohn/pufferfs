"""Deploy CPU/no-vector index role: modal deploy index_cpu_app.py."""

import os
import modal
from index_image import index_image, worker_secret, endpoint_secret

app = modal.App("pufferfs-index-cpu")


@app.function(image=index_image, secrets=[worker_secret, endpoint_secret], cpu=2, memory=4096,
              timeout=3600, max_containers=int(os.getenv("PUFFERFS_MODAL_INDEX_MAX_CONTAINERS", "16")))
@modal.fastapi_endpoint(method="POST", label="pufferfs-file-index-cpu")
def index(item: dict):
    from aws_clients import client
    from index_worker import index_file, turbopuffer_client
    from role_auth import require_work_request

    work, token = require_work_request(item)
    with turbopuffer_client() as tp:
        return index_file(work, token, None, client("s3"), os.environ["AWS_BUCKET_NAME"], tp, cpu_only=True)
