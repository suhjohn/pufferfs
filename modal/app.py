"""PufferFs Modal app — file chunking and embedding functions."""

from __future__ import annotations

import os
from dataclasses import asdict

import modal

from models import Chunk, ChunkWithEmbedding

app = modal.App("pufferfs")

# ---------------------------------------------------------------------------
# Container images
# ---------------------------------------------------------------------------

chunking_image = (
    modal.Image.debian_slim(python_version="3.12")
    .pip_install(
        "boto3>=1.34.0",
        "pymupdf>=1.24.0",
        "python-docx>=1.1.0",
        "python-pptx>=0.6.23",
        "Pillow>=10.0.0",
    )
    .add_local_file("models.py", "/root/models.py")
    .add_local_file("chunkers.py", "/root/chunkers.py")
)

embedding_image = (
    modal.Image.debian_slim(python_version="3.12")
    .pip_install(
        "sentence-transformers>=3.0.0",
        "torch>=2.0.0",
    )
    .add_local_file("models.py", "/root/models.py")
)

# ---------------------------------------------------------------------------
# Secrets (S3 credentials)
# ---------------------------------------------------------------------------

s3_secret = modal.Secret.from_name(
    "pufferfs-s3",
    required_keys=[
        "AWS_ACCESS_KEY_ID",
        "AWS_SECRET_ACCESS_KEY",
        "AWS_ENDPOINT_URL",
        "AWS_BUCKET_NAME",
    ],
)

# ---------------------------------------------------------------------------
# file_to_chunks: convert a file to a list of Chunk dicts
# ---------------------------------------------------------------------------


@app.function(
    image=chunking_image,
    secrets=[s3_secret],
    timeout=600,
    memory=2048,
)
def file_to_chunks(
    s3_key: str,
    file_path: str,
    file_type: str,
    root_id: str,
) -> list[dict]:
    """Download a file from S3, chunk it based on type, return Chunk dicts."""
    import boto3
    from chunkers import (
        CODE_EXTENSIONS,
        chunk_code,
        chunk_docx,
        chunk_image,
        chunk_markdown,
        chunk_pdf,
        chunk_pptx,
        detect_file_type,
    )

    # Resolve file type if not provided
    if not file_type or file_type == "auto":
        file_type = detect_file_type(file_path)

    # Set up S3 client
    s3 = boto3.client(
        "s3",
        endpoint_url=os.environ.get("AWS_ENDPOINT_URL"),
        aws_access_key_id=os.environ["AWS_ACCESS_KEY_ID"],
        aws_secret_access_key=os.environ["AWS_SECRET_ACCESS_KEY"],
    )
    bucket = os.environ["AWS_BUCKET_NAME"]

    # Download the file
    resp = s3.get_object(Bucket=bucket, Key=s3_key)
    file_bytes = resp["Body"].read()

    # Route to the right chunker
    chunks: list[Chunk] = []

    if file_type == "pdf":
        chunks = chunk_pdf(file_bytes, root_id, file_path, s3, bucket)
    elif file_type == "docx":
        chunks = chunk_docx(file_bytes, root_id, file_path)
    elif file_type == "pptx":
        chunks = chunk_pptx(file_bytes, root_id, file_path)
    elif file_type == "image":
        chunks = chunk_image(file_bytes, root_id, file_path, s3, bucket)
    elif file_type in CODE_EXTENSIONS:
        text = file_bytes.decode("utf-8", errors="replace")
        # Also upload the extracted text as markdown
        md_key = f"files/{root_id}/{file_path}.md"
        s3.put_object(Bucket=bucket, Key=md_key, Body=text.encode(), ContentType="text/markdown")
        chunks = chunk_code(text, root_id, file_path, file_type)
    else:
        # Default: treat as text / markdown
        text = file_bytes.decode("utf-8", errors="replace")
        md_key = f"files/{root_id}/{file_path}.md"
        s3.put_object(Bucket=bucket, Key=md_key, Body=text.encode(), ContentType="text/markdown")
        chunks = chunk_markdown(text, root_id, file_path, file_type)

    return [asdict(c) for c in chunks]


# ---------------------------------------------------------------------------
# chunks_to_embeddings: embed a batch of chunks
# ---------------------------------------------------------------------------

EMBEDDING_MODEL = "BAAI/bge-base-en-v1.5"
EMBEDDING_DIM = 768


@app.cls(
    image=embedding_image,
    gpu="T4",
    timeout=600,
    memory=4096,
    container_idle_timeout=300,
)
class Embedder:
    """Persistent embedding model container."""

    @modal.enter()
    def load_model(self):
        from sentence_transformers import SentenceTransformer

        self.model = SentenceTransformer(EMBEDDING_MODEL)

    @modal.method()
    def embed_chunks(self, chunk_dicts: list[dict]) -> list[dict]:
        """Embed a batch of Chunk dicts, return ChunkWithEmbedding dicts."""
        if not chunk_dicts:
            return []

        texts = [c["content"] for c in chunk_dicts]
        embeddings = self.model.encode(texts, normalize_embeddings=True, show_progress_bar=False)

        results: list[dict] = []
        for chunk_dict, emb in zip(chunk_dicts, embeddings):
            chunk = Chunk(**chunk_dict)
            cwe = ChunkWithEmbedding(chunk=chunk, embedding=emb.tolist())
            results.append(asdict(cwe))

        return results

    @modal.method()
    def embed_texts(self, texts: list[str]) -> list[list[float]]:
        """Embed a list of raw text strings. Used for query embedding."""
        if not texts:
            return []
        embeddings = self.model.encode(texts, normalize_embeddings=True, show_progress_bar=False)
        return [emb.tolist() for emb in embeddings]


# ---------------------------------------------------------------------------
# Convenience web endpoints for the Go server to call
# ---------------------------------------------------------------------------


@app.function(
    image=chunking_image,
    secrets=[s3_secret],
    timeout=600,
    memory=2048,
)
@modal.web_endpoint(method="POST")
def chunk_file_endpoint(item: dict) -> dict:
    """HTTP endpoint: POST {s3_key, file_path, file_type, root_id} -> {chunks: [...]}"""
    chunks = file_to_chunks.local(
        s3_key=item["s3_key"],
        file_path=item["file_path"],
        file_type=item.get("file_type", "auto"),
        root_id=item["root_id"],
    )
    return {"chunks": chunks, "count": len(chunks)}


@app.function(
    image=embedding_image,
    gpu="T4",
    timeout=600,
    memory=4096,
)
@modal.web_endpoint(method="POST")
def embed_chunks_endpoint(item: dict) -> dict:
    """HTTP endpoint: POST {chunks: [...]} -> {results: [...]}"""
    embedder = Embedder()
    results = embedder.embed_chunks.local(item["chunks"])
    return {"results": results, "count": len(results)}


@app.function(
    image=embedding_image,
    gpu="T4",
    timeout=600,
    memory=4096,
)
@modal.web_endpoint(method="POST")
def embed_query_endpoint(item: dict) -> dict:
    """HTTP endpoint: POST {texts: [...]} -> {embeddings: [...]}"""
    embedder = Embedder()
    embeddings = embedder.embed_texts.local(item["texts"])
    return {"embeddings": embeddings}
