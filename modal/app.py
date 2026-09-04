"""PufferFs Modal app — file chunking and embedding functions."""

from __future__ import annotations

import os
import base64
import binascii
import gzip
import hashlib
import hmac
import json
import tempfile
import time
from dataclasses import asdict

import modal

from models import Chunk, ChunkWithEmbedding

app = modal.App(os.getenv("PUFFERFS_MODAL_APP_NAME", "pufferfs"))
EMBEDDING_MODEL = "nomic-ai/nomic-embed-text-v1.5"
EMBEDDING_MODEL_REVISION = "e9b6763023c676ca8431644204f50c2b100d9aab"
EMBEDDING_CODE_REVISION = "7710840340a098cfb869c4f65e87cf2b1b70caca"
EMBEDDING_DIM = 768
PDF_RENDERER_INSTALL_URL = os.getenv(
    "PUFFERFS_PDF_RENDERER_INSTALL_URL",
    "https://raw.githubusercontent.com/suhjohn/frpdf-renderer/main/install.sh",
)
PDF_RENDERER_VERSION = os.getenv("PUFFERFS_PDF_RENDERER_VERSION", "v0.1.1")

# ---------------------------------------------------------------------------
# Container images
# ---------------------------------------------------------------------------

chunking_image = (
    modal.Image.from_registry("python:3.12-slim-trixie")
    .apt_install(
        "ca-certificates",
        "curl",
        "ffmpeg",
        "libreoffice-core",
        "libreoffice-writer",
        "libreoffice-impress",
        "mupdf-tools",
    )
    .run_commands(
        f"curl -fsSL {PDF_RENDERER_INSTALL_URL} "
        f"| FRPDF_VERSION={PDF_RENDERER_VERSION} INSTALL_DIR=/usr/local/bin sh "
        "&& (/usr/local/bin/frpdf 2>&1 || true) | grep -q 'Usage: frpdf'"
    )
    .pip_install(
        "boto3>=1.34.0",
        "extract-msg>=0.50.0",
        "pymupdf>=1.24.0",
        "Pillow>=10.0.0",
        "google-genai>=1.0.0",
        "openai>=1.0.0",
        "fastapi[standard]",
    )
    .add_local_file("models.py", "/root/models.py", copy=True)
    .add_local_file("chunkers.py", "/root/chunkers.py", copy=True)
)


def _cache_embedding_model() -> None:
    from sentence_transformers import SentenceTransformer

    SentenceTransformer(
        EMBEDDING_MODEL,
        revision=EMBEDDING_MODEL_REVISION,
        trust_remote_code=True,
        model_kwargs={"code_revision": EMBEDDING_CODE_REVISION},
    )


embedding_image = (
    modal.Image.debian_slim(python_version="3.12")
    .pip_install(
        "boto3>=1.34.0",
        "sentence-transformers>=3.0.0",
        "torch>=2.0.0",
        "einops>=0.7.0",
        "fastapi[standard]",
    )
    .pip_install("turbopuffer>=2.9.0,<3")
    .add_local_file("models.py", "/root/models.py", copy=True)
    .run_function(_cache_embedding_model)
)

# ---------------------------------------------------------------------------
# Secrets
# ---------------------------------------------------------------------------

modal_secret = modal.Secret.from_name(
    os.getenv("PUFFERFS_MODAL_SECRET_NAME", "pufferfs"),
)
endpoint_auth_secret = modal.Secret.from_name(
    os.getenv("PUFFERFS_MODAL_ENDPOINT_SECRET_NAME", "pufferfs-endpoint-auth"),
)


def _require_modal_secret(item: dict) -> None:
    from fastapi import HTTPException

    expected = os.environ.get("PUFFERFS_MODAL_ENDPOINT_AUTH_KEY", "")
    if not expected:
        raise HTTPException(status_code=503, detail="Modal endpoint authentication is not configured")
    provided = str(item.get("secret_key") or item.get("modal_secret_key") or "")
    if not hmac.compare_digest(provided, expected):
        raise HTTPException(status_code=401, detail="invalid secret_key")


