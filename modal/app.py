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
    .apt_install("libreoffice-core", "libreoffice-writer", "libreoffice-impress")
    .pip_install(
        "boto3>=1.34.0",
        "pymupdf>=1.24.0",
        "Pillow>=10.0.0",
        "google-genai>=1.0.0",
        "fastapi[standard]",
    )
    .add_local_file("models.py", "/root/models.py", copy=True)
    .add_local_file("chunkers.py", "/root/chunkers.py", copy=True)
)

embedding_image = (
    modal.Image.debian_slim(python_version="3.12")
    .pip_install(
        "sentence-transformers>=3.0.0",
        "torch>=2.0.0",
        "einops>=0.7.0",
        "fastapi[standard]",
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

gemini_secret = modal.Secret.from_name(
    "pufferfs-gemini",
    required_keys=["GEMINI_API_KEY"],
)

# ---------------------------------------------------------------------------
# file_to_chunks: convert a file to a list of Chunk dicts
# ---------------------------------------------------------------------------


@app.function(
    image=chunking_image,
    timeout=600,
    memory=2048,
)
def office_to_pdf(file_bytes: bytes, file_type: str, file_path: str) -> bytes:
    """Convert an Office document to PDF bytes."""
    import time

    from chunkers import _convert_to_pdf

    convert_start = time.perf_counter()
    pdf_bytes = _convert_to_pdf(file_bytes, file_type)
    print(
        f"timing file={file_path} stage=office_to_pdf file_type={file_type} "
        f"bytes_in={len(file_bytes)} bytes_out={len(pdf_bytes)} elapsed={time.perf_counter() - convert_start:.3f}s",
        flush=True,
    )
    return pdf_bytes


@app.function(
    image=chunking_image,
    timeout=600,
    memory=2048,
)
def pdf_to_page_images(pdf_bytes: bytes, file_path: str) -> list[dict]:
    """Render PDF bytes to per-page JPEG images plus fallback text."""
    import time

    import fitz

    render_start = time.perf_counter()
    doc = fitz.open(stream=pdf_bytes, filetype="pdf")
    pages: list[dict] = []
    for page_num in range(len(doc)):
        page = doc[page_num]
        pix = page.get_pixmap(dpi=200)
        pages.append(
            {
                "page_num": page_num,
                "image_bytes": pix.tobytes("jpeg"),
                "fallback_text": page.get_text("text"),
            }
        )
    doc.close()
    print(
        f"timing file={file_path} stage=pdf_render pages={len(pages)} elapsed={time.perf_counter() - render_start:.3f}s",
        flush=True,
    )
    return pages


@app.function(
    image=chunking_image,
    secrets=[gemini_secret],
    timeout=600,
    memory=2048,
    min_containers=int(os.getenv("PUFFERFS_MODAL_PAGE_TEXT_MIN_CONTAINERS", "4")),
    max_containers=int(os.getenv("PUFFERFS_MODAL_PAGE_TEXT_MAX_CONTAINERS", "32")),
    scaledown_window=900,
)
def page_image_to_text(file_path: str, page_num: int, image_bytes: bytes, fallback_text: str) -> str:
    """Extract text from one rendered page image."""
    import time

    from chunkers import image_to_text

    text_start = time.perf_counter()
    gemini_key = os.environ.get("GEMINI_API_KEY", "")
    if gemini_key:
        text = image_to_text(image_bytes, gemini_key, mime_type="image/jpeg")
    else:
        text = fallback_text
    print(
        f"timing file={file_path} page={page_num} stage=image_to_text "
        f"chars={len(text)} elapsed={time.perf_counter() - text_start:.3f}s",
        flush=True,
    )
    return text


def chunk_document_with_stage_functions(
    file_bytes: bytes,
    root_id: str,
    file_path: str,
    file_type: str,
    s3_client,
    bucket: str,
) -> list[Chunk]:
    """Orchestrate split Modal functions for PDF/DOCX/PPTX chunking."""
    import concurrent.futures
    import os
    import time

    from chunkers import _page_image_key

    total_start = time.perf_counter()
    if file_type in ("docx", "pptx"):
        pdf_bytes = office_to_pdf.remote(file_bytes, file_type, file_path)
    else:
        pdf_bytes = file_bytes

    pages = pdf_to_page_images.remote(pdf_bytes, file_path)
    if not pages:
        print(
            f"timing file={file_path} stage=document_chunk_total pages=0 chunks=0 elapsed={time.perf_counter() - total_start:.3f}s",
            flush=True,
        )
        return []

    def page_to_chunk(page_data: dict) -> Chunk:
        page_start = time.perf_counter()
        page_num = int(page_data["page_num"])
        img_bytes = page_data["image_bytes"]
        fallback_text = page_data["fallback_text"]
        image_key = _page_image_key(root_id, file_path, page_num)
        if s3_client and bucket:
            upload_start = time.perf_counter()
            s3_client.put_object(
                Bucket=bucket,
                Key=image_key,
                Body=img_bytes,
                ContentType="image/jpeg",
            )
            print(
                f"timing file={file_path} page={page_num} stage=page_image_upload "
                f"bytes={len(img_bytes)} elapsed={time.perf_counter() - upload_start:.3f}s",
                flush=True,
            )

        text = page_image_to_text.remote(file_path, page_num, img_bytes, fallback_text)
        if not text.strip():
            text = f"[Page {page_num + 1}: no extractable text]"

        chunk = Chunk(
            id=Chunk.make_id(root_id, file_path, page_num),
            root_id=root_id,
            file_path=file_path,
            chunk_index=page_num,
            content=text,
            content_hash=Chunk.hash_content(text),
            file_type=file_type,
            page_number=page_num,
            image_path=image_key,
        )
        print(
            f"timing file={file_path} page={page_num} stage=page_total elapsed={time.perf_counter() - page_start:.3f}s",
            flush=True,
        )
        return chunk

    max_workers = max(1, int(os.getenv("PUFFERFS_PAGE_TEXT_WORKERS", "8")))
    max_workers = min(max_workers, len(pages))
    text_start = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers) as executor:
        chunks = list(executor.map(page_to_chunk, pages))
    print(
        f"timing file={file_path} stage=parallel_image_to_text pages={len(pages)} workers={max_workers} elapsed={time.perf_counter() - text_start:.3f}s",
        flush=True,
    )
    chunks.sort(key=lambda chunk: chunk.chunk_index)
    print(
        f"timing file={file_path} stage=document_chunk_total pages={len(pages)} chunks={len(chunks)} elapsed={time.perf_counter() - total_start:.3f}s",
        flush=True,
    )
    return chunks


@app.function(
    image=chunking_image,
    secrets=[s3_secret, gemini_secret],
    timeout=600,
    memory=2048,
)
def file_to_chunks(
    s3_key: str,
    file_path: str,
    file_type: str,
    root_id: str,
    content_b64: str | None = None,
) -> list[dict]:
    """Chunk a file. If content_b64 is provided, uses it directly; otherwise downloads from S3."""
    import base64

    import boto3
    from chunkers import (
        CODE_EXTENSIONS,
        chunk_code,
        chunk_image,
        chunk_markdown,
        detect_file_type,
    )

    if not file_type or file_type == "auto":
        file_type = detect_file_type(file_path)

    # Get file bytes: either from base64 content or S3
    s3 = None
    bucket = None
    if content_b64:
        file_bytes = base64.b64decode(content_b64)
    else:
        s3 = boto3.client(
            "s3",
            endpoint_url=os.environ.get("AWS_ENDPOINT_URL"),
            aws_access_key_id=os.environ["AWS_ACCESS_KEY_ID"],
            aws_secret_access_key=os.environ["AWS_SECRET_ACCESS_KEY"],
        )
        bucket = os.environ["AWS_BUCKET_NAME"]
        resp = s3.get_object(Bucket=bucket, Key=s3_key)
        file_bytes = resp["Body"].read()

    # Only use S3 from Modal when NOT using inline content (i.e., Modal can reach S3)
    def _ensure_s3():
        nonlocal s3, bucket
        if not s3:
            s3 = boto3.client(
                "s3",
                endpoint_url=os.environ.get("AWS_ENDPOINT_URL"),
                aws_access_key_id=os.environ["AWS_ACCESS_KEY_ID"],
                aws_secret_access_key=os.environ["AWS_SECRET_ACCESS_KEY"],
            )
            bucket = os.environ["AWS_BUCKET_NAME"]
        return s3, bucket

    # When content is inline, S3 is managed by the Go server — skip S3 ops in Modal
    s3c = None
    bkt = None
    if not content_b64:
        s3c, bkt = _ensure_s3()

    gemini_key = os.environ.get("GEMINI_API_KEY", "")
    chunks: list[Chunk] = []

    if file_type in ("pdf", "docx", "pptx"):
        chunks = chunk_document_with_stage_functions(file_bytes, root_id, file_path, file_type, s3c, bkt)
    elif file_type == "image":
        chunks = chunk_image(file_bytes, root_id, file_path, s3c, bkt, gemini_key)
    elif file_type in CODE_EXTENSIONS:
        text = file_bytes.decode("utf-8", errors="replace")
        chunks = chunk_code(text, root_id, file_path, file_type)
    else:
        text = file_bytes.decode("utf-8", errors="replace")
        chunks = chunk_markdown(text, root_id, file_path, file_type)

    return [asdict(c) for c in chunks]


# ---------------------------------------------------------------------------
# chunks_to_embeddings: embed a batch of chunks
# ---------------------------------------------------------------------------

EMBEDDING_MODEL = "nomic-ai/nomic-embed-text-v1.5"
EMBEDDING_DIM = 768


@app.cls(
    image=embedding_image,
    gpu=os.getenv("PUFFERFS_MODAL_EMBED_GPU", "L4"),
    cpu=4,
    timeout=600,
    memory=8192,
    min_containers=int(os.getenv("PUFFERFS_MODAL_EMBED_MIN_CONTAINERS", "1")),
    max_containers=int(os.getenv("PUFFERFS_MODAL_EMBED_MAX_CONTAINERS", "8")),
    scaledown_window=900,
)
class Embedder:
    """Persistent embedding model container (GPU-backed, Nomic Embed v1.5)."""

    @modal.enter()
    def load_model(self):
        import torch
        from sentence_transformers import SentenceTransformer

        self.device = "cuda" if torch.cuda.is_available() else "cpu"
        self.encode_batch_size = int(os.getenv("PUFFERFS_MODAL_EMBED_ENCODE_BATCH_SIZE", "64"))
        self.model = SentenceTransformer(
            EMBEDDING_MODEL,
            trust_remote_code=True,
            device=self.device,
        )

    def _embed_chunks(self, chunk_dicts: list[dict]) -> list[dict]:
        """Embed a batch of Chunk dicts, return ChunkWithEmbedding dicts."""
        if not chunk_dicts:
            return []

        texts = [f"search_document: {c['content']}" for c in chunk_dicts]
        embeddings = self.model.encode(
            texts,
            normalize_embeddings=True,
            show_progress_bar=False,
            batch_size=self.encode_batch_size,
            device=self.device,
        )

        results: list[dict] = []
        for chunk_dict, emb in zip(chunk_dicts, embeddings):
            chunk = Chunk(**chunk_dict)
            cwe = ChunkWithEmbedding(chunk=chunk, embedding=emb.tolist())
            results.append(asdict(cwe))

        return results

    def _embed_texts(self, texts: list[str]) -> list[list[float]]:
        """Embed a list of raw text strings. Used for query embedding."""
        if not texts:
            return []
        prefixed = [f"search_query: {t}" for t in texts]
        embeddings = self.model.encode(
            prefixed,
            normalize_embeddings=True,
            show_progress_bar=False,
            batch_size=self.encode_batch_size,
            device=self.device,
        )
        return [emb.tolist() for emb in embeddings]

    @modal.fastapi_endpoint(method="POST", label="pufferfs-embed-chunks-endpoint")
    def embed_chunks_endpoint(self, item: dict) -> dict:
        """HTTP endpoint: POST {chunks: [...]} -> {results: [...]}"""
        results = self._embed_chunks(item["chunks"])
        return {"results": results, "count": len(results)}

    @modal.fastapi_endpoint(method="POST", label="pufferfs-embed-query-endpoint")
    def embed_query_endpoint(self, item: dict) -> dict:
        """HTTP endpoint: POST {texts: [...]} -> {embeddings: [...]}"""
        embeddings = self._embed_texts(item["texts"])
        return {"embeddings": embeddings}


# ---------------------------------------------------------------------------
# Convenience web endpoints for the Go server to call
# ---------------------------------------------------------------------------


@app.function(
    image=chunking_image,
    secrets=[s3_secret, gemini_secret],
    timeout=600,
    memory=2048,
)
@modal.fastapi_endpoint(method="POST")
def chunk_file_endpoint(item: dict) -> dict:
    """HTTP endpoint: POST {s3_key, file_path, file_type, root_id, content_b64?} -> {chunks: [...]}"""
    chunks = file_to_chunks.local(
        s3_key=item.get("s3_key", ""),
        file_path=item["file_path"],
        file_type=item.get("file_type", "auto"),
        root_id=item["root_id"],
        content_b64=item.get("content_b64"),
    )
    return {"chunks": chunks, "count": len(chunks)}
