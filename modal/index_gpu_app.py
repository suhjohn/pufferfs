"""Deploy bulk Nomic embedding: modal deploy index_gpu_app.py. Query pool stays separate."""

import os
import threading
import modal
from index_image import index_image, worker_secret, endpoint_secret
from nomic_model import cache_model

MAX_INPUTS = int(os.getenv("PUFFERFS_INDEX_INPUTS_PER_CONTAINER", "1"))
BATCH_SIZE = int(os.getenv("PUFFERFS_EMBED_BATCH_SIZE", "64"))
GPU = os.getenv("PUFFERFS_MODAL_EMBED_GPU", "L4")
CPU = float(os.getenv("PUFFERFS_MODAL_EMBED_CPU", "1"))
MEMORY = int(os.getenv("PUFFERFS_MODAL_EMBED_MEMORY_MIB", "6144"))
if not 1 <= MAX_INPUTS <= 16:
    raise ValueError("PUFFERFS_INDEX_INPUTS_PER_CONTAINER must be 1..16")
if not 1 <= BATCH_SIZE <= 128:
    raise ValueError("PUFFERFS_EMBED_BATCH_SIZE must be 1..128")

gpu_image = (index_image.pip_install_from_requirements("requirements-embedding.txt")
             .run_function(cache_model)
             .env({"PUFFERFS_INDEX_INPUTS_PER_CONTAINER": str(MAX_INPUTS),
                   "PUFFERFS_EMBED_BATCH_SIZE": str(BATCH_SIZE),
                   "PUFFERFS_EMBEDDING_DEVICE": "cpu" if GPU == "none" else "cuda"}))
app = modal.App(os.getenv("PUFFERFS_INDEX_GPU_APP_NAME", "pufferfs-index-gpu"))


@app.cls(image=gpu_image, secrets=[worker_secret, endpoint_secret],
         gpu=None if GPU == "none" else GPU, cpu=CPU, memory=MEMORY, timeout=3600,
         region=os.getenv("PUFFERFS_MODAL_WORKER_REGION") or None,
         cloud=os.getenv("PUFFERFS_MODAL_WORKER_CLOUD") or None,
         max_containers=int(os.getenv("PUFFERFS_MODAL_INDEX_MAX_CONTAINERS", "16")), scaledown_window=900)
@modal.concurrent(max_inputs=MAX_INPUTS)
class Indexer:
    @modal.enter()
    def load(self):
        from nomic_model import load_model
        # Compose can exercise the same pinned model on CPU. Production keeps
        # CUDA by default; this is not a different encoder or fake vector path.
        self.device = os.getenv("PUFFERFS_EMBEDDING_DEVICE", "cuda")
        self.model, self.device = load_model(self.device)
        # Nomic mutates model caches during encoding. Share one model safely;
        # independent jobs can still overlap S3, database and search IO.
        self.encode_lock = threading.Lock()
        # ASGI cancellation can release a Modal input while its synchronous
        # handler is still running. Keep the work permit in that handler until
        # all of its IO, publication and lease cleanup have actually finished.
        self.work_slots = threading.BoundedSemaphore(MAX_INPUTS)
        print(f"Bulk model ready: device={self.device}, dtype={next(self.model.parameters()).dtype}", flush=True)

    @modal.fastapi_endpoint(method="POST", label=os.getenv("PUFFERFS_INDEX_GPU_ENDPOINT_LABEL", "pufferfs-file-index-gpu"))
    def index(self, item: dict):
        from contextlib import closing
        from aws_clients import client
        from index_worker import index_file, turbopuffer_client
        from role_auth import require_work_request
        from worker_metrics import timed, count
        work, token = require_work_request(item)

        def encode(texts):
            with timed("encode_wait"):
                self.encode_lock.acquire()
            try:
                with timed("encode_run"):
                    result = self.model.encode(["search_document: " + text for text in texts],
                        normalize_embeddings=True, show_progress_bar=False,
                        batch_size=BATCH_SIZE, device=self.device).tolist()
                count("encoder_texts", len(texts))
                count("encoder_batches", (len(texts) + BATCH_SIZE - 1) // BATCH_SIZE)
                return result
            finally:
                self.encode_lock.release()

        with self.work_slots, closing(client("s3")) as s3, turbopuffer_client() as tp:
            return index_file(work, token, encode, s3, os.environ["AWS_BUCKET_NAME"], tp)