def _decode_b64_field(item: dict, field: str) -> bytes:
    from fastapi import HTTPException

    value = item.get(field)
    if not isinstance(value, str) or not value:
        raise HTTPException(status_code=400, detail=f"{field} is required")
    try:
        return base64.b64decode(value, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise HTTPException(status_code=400, detail=f"{field} must be valid base64") from exc


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
    secrets=[modal_secret, endpoint_auth_secret],
    timeout=600,
    memory=2048,
)
@modal.fastapi_endpoint(method="POST", label="pufferfs-office-to-pdf-endpoint")
def office_to_pdf_endpoint(item: dict) -> dict:
    """HTTP endpoint: POST {secret_key, content_b64, file_type, file_path} -> {pdf_b64, bytes}."""
    _require_modal_secret(item)
    file_bytes = _decode_b64_field(item, "content_b64")
    file_type = str(item.get("file_type") or "")
    file_path = str(item.get("file_path") or "document")
    if file_type not in ("docx", "pptx"):
        from fastapi import HTTPException

        raise HTTPException(status_code=400, detail="file_type must be docx or pptx")
    pdf_bytes = office_to_pdf.local(file_bytes, file_type, file_path)
    return {
        "file_path": file_path,
        "file_type": file_type,
        "pdf_b64": base64.b64encode(pdf_bytes).decode("ascii"),
        "bytes": len(pdf_bytes),
    }


@app.function(
    image=chunking_image,
    timeout=600,
    memory=2048,
)
def pdf_to_page_images(pdf_bytes: bytes, file_path: str) -> list[dict]:
    """Render PDF bytes to per-page JPEG images plus fallback native text."""
    import time

    from chunkers import _page_image_dpi, _page_image_jpeg_quality, extract_pdf_page_texts, render_pdf_pages_jpeg

    render_start = time.perf_counter()
    rendered_pages = render_pdf_pages_jpeg(pdf_bytes)
    print(
        f"timing file={file_path} stage=pdf_render renderer=frpdf-renderer pages={len(rendered_pages)} "
        f"dpi={_page_image_dpi()} jpeg_quality={_page_image_jpeg_quality()} "
        f"elapsed={time.perf_counter() - render_start:.3f}s",
        flush=True,
    )

    text_start = time.perf_counter()
    page_texts = extract_pdf_page_texts(pdf_bytes)
    print(
        f"timing file={file_path} stage=pdf_text_extract engine=pymupdf pages={len(page_texts)} "
        f"elapsed={time.perf_counter() - text_start:.3f}s",
        flush=True,
    )

    pages: list[dict] = []
    for page_num, image_bytes in rendered_pages:
        pages.append(
            {
                "page_num": page_num,
                "image_bytes": image_bytes,
                "fallback_text": page_texts[page_num] if page_num < len(page_texts) else "",
            }
        )
    return pages


@app.function(
    image=chunking_image,
    secrets=[modal_secret, endpoint_auth_secret],
    timeout=600,
    memory=2048,
)
@modal.fastapi_endpoint(method="POST", label="pufferfs-pdf-to-page-images-endpoint")
def pdf_to_page_images_endpoint(item: dict) -> dict:
    """HTTP endpoint: POST {secret_key, pdf_b64, file_path} -> {pages:[...]}."""
    _require_modal_secret(item)
    pdf_bytes = _decode_b64_field(item, "pdf_b64")
    file_path = str(item.get("file_path") or "document.pdf")
    pages = pdf_to_page_images.local(pdf_bytes, file_path)
    out_pages: list[dict] = []
    for page in pages:
        image_bytes = page["image_bytes"]
        out_pages.append(
            {
                "page_num": page["page_num"],
                "image_b64": base64.b64encode(image_bytes).decode("ascii"),
                "image_bytes": len(image_bytes),
                "fallback_text": page.get("fallback_text", ""),
            }
        )
    return {
        "file_path": file_path,
        "page_count": len(out_pages),
        "pages": out_pages,
    }


