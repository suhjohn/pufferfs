"""Independent query deployment. No S3, Postgres, queue or provider credentials."""

import os
import modal

from nomic_model import cache_model

app = modal.App(os.getenv("PUFFERFS_QUERY_APP_NAME", "pufferfs-query"))
image = (modal.Image.debian_slim(python_version="3.12")
         .pip_install("sentence-transformers>=3", "torch>=2", "einops>=0.7", "fastapi[standard]")
         .add_local_file("nomic_model.py", "/root/nomic_model.py", copy=True)
         .add_local_file("role_auth.py", "/root/role_auth.py", copy=True)
         .run_function(cache_model))
endpoint_secret = modal.Secret.from_name(
    os.getenv("PUFFERFS_MODAL_ENDPOINT_SECRET_NAME", "pufferfs-endpoint-auth"))


@app.cls(image=image, secrets=[endpoint_secret],
         gpu=os.getenv("PUFFERFS_MODAL_QUERY_EMBED_GPU", "L4"), cpu=2, memory=4096,
         timeout=300, scaledown_window=900,
         min_containers=int(os.getenv("PUFFERFS_MODAL_QUERY_EMBED_MIN_CONTAINERS", "1")),
         max_containers=int(os.getenv("PUFFERFS_MODAL_QUERY_EMBED_MAX_CONTAINERS", "2")))
class QueryEmbedder:
    @modal.enter()
    def load_model(self):
        from nomic_model import load_model
        self.model, self.device = load_model(os.getenv("PUFFERFS_EMBEDDING_DEVICE", "cuda"))
        print(f"Query model ready: device={self.device}, dtype={next(self.model.parameters()).dtype}", flush=True)

    @modal.fastapi_endpoint(method="POST", label=os.getenv("PUFFERFS_QUERY_ENDPOINT_LABEL", "pufferfs-query-embed"))
    def embed_query_endpoint(self, item: dict) -> dict:
        from fastapi import HTTPException
        from nomic_model import encode_texts
        from role_auth import require_endpoint_secret

        require_endpoint_secret(item)
        texts = item.get("texts")
        if (not isinstance(texts, list) or not 1 <= len(texts) <= 64
                or any(not isinstance(text, str) for text in texts)):
            raise HTTPException(status_code=400, detail="texts must contain 1..64 strings")
        try:
            size = sum(len(text.encode("utf-8")) for text in texts)
        except UnicodeEncodeError as error:
            raise HTTPException(status_code=400, detail="texts must be valid UTF-8") from error
        if size > 1 << 20:
            raise HTTPException(status_code=413, detail="query text exceeds 1 MiB")
        return {"embeddings": encode_texts(self.model, self.device, texts, "search_query: ")}
