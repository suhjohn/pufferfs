"""Deploy bulk Nomic GPU role: modal deploy index_gpu_app.py. Query pool stays separate."""

import os
import modal
from index_image import index_image, worker_secret, endpoint_secret
from nomic_model import cache_model


gpu_image = index_image.pip_install("sentence-transformers>=3", "torch>=2", "einops>=0.7").run_function(cache_model)
app = modal.App(os.getenv("PUFFERFS_INDEX_GPU_APP_NAME", "pufferfs-index-gpu"))


@app.cls(image=gpu_image, secrets=[worker_secret, endpoint_secret],
         gpu=os.getenv("PUFFERFS_MODAL_EMBED_GPU", "L4"), cpu=2, memory=4096, timeout=3600,
         max_containers=int(os.getenv("PUFFERFS_MODAL_INDEX_MAX_CONTAINERS", "16")), scaledown_window=900)
class Indexer:
    @modal.enter()
    def load(self):
        from nomic_model import load_model
        from index_worker import turbopuffer_client
        # Compose can exercise the same pinned model on CPU. Production keeps
        # CUDA by default; this is not a different encoder or fake vector path.
        self.device = os.getenv("PUFFERFS_EMBEDDING_DEVICE", "cuda")
        self.model, self.device = load_model(self.device)
        print(f"Bulk model ready: device={self.device}, dtype={next(self.model.parameters()).dtype}", flush=True)
        self.tp = turbopuffer_client()

    @modal.exit()
    def close(self):
        self.tp.close()

    @modal.fastapi_endpoint(method="POST", label=os.getenv("PUFFERFS_INDEX_GPU_ENDPOINT_LABEL", "pufferfs-file-index-gpu"))
    def index(self, item: dict):
        import boto3
        from index_worker import index_file
        from role_auth import require_work_request
        work, token = require_work_request(item)

        def encode(texts):
            return self.model.encode(["search_document: " + text for text in texts],
                normalize_embeddings=True, show_progress_bar=False, batch_size=64, device=self.device).tolist()

        return index_file(work, token, encode, boto3.client("s3"), os.environ["AWS_BUCKET_NAME"], self.tp)