@app.function(
    image=chunking_image,
    secrets=[modal_secret],
    timeout=600,
    memory=2048,
    min_containers=int(os.getenv("PUFFERFS_MODAL_PAGE_TEXT_MIN_CONTAINERS", "4")),
    max_containers=int(os.getenv("PUFFERFS_MODAL_OCR_MAX_CONTAINERS", "100")),
    scaledown_window=900,
)
def page_image_to_text(file_path: str, page_num: int, image_bytes: bytes, fallback_text: str) -> str:
    """Extract page text with native PDF text first, then VLLM OCR."""
    import time

    from chunkers import _available_image_providers, image_to_text

    text_start = time.perf_counter()
    gemini_key = os.environ.get("GEMINI_API_KEY", "")
    text = fallback_text
    source = "native" if text.strip() else "none"

    if not text.strip() and _available_image_providers(gemini_key):
        try:
            text = image_to_text(image_bytes, gemini_key, mime_type="image/jpeg")
            if text.strip():
                source = "vllm"
        except Exception as exc:
            print(
                f"timing file={file_path} page={page_num} stage=image_to_text_vllm_error error={type(exc).__name__}",
                flush=True,
            )
    print(
        f"timing file={file_path} page={page_num} stage=image_to_text "
        f"source={source} chars={len(text)} elapsed={time.perf_counter() - text_start:.3f}s",
        flush=True,
    )
    return text


def _env_int(name: str, default: int, minimum: int, maximum: int) -> int:
    try:
        value = int(os.getenv(name, str(default)))
    except ValueError:
        return default
    return max(minimum, min(maximum, value))


def _page_image_upload_concurrency() -> int:
    return _env_int("PUFFERFS_MODAL_PAGE_IMAGE_UPLOAD_CONCURRENCY", 32, 1, 128)


def _s3_max_pool_connections() -> int:
    return max(10, _page_image_upload_concurrency() + 8)


def _upload_page_images(s3_client, bucket: str, file_path: str, uploads: list[tuple[int, str, bytes]]) -> None:
    """Upload rendered page images with bounded parallelism."""
    if not s3_client or not bucket or not uploads:
        return

    import concurrent.futures
    import time

    upload_all_start = time.perf_counter()

    def upload_one(upload: tuple[int, str, bytes]) -> None:
        page_num, image_key, img_bytes = upload
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

    max_workers = min(_page_image_upload_concurrency(), len(uploads))
    with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers) as executor:
        futures = [executor.submit(upload_one, upload) for upload in uploads]
        for future in concurrent.futures.as_completed(futures):
            future.result()

    print(
        f"timing file={file_path} stage=parallel_page_image_upload pages={len(uploads)} "
        f"concurrency={max_workers} elapsed={time.perf_counter() - upload_all_start:.3f}s",
        flush=True,
    )


def chunk_document_with_stage_functions(
    file_bytes: bytes,
    root_id: str,
    file_path: str,
    file_type: str,
    s3_client,
    bucket: str,
) -> list[Chunk]:
    """Orchestrate split Modal functions for PDF/DOCX/PPTX chunking."""
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

    page_inputs: list[tuple[str, int, bytes, str]] = []
    page_texts_by_num: dict[int, str] = {}
    page_image_keys: dict[int, str] = {}
    page_uploads: list[tuple[int, str, bytes]] = []
    for page_data in pages:
        page_num = int(page_data["page_num"])
        img_bytes = page_data["image_bytes"]
        fallback_text = page_data.get("fallback_text") or ""
        image_key = _page_image_key(root_id, file_path, page_num)
        page_image_keys[page_num] = image_key
        page_uploads.append((page_num, image_key, img_bytes))
        if fallback_text.strip():
            page_texts_by_num[page_num] = fallback_text
        else:
            page_inputs.append((file_path, page_num, img_bytes, fallback_text))

    import concurrent.futures

    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as upload_executor:
        upload_future = upload_executor.submit(_upload_page_images, s3_client, bucket, file_path, page_uploads)

        text_start = time.perf_counter()
        page_texts = list(page_image_to_text.starmap(page_inputs, order_outputs=True)) if page_inputs else []
        print(
            f"timing file={file_path} stage=parallel_image_to_text pages={len(page_inputs)} elapsed={time.perf_counter() - text_start:.3f}s",
            flush=True,
        )
        for page_input, text in zip(page_inputs, page_texts):
            page_texts_by_num[page_input[1]] = text

        upload_future.result()

    chunks: list[Chunk] = []
    for page_data in pages:
        page_start = time.perf_counter()
        page_num = int(page_data["page_num"])
        text = page_texts_by_num.get(page_num, "")
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
            image_path=page_image_keys[page_num],
        )
        print(
            f"timing file={file_path} page={page_num} stage=page_total elapsed={time.perf_counter() - page_start:.3f}s",
            flush=True,
        )
        chunks.append(chunk)

    chunks.sort(key=lambda chunk: chunk.chunk_index)
    print(
        f"timing file={file_path} stage=document_chunk_total pages={len(pages)} chunks={len(chunks)} elapsed={time.perf_counter() - total_start:.3f}s",
        flush=True,
    )
    return chunks


@app.function(
    image=chunking_image,
    secrets=[modal_secret],
    timeout=3600,
    memory=2048,
)
def file_to_chunks(
    s3_key: str,
    file_path: str,
    file_type: str,
    root_id: str,
    absolute_path: str = "",
    content_b64: str | None = None,
) -> list[dict]:
    """Chunk a file. If content_b64 is provided, uses it directly; otherwise downloads from S3."""
    import base64
    from chunkers import (
        CODE_EXTENSIONS,
        chunk_code,
        chunk_image,
        chunk_markdown,
        chunk_media,
        chunk_structured_file,
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
        s3 = _s3_client()
        bucket = os.environ["AWS_BUCKET_NAME"]
        resp = s3.get_object(Bucket=bucket, Key=s3_key)
        try:
            file_bytes = resp["Body"].read()
        finally:
            resp["Body"].close()

    # Only use S3 from Modal when NOT using inline content (i.e., Modal can reach S3)
    def _ensure_s3():
        nonlocal s3, bucket
        if not s3:
            s3 = _s3_client()
            bucket = os.environ["AWS_BUCKET_NAME"]
        return s3, bucket

    # Source bytes can be inline, but generated images still need persisted S3 keys.
    s3c = None
    bkt = None
    if not content_b64 or file_type in ("pdf", "docx", "pptx", "image"):
        s3c, bkt = _ensure_s3()

    gemini_key = os.environ.get("GEMINI_API_KEY", "")
    chunks: list[Chunk] = []

    if file_type in ("pdf", "docx", "pptx"):
        chunks = chunk_document_with_stage_functions(file_bytes, root_id, file_path, file_type, s3c, bkt)
    elif file_type == "image":
        chunks = chunk_image(file_bytes, root_id, file_path, s3c, bkt, gemini_key)
    elif file_type in ("audio", "video"):
        chunks = chunk_media(file_bytes, root_id, file_path, file_type, gemini_key)
    elif file_type in ("eml", "msg", "vcf", "ics"):
        chunks = chunk_structured_file(file_bytes, root_id, file_path, file_type)
    elif file_type in CODE_EXTENSIONS:
        text = file_bytes.decode("utf-8", errors="replace")
        chunks = chunk_code(text, root_id, file_path, file_type)
    else:
        text = file_bytes.decode("utf-8", errors="replace")
        chunks = chunk_markdown(text, root_id, file_path, file_type)

    for chunk in chunks:
        chunk.absolute_path = absolute_path
    return [asdict(c) for c in chunks]


# ---------------------------------------------------------------------------
# Queue index stage: job pointer -> S3 chunk stream -> Turbopuffer
# ---------------------------------------------------------------------------


def _s3_client():
    import boto3
    from botocore.config import Config

    return boto3.client(
        "s3",
        endpoint_url=os.environ.get("AWS_ENDPOINT_URL"),
        aws_access_key_id=os.environ["AWS_ACCESS_KEY_ID"],
        aws_secret_access_key=os.environ["AWS_SECRET_ACCESS_KEY"],
        config=Config(max_pool_connections=_s3_max_pool_connections()),
    )


def _iter_jsonl(s3, key: str):
    from boto3.s3.transfer import TransferConfig

    bucket = os.environ["AWS_BUCKET_NAME"]
    started = time.perf_counter()
    with tempfile.TemporaryDirectory() as temp_dir:
        payload_path = os.path.join(temp_dir, "chunk-artifact")
        s3.download_file(
            bucket,
            key,
            payload_path,
            Config=TransferConfig(
                multipart_threshold=8 << 20,
                multipart_chunksize=8 << 20,
                max_concurrency=4,
                num_download_attempts=5,
            ),
        )
        payload_bytes = os.path.getsize(payload_path)
        download_elapsed = time.perf_counter() - started
        print(
            f"timing stage=index_shard_artifact_download key={key} "
            f"bytes={payload_bytes} elapsed={download_elapsed:.3f}s",
            flush=True,
        )
        opener = gzip.open if key.endswith(".gz") else open
        with opener(payload_path, "rb") as lines:
            for line in lines:
                if line.strip():
                    yield json.loads(line)


TP_SCHEMA = {
    "content": {"type": "string", "full_text_search": True},
    "file_path": {"type": "string"},
    "chunk_index": {"type": "uint"},
    "content_hash": {"type": "string"},
    "file_hash": {"type": "string"},
    "file_type": {"type": "string"},
    "page_number": {"type": "uint"},
    "image_path": {"type": "string"},
    "line_start": {"type": "uint"},
    "line_end": {"type": "uint"},
    "root_id": {"type": "string"},
    "generation_id": {"type": "string"},
    "valid_from_generation": {"type": "string"},
    "valid_from_generation_seq": {"type": "uint"},
    "valid_to_generation": {"type": "string"},
    "valid_to_generation_seq": {"type": "uint"},
}


def _index_namespace_map(job: dict) -> tuple[dict[int, str], int]:
    entries = job.get("index_namespaces") or []
    if not entries:
        raise ValueError("index_namespaces is required")
    shard_counts = {int(entry.get("shard_count") or 0) for entry in entries}
    if len(shard_counts) != 1:
        raise ValueError("index namespace shard counts do not match")
    shard_count = shard_counts.pop()
    if shard_count < 1 or shard_count > 256 or len(entries) != shard_count:
        raise ValueError("index namespace set is incomplete")
    by_shard: dict[int, str] = {}
    for entry in entries:
        shard_index = int(entry.get("shard_index", -1))
        namespace = str(entry.get("namespace") or "")
        if shard_index < 0 or shard_index >= shard_count or not namespace or shard_index in by_shard:
            raise ValueError("index namespace entry is invalid")
        by_shard[shard_index] = namespace
    return by_shard, shard_count


def _namespace_for_path(by_shard: dict[int, str], shard_count: int, file_path: str) -> str:
    if not file_path:
        raise ValueError("file_path is required for namespace routing")
    digest = hashlib.sha256(file_path.encode()).digest()
    shard_index = int.from_bytes(digest[:8], "big") % shard_count
    return by_shard[shard_index]


def _and_filter(filters: list) -> list | None:
    if not filters:
        return None
    if len(filters) == 1:
        return filters[0]
    return ["And", filters]


def _active_generation_filter(sequence: int) -> list:
    return [
        "And",
        [
            ["valid_from_generation_seq", "Lte", sequence],
            [
                "Or",
                [
                    ["valid_to_generation_seq", "Eq", 0],
                    ["valid_to_generation_seq", "Gt", sequence],
                ],
            ],
        ],
    ]


def _embedding_chunk(row: dict) -> dict:
    return {
        "id": row.get("id"),
        "content": row.get("content", ""),
        "file_path": row.get("file_path", ""),
        "chunk_index": row.get("chunk_index", 0),
        "content_hash": row.get("content_hash", ""),
        "file_type": row.get("file_type", ""),
        "root_id": row.get("root_id"),
        "absolute_path": row.get("absolute_path", ""),
        "page_number": row.get("page_number"),
        "image_path": row.get("image_path"),
        "line_start": row.get("line_start"),
        "line_end": row.get("line_end"),
    }


# ---------------------------------------------------------------------------
# Shared embedding model loader
# ---------------------------------------------------------------------------


def _load_embedding_model():
    import torch
    from sentence_transformers import SentenceTransformer

    device = "cuda" if torch.cuda.is_available() else "cpu"
    model = SentenceTransformer(
        EMBEDDING_MODEL,
        revision=EMBEDDING_MODEL_REVISION,
        trust_remote_code=True,
        device=device,
        model_kwargs={"code_revision": EMBEDDING_CODE_REVISION},
    )
    if device == "cuda":
        model.half()
    return model, device


def _encode_texts(model, device: str, texts: list[str], prefix: str, batch_size: int) -> list[list[float]]:
    if not texts:
        return []
    embeddings = model.encode(
        [f"{prefix}{text}" for text in texts],
        normalize_embeddings=True,
        show_progress_bar=False,
        batch_size=batch_size,
        device=device,
    )
    return [embedding.tolist() for embedding in embeddings]


# ---------------------------------------------------------------------------
# Bulk chunk embedding and index writes
# ---------------------------------------------------------------------------


@app.cls(
    image=embedding_image,
    secrets=[modal_secret, endpoint_auth_secret],
    gpu=os.getenv("PUFFERFS_MODAL_EMBED_GPU", "L4"),
    cpu=4,
    timeout=3600,
    memory=8192,
    min_containers=int(os.getenv("PUFFERFS_MODAL_EMBED_MIN_CONTAINERS", "0")),
    max_containers=int(os.getenv("PUFFERFS_MODAL_EMBED_MAX_CONTAINERS", "16")),
    scaledown_window=900,
)
class Embedder:
    """Persistent embedding model container (GPU-backed, Nomic Embed v1.5)."""

    @modal.enter()
    def load_model(self):
        self.model, self.device = _load_embedding_model()
        self.encode_batch_size = _env_int("PUFFERFS_MODAL_EMBED_ENCODE_BATCH_SIZE", 64, 1, 512)
        self.shard_embed_batch_size = _env_int("PUFFERFS_MODAL_SHARD_EMBED_BATCH_ROWS", 64, 1, 64)
        self.tp_write_batch_rows = _env_int("PUFFERFS_TP_WRITE_BATCH_ROWS", 512, 1, 512)
        self.tp_write_batch_bytes = _env_int("PUFFERFS_TP_WRITE_BATCH_BYTES", 8 << 20, 1 << 20, 8 << 20)
        self.tp = None

    def _turbopuffer(self):
        if self.tp is not None:
            return self.tp
        from turbopuffer import Turbopuffer

        api_key = os.environ.get("TURBOPUFFER_API_KEY", "")
        if not api_key:
            raise RuntimeError("TURBOPUFFER_API_KEY is not configured")
        options = {
            "api_key": api_key,
            "region": os.environ.get("TURBOPUFFER_REGION", "gcp-us-central1"),
            "timeout": 120,
            "max_retries": 4,
            "compression": True,
        }
        base_url = os.environ.get("TURBOPUFFER_API_URL")
        if base_url:
            options["base_url"] = base_url
            if "{region}" not in base_url:
                options.pop("region")
        self.tp = Turbopuffer(**options)
        return self.tp

    def _embed_chunks(self, chunk_dicts: list[dict]) -> list[dict]:
        """Embed a batch of Chunk dicts, return ChunkWithEmbedding dicts."""
        if not chunk_dicts:
            return []

        vectors = _encode_texts(
            self.model,
            self.device,
            [chunk["content"] for chunk in chunk_dicts],
            "search_document: ",
            self.encode_batch_size,
        )

        results: list[dict] = []
        for chunk_dict, vector in zip(chunk_dicts, vectors):
            chunk = Chunk(**chunk_dict)
            cwe = ChunkWithEmbedding(chunk=chunk, embedding=vector)
            results.append(asdict(cwe))

        return results

    @modal.fastapi_endpoint(method="POST", label="pufferfs-embed-chunks-endpoint")
    def embed_chunks_endpoint(self, item: dict) -> dict:
        """HTTP endpoint: POST {chunks: [...]} -> {results: [...]}"""
        results = self._embed_chunks(item["chunks"])
        return {"results": results}

    @modal.fastapi_endpoint(method="POST", label="pufferfs-index-shard-endpoint")
    def index_shard_endpoint(self, item: dict) -> dict:
        """Embed one chunk artifact and write bounded batches to Turbopuffer."""
        _require_modal_secret(item)
        job = item.get("job") or {}
        generation_id = str(job.get("generation_id") or "")
        generation_sequence = int(job.get("generation_seq") or 0)
        base_sequence = int(job.get("base_generation_seq") or 0)
        payload_ref = str(job.get("payload_ref") or "")
        if not generation_id or not payload_ref.startswith(f"syncs/{generation_id}/chunks/"):
            raise ValueError("payload_ref must reference this generation's chunk artifact")
        if not payload_ref.endswith((".jsonl", ".jsonl.gz")):
            raise ValueError("payload_ref must be a JSONL chunk artifact")
        if bool(job.get("disable_vector")):
            raise ValueError("vector-disabled shards must use the Go index path")
        if generation_sequence < 1 or base_sequence < 0 or base_sequence >= generation_sequence:
            raise ValueError("generation sequence metadata is invalid")

        by_shard, shard_count = _index_namespace_map(job)
        from turbopuffer import NotFoundError

        s3 = _s3_client()
        tp = self._turbopuffer()
        pending: list[dict] = []
        write_buffer: list[tuple[str, dict]] = []
        write_bytes = 0
        close_paths: dict[str, set[str]] = {}
        chunks_added = 0
        chunks_removed = 0
        embedding_seconds = 0.0
        tp_write_seconds = 0.0
        request_row_budget = self.tp_write_batch_bytes - (64 << 10)
        started = time.perf_counter()

        def flush_writes() -> None:
            nonlocal write_bytes, chunks_added, tp_write_seconds
            if not write_buffer:
                return
            rows_by_namespace: dict[str, list[dict]] = {}
            for namespace, row in write_buffer:
                rows_by_namespace.setdefault(namespace, []).append(row)
            for namespace, rows in rows_by_namespace.items():
                write_started = time.perf_counter()
                try:
                    tp.namespace(namespace).write(
                        upsert_rows=rows,
                        schema=TP_SCHEMA,
                        distance_metric="cosine_distance",
                    )
                finally:
                    tp_write_seconds += time.perf_counter() - write_started
                chunks_added += len(rows)
            write_buffer.clear()
            write_bytes = 0

        def append_write(row: dict) -> None:
            nonlocal write_bytes
            file_path = str(row.get("file_path") or "")
            namespace = _namespace_for_path(by_shard, shard_count, file_path)
            row_bytes = len(json.dumps(row, separators=(",", ":"), ensure_ascii=False, allow_nan=False).encode()) + 1
            if row_bytes > request_row_budget:
                raise ValueError(f"index row for {file_path} is too large for one write request")
            if write_buffer and (
                len(write_buffer) >= self.tp_write_batch_rows
                or write_bytes + row_bytes > request_row_budget
            ):
                flush_writes()
            write_buffer.append((namespace, row))
            write_bytes += row_bytes
            if len(write_buffer) >= self.tp_write_batch_rows or write_bytes >= request_row_budget:
                flush_writes()

        def flush_pending() -> None:
            nonlocal embedding_seconds
            if not pending:
                return
            unique: dict[str, dict] = {}
            for row in pending:
                chunk = _embedding_chunk(row)
                key = (
                    f"hash:{chunk['content_hash']}"
                    if chunk.get("content_hash")
                    else f"content:{chunk['content']}"
                )
                unique.setdefault(key, chunk)
            unique_keys = list(unique)
            embed_started = time.perf_counter()
            try:
                embedded = self._embed_chunks([unique[key] for key in unique_keys])
            finally:
                embedding_seconds += time.perf_counter() - embed_started
            if len(embedded) != len(unique_keys):
                raise RuntimeError(f"embedded {len(embedded)} chunks for {len(unique_keys)} unique pending rows")
            vectors = {key: result.get("embedding") for key, result in zip(unique_keys, embedded)}
            for row in pending:
                key = (
                    f"hash:{row['content_hash']}"
                    if row.get("content_hash")
                    else f"content:{row.get('content', '')}"
                )
                embedding = vectors.get(key)
                if embedding is None:
                    raise RuntimeError("embedding result missing embedding vector")
                row["vector"] = embedding
                append_write(row)
            pending.clear()

        def flush_closes() -> None:
            nonlocal chunks_removed, tp_write_seconds
            if not close_paths:
                return
            patch = {
                "valid_to_generation": generation_id,
                "valid_to_generation_seq": generation_sequence,
            }
            for namespace, paths in close_paths.items():
                filters = [
                    ["file_path", "In", sorted(paths)],
                    [
                        "Or",
                        [
                            ["valid_to_generation_seq", "Eq", 0],
                            ["valid_to_generation_seq", "Lte", patch["valid_to_generation_seq"]],
                        ],
                    ],
                ]
                if base_sequence > 0:
                    filters.append(_active_generation_filter(base_sequence))
                for attempt in range(100):
                    write_started = time.perf_counter()
                    try:
                        response = tp.namespace(namespace).write(
                            patch_by_filter={"filters": _and_filter(filters), "patch": patch},
                            patch_by_filter_allow_partial=True,
                        )
                    except NotFoundError:
                        break
                    finally:
                        tp_write_seconds += time.perf_counter() - write_started
                    chunks_removed += int(response.rows_patched or response.rows_affected or 0)
                    if not response.rows_remaining:
                        break
                    if attempt == 99:
                        raise RuntimeError(f"closing rows in {namespace}: rows remain after repeated patch passes")
            close_paths.clear()

        for artifact in _iter_jsonl(s3, payload_ref):
            op = artifact.get("op")
            if op == "close" and artifact.get("close_path"):
                flush_pending()
                flush_writes()
                path = str(artifact["close_path"])
                namespace = _namespace_for_path(by_shard, shard_count, path)
                close_paths.setdefault(namespace, set()).add(path)
                continue
            if op != "upsert" or not artifact.get("row"):
                continue
            flush_closes()
            row = artifact["row"]
            if row.get("vector") is not None:
                append_write(row)
            else:
                pending.append(row)
                if len(pending) >= self.shard_embed_batch_size:
                    flush_pending()

        flush_pending()
        flush_closes()
        flush_writes()
        print(
            f"timing stage=index_shard job_id={job.get('job_id', '')} "
            f"chunks_added={chunks_added} chunks_removed={chunks_removed} "
            f"embedding={embedding_seconds:.3f}s tp_writes={tp_write_seconds:.3f}s "
            f"elapsed={time.perf_counter() - started:.3f}s",
            flush=True,
        )
        return {
            "status": "indexed",
            "chunks_added": chunks_added,
            "chunks_removed": chunks_removed,
        }


# ---------------------------------------------------------------------------
# Latency-isolated query embedding
# ---------------------------------------------------------------------------


@app.cls(
    image=embedding_image,
    secrets=[modal_secret, endpoint_auth_secret],
    gpu=os.getenv("PUFFERFS_MODAL_QUERY_EMBED_GPU", "L4"),
    cpu=2,
    timeout=300,
    memory=4096,
    min_containers=int(os.getenv("PUFFERFS_MODAL_QUERY_EMBED_MIN_CONTAINERS", "1")),
    max_containers=int(os.getenv("PUFFERFS_MODAL_QUERY_EMBED_MAX_CONTAINERS", "2")),
    scaledown_window=900,
)
class QueryEmbedder:
    """Dedicated Nomic pool so bulk indexing cannot starve interactive queries."""

    @modal.enter()
    def load_model(self):
        self.model, self.device = _load_embedding_model()

    @modal.fastapi_endpoint(method="POST", label="pufferfs-embed-query-endpoint")
    def embed_query_endpoint(self, item: dict) -> dict:
        """HTTP endpoint: POST {texts: [...]} -> {embeddings: [...]}"""
        embeddings = _encode_texts(
            self.model,
            self.device,
            item["texts"],
            "search_query: ",
            64,
        )
        return {"embeddings": embeddings}


# ---------------------------------------------------------------------------
# Convenience web endpoints for the Go server to call
# ---------------------------------------------------------------------------


@app.function(
    image=chunking_image,
    secrets=[modal_secret],
    timeout=3600,
    memory=2048,
)
@modal.fastapi_endpoint(method="POST")
def chunk_file_endpoint(item: dict) -> dict:
    """HTTP endpoint: POST {s3_key, file_path, file_type, root_id, content_b64?} -> {chunks: [...]}"""
    chunks = file_to_chunks.local(
        s3_key=item.get("s3_key", ""),
        file_path=item["file_path"],
        absolute_path=item.get("absolute_path", ""),
        file_type=item.get("file_type", "auto"),
        root_id=item["root_id"],
        content_b64=item.get("content_b64"),
    )
    return {"chunks": chunks}
