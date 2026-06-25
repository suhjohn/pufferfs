"""File type-specific chunking strategies."""

from __future__ import annotations

import base64
import email
from email import policy
from html.parser import HTMLParser
import os
import quopri
import random
import re
import subprocess
import tempfile
import time

from models import Chunk

# ---------------------------------------------------------------------------
# Vision model routing: image/media → text
# ---------------------------------------------------------------------------

DEFAULT_VLLM_MODELS = [("gemini", "gemini-2.5-flash-lite"), ("gemini", "gemini-3.1-flash-lite")]
GEMINI_FAILURE_FALLBACK = ("openai", "gpt-5.4-nano")


class GeminiRecitationError(RuntimeError):
    """Gemini declined a response because the output looked like recitation."""


def _vllm_models() -> list[str]:
    return [f"{provider}/{model}" for provider, model, _weight in _vllm_model_weights()]


def _vllm_model_weights() -> list[tuple[str, str, float]]:
    raw = os.getenv("PUFFERFS_VLLM_MODELS", "")
    weights: list[tuple[str, str, float]] = []
    for item in raw.split(","):
        spec = item.strip()
        if not spec:
            continue
        provider_model, sep, weight_text = spec.rpartition(":")
        if not sep:
            provider_model = spec
            weight = 1.0
        else:
            provider_model = provider_model.strip()
            try:
                weight = float(weight_text.strip())
            except ValueError:
                continue
        provider, slash, model = provider_model.partition("/")
        provider = provider.strip().lower()
        model = model.strip()
        if not slash or not provider or not model or weight <= 0:
            continue
        weights.append((provider, model, weight))
    if weights:
        return weights
    return [(provider, model, 1.0) for provider, model in DEFAULT_VLLM_MODELS]


def _choose_vllm_model(available_providers: set[str] | None = None) -> tuple[str, str]:
    weights = _vllm_model_weights()
    if available_providers is not None:
        weights = [(provider, model, weight) for provider, model, weight in weights if provider in available_providers]
    if not weights:
        configured = ", ".join(_vllm_models())
        available = ", ".join(sorted(available_providers or [])) or "none"
        raise RuntimeError(f"no configured VLLM models are available; configured={configured}; available_providers={available}")
    return random.choices(
        [(provider, model) for provider, model, _weight in weights],
        weights=[weight for _provider, _model, weight in weights],
        k=1,
    )[0]


def _available_image_providers(gemini_api_key: str) -> set[str]:
    providers: set[str] = set()
    if gemini_api_key:
        providers.add("gemini")
    for provider, _model, _weight in _vllm_model_weights():
        if provider != "gemini" and _openai_compatible_api_key(provider):
            providers.add(provider)
    return providers


def _provider_env_prefix(provider: str) -> str:
    return re.sub(r"[^A-Za-z0-9]+", "_", provider).upper()


def _openai_compatible_api_key(provider: str) -> str:
    return os.getenv(f"{_provider_env_prefix(provider)}_API_KEY", "")


def _openai_compatible_base_url(provider: str) -> str | None:
    configured = os.getenv(f"{_provider_env_prefix(provider)}_BASE_URL", "").strip()
    if configured:
        return configured
    defaults = {
        "openai": "https://api.openai.com/v1",
        "fireworks": "https://api.fireworks.ai/inference/v1",
    }
    return defaults.get(provider)


def _image_prompt() -> str:
    return (
        "Extract all text from this image. If it contains a document page, "
        "return the full text content preserving structure. If it is a photo "
        "or diagram, describe what you see in detail. Return only the extracted "
        "text or description, no preamble."
    )


def image_to_text(image_bytes: bytes, gemini_api_key: str, mime_type: str = "image/jpeg") -> str:
    """Call a configured vision model to extract text / describe an image."""
    provider, model = _choose_vllm_model(_available_image_providers(gemini_api_key))
    if provider == "gemini":
        try:
            return _gemini_image_to_text(image_bytes, gemini_api_key, model, mime_type)
        except Exception:
            fallback_provider, fallback_model = GEMINI_FAILURE_FALLBACK
            return _openai_compatible_image_to_text(fallback_provider, image_bytes, fallback_model, mime_type)
    if _openai_compatible_base_url(provider):
        return _openai_compatible_image_to_text(provider, image_bytes, model, mime_type)
    raise RuntimeError(f"unsupported image-to-text provider {provider!r}")


def _env_int(name: str, default: int, minimum: int, maximum: int) -> int:
    try:
        value = int(os.getenv(name, str(default)))
    except ValueError:
        return default
    return max(minimum, min(maximum, value))


def _page_image_dpi() -> int:
    return _env_int("PUFFERFS_MODAL_PAGE_IMAGE_DPI", 160, 72, 300)


def _page_image_jpeg_quality() -> int:
    return _env_int("PUFFERFS_MODAL_PAGE_IMAGE_JPEG_QUALITY", 75, 30, 95)


def render_page_jpeg(page) -> bytes:
    """Render one PDF page to a compact JPEG for OCR and page previews."""
    pix = page.get_pixmap(dpi=_page_image_dpi(), alpha=False)
    return pix.tobytes("jpeg", jpg_quality=_page_image_jpeg_quality())


def _gemini_image_to_text(image_bytes: bytes, gemini_api_key: str, model: str, mime_type: str) -> str:
    from google import genai
    from google.genai.types import Part

    client = genai.Client(api_key=gemini_api_key)
    response = client.models.generate_content(
        model=model,
        contents=[
            Part.from_bytes(data=image_bytes, mime_type=mime_type),
            _image_prompt(),
        ],
    )
    _raise_if_gemini_recitation(response)
    return response.text or ""


def _raise_if_gemini_recitation(response) -> None:
    for candidate in getattr(response, "candidates", []) or []:
        reason = getattr(candidate, "finish_reason", None)
        reason_name = getattr(reason, "name", reason)
        if str(reason_name).upper() == "RECITATION":
            raise GeminiRecitationError("Gemini response blocked for recitation")


def _openai_compatible_image_to_text(provider: str, image_bytes: bytes, model: str, mime_type: str) -> str:
    from openai import OpenAI

    data = base64.b64encode(image_bytes).decode("ascii")
    api_key = _openai_compatible_api_key(provider)
    if not api_key:
        raise RuntimeError(f"missing {_provider_env_prefix(provider)}_API_KEY for provider {provider!r}")
    base_url = _openai_compatible_base_url(provider)
    if not base_url:
        raise RuntimeError(f"missing {_provider_env_prefix(provider)}_BASE_URL for provider {provider!r}")
    client = OpenAI(api_key=api_key, base_url=base_url)
    response = client.chat.completions.create(
        model=model,
        messages=[
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": _image_prompt()},
                    {"type": "image_url", "image_url": {"url": f"data:{mime_type};base64,{data}"}},
                ],
            }
        ],
    )
    if not response.choices:
        return ""
    content = response.choices[0].message.content
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(str(part.get("text", "")) for part in content if isinstance(part, dict))
    return ""


def media_segment_to_text(
    segment_path: str,
    gemini_api_key: str,
    media_type: str,
    start_seconds: float,
    end_seconds: float,
    mime_type: str,
) -> str:
    """Call a configured vision/media model to describe an audio/video segment for retrieval."""
    from google import genai

    provider, model = _choose_vllm_model({"gemini"} if gemini_api_key else set())
    if provider != "gemini":
        raise RuntimeError(f"unsupported media-to-text provider {provider!r}")

    client = genai.Client(api_key=gemini_api_key)
    uploaded = client.files.upload(file=segment_path, config={"mime_type": mime_type})
    uploaded = _wait_for_file(uploaded, client)

    time_range = f"{_format_timestamp(start_seconds)}-{_format_timestamp(end_seconds)}"
    prompt = (
        f"This is a segment from a larger {media_type} file, covering {time_range}.\n\n"
        "Describe the important content in this segment for semantic search indexing.\n"
        "Include enough detail that someone searching later can find this file and decide whether to open it.\n"
        "Do not transcribe verbatim. Ignore filler and unimportant chatter.\n"
        "Return only the text to index."
    )
    if media_type == "video":
        prompt += "\nInclude visible content if it helps explain what the segment is about."

    response = client.models.generate_content(model=model, contents=[uploaded, prompt])
    return response.text or ""


def _wait_for_file(uploaded, client):
    deadline = time.time() + 300
    while True:
        state = None
        try:
            if uploaded.state is not None:
                state = uploaded.state.name
        except AttributeError:
            state = None
        if state in (None, "ACTIVE"):
            return uploaded
        if state == "FAILED":
            raise RuntimeError(f"Gemini file processing failed for {uploaded.name}")
        if time.time() >= deadline:
            raise TimeoutError(f"timed out waiting for Gemini file processing for {uploaded.name}")
        time.sleep(5)
        uploaded = client.files.get(name=uploaded.name)


# ---------------------------------------------------------------------------
# Code chunking (line-based)
# ---------------------------------------------------------------------------

CODE_EXTENSIONS = {
    "python", "javascript", "typescript", "go", "rust", "java", "c", "cpp",
    "csharp", "ruby", "php", "swift", "kotlin", "scala", "shell", "bash",
    "lua", "perl", "r", "sql", "html", "css", "scss", "yaml", "toml", "json",
    "xml", "proto", "graphql", "hcl", "terraform", "dockerfile", "makefile",
}

CHUNK_LINES = 300
OVERLAP_LINES = 50


def chunk_code(
    text: str,
    root_id: str,
    file_path: str,
    file_type: str,
) -> list[Chunk]:
    """Split code into line-based chunks with overlap."""
    lines = text.splitlines(keepends=True)
    if not lines:
        return []

    chunks: list[Chunk] = []
    start = 0
    idx = 0

    while start < len(lines):
        end = min(start + CHUNK_LINES, len(lines))
        content = "".join(lines[start:end])
        if content.strip():
            chunk = Chunk(
                id=Chunk.make_id(root_id, file_path, idx),
                root_id=root_id,
                file_path=file_path,
                chunk_index=idx,
                content=content,
                content_hash=Chunk.hash_content(content),
                file_type=file_type,
                line_start=start + 1,
                line_end=end,
            )
            chunks.append(chunk)
            idx += 1
        start = end - OVERLAP_LINES if end < len(lines) else end

    return chunks


# ---------------------------------------------------------------------------
# Markdown / plaintext chunking (section-based)
# ---------------------------------------------------------------------------

MAX_SECTION_CHARS = 2000
SECTION_OVERLAP_CHARS = 200

_HEADING_RE = re.compile(r"^#{1,6}\s", re.MULTILINE)


def chunk_markdown(
    text: str,
    root_id: str,
    file_path: str,
    file_type: str = "markdown",
) -> list[Chunk]:
    """Split markdown/text by headings, then by size."""
    sections = _split_by_headings(text)
    chunks: list[Chunk] = []
    idx = 0

    for section in sections:
        for piece in _split_large(section, MAX_SECTION_CHARS, SECTION_OVERLAP_CHARS):
            if piece.strip():
                chunk = Chunk(
                    id=Chunk.make_id(root_id, file_path, idx),
                    root_id=root_id,
                    file_path=file_path,
                    chunk_index=idx,
                    content=piece,
                    content_hash=Chunk.hash_content(piece),
                    file_type=file_type,
                )
                chunks.append(chunk)
                idx += 1

    return chunks


def _split_by_headings(text: str) -> list[str]:
    positions = [m.start() for m in _HEADING_RE.finditer(text)]
    if not positions:
        return [text] if text.strip() else []
    sections: list[str] = []
    if positions[0] > 0:
        sections.append(text[: positions[0]])
    for i, pos in enumerate(positions):
        end = positions[i + 1] if i + 1 < len(positions) else len(text)
        sections.append(text[pos:end])
    return sections


def _split_large(text: str, max_chars: int, overlap: int) -> list[str]:
    if len(text) <= max_chars:
        return [text]
    pieces: list[str] = []
    start = 0
    while start < len(text):
        end = min(start + max_chars, len(text))
        pieces.append(text[start:end])
        start = end - overlap if end < len(text) else end
    return pieces


# ---------------------------------------------------------------------------
# Document chunking: PDF / DOCX / PPTX → PDF pages → images → text
# ---------------------------------------------------------------------------


def _convert_to_pdf(file_bytes: bytes, file_type: str) -> bytes:
    """Convert DOCX or PPTX to PDF via LibreOffice headless."""
    import os
    import subprocess
    import tempfile

    ext = {"docx": ".docx", "pptx": ".pptx"}[file_type]
    with tempfile.TemporaryDirectory() as tmpdir:
        in_path = os.path.join(tmpdir, f"input{ext}")
        with open(in_path, "wb") as f:
            f.write(file_bytes)

        subprocess.run(
            ["libreoffice", "--headless", "--convert-to", "pdf", "--outdir", tmpdir, in_path],
            check=True,
            capture_output=True,
            timeout=120,
        )

        pdf_path = os.path.join(tmpdir, "input.pdf")
        with open(pdf_path, "rb") as f:
            return f.read()


def chunk_document_via_images(
    file_bytes: bytes,
    root_id: str,
    file_path: str,
    file_type: str,
    s3_client,
    bucket: str,
    gemini_api_key: str,
) -> list[Chunk]:
    """Unified pipeline: PDF/DOCX/PPTX → PDF pages → JPEG images → Gemini → text chunks."""
    import fitz  # pymupdf
    import time

    total_start = time.perf_counter()

    # Convert DOCX/PPTX to PDF first
    if file_type in ("docx", "pptx"):
        convert_start = time.perf_counter()
        pdf_bytes = _convert_to_pdf(file_bytes, file_type)
        print(
            f"timing file={file_path} stage=office_to_pdf file_type={file_type} "
            f"bytes_in={len(file_bytes)} bytes_out={len(pdf_bytes)} elapsed={time.perf_counter() - convert_start:.3f}s",
            flush=True,
        )
    else:
        pdf_bytes = file_bytes

    render_start = time.perf_counter()
    doc = fitz.open(stream=pdf_bytes, filetype="pdf")
    pages: list[tuple[int, bytes, str]] = []
    for page_num in range(len(doc)):
        page = doc[page_num]
        img_bytes = render_page_jpeg(page)
        fallback_text = page.get_text("text")
        pages.append((page_num, img_bytes, fallback_text))
    doc.close()
    print(
        f"timing file={file_path} stage=pdf_render pages={len(pages)} "
        f"dpi={_page_image_dpi()} jpeg_quality={_page_image_jpeg_quality()} "
        f"elapsed={time.perf_counter() - render_start:.3f}s",
        flush=True,
    )

    def page_to_chunk(page_data: tuple[int, bytes, str]) -> Chunk:
        page_start = time.perf_counter()
        page_num, img_bytes, fallback_text = page_data
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

        text_start = time.perf_counter()
        text = fallback_text
        source = "native" if text.strip() else "none"
        if not text.strip() and _available_image_providers(gemini_api_key):
            try:
                text = image_to_text(img_bytes, gemini_api_key, mime_type="image/jpeg")
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

    if not pages:
        print(
            f"timing file={file_path} stage=document_chunk_total pages=0 chunks=0 elapsed={time.perf_counter() - total_start:.3f}s",
            flush=True,
        )
        return []

    text_start = time.perf_counter()
    chunks = [page_to_chunk(page) for page in pages]
    print(
        f"timing file={file_path} stage=image_to_text_pages pages={len(pages)} elapsed={time.perf_counter() - text_start:.3f}s",
        flush=True,
    )
    chunks.sort(key=lambda chunk: chunk.chunk_index)
    print(
        f"timing file={file_path} stage=document_chunk_total pages={len(pages)} chunks={len(chunks)} elapsed={time.perf_counter() - total_start:.3f}s",
        flush=True,
    )
    return chunks

# ---------------------------------------------------------------------------
# Standalone image chunking: image → VLLM → text
# ---------------------------------------------------------------------------


def chunk_image(
    file_bytes: bytes,
    root_id: str,
    file_path: str,
    s3_client,
    bucket: str,
    gemini_api_key: str,
) -> list[Chunk]:
    """For images: optionally upload to S3, call a vision model for text extraction/captioning."""
    image_key = f"chunks/{root_id}/{file_path}"
    content_type = _guess_image_content_type(file_path)

    # Upload original image to S3 (skipped when s3_client is None)
    if s3_client and bucket:
        s3_client.put_object(
            Bucket=bucket,
            Key=image_key,
            Body=file_bytes,
            ContentType=content_type,
        )

    # Call the configured vision model for text extraction / captioning.
    content = ""
    if _available_image_providers(gemini_api_key):
        try:
            content = image_to_text(file_bytes, gemini_api_key, mime_type=content_type)
        except Exception as exc:
            print(
                f"timing file={file_path} stage=image_to_text_vllm_error error={type(exc).__name__}",
                flush=True,
            )

    if not content.strip():
        content = f"[Image: {file_path}]"

    chunk = Chunk(
        id=Chunk.make_id(root_id, file_path, 0),
        root_id=root_id,
        file_path=file_path,
        chunk_index=0,
        content=content,
        content_hash=Chunk.hash_content(content),
        file_type="image",
        image_path=image_key,
    )
    return [chunk]


# ---------------------------------------------------------------------------
# Email / calendar / contact chunking
# ---------------------------------------------------------------------------


class _TextExtractor(HTMLParser):
    def __init__(self):
        super().__init__()
        self.parts: list[str] = []

    def handle_data(self, data: str) -> None:
        if data.strip():
            self.parts.append(data.strip())

    def text(self) -> str:
        return "\n".join(self.parts)


def chunk_structured_file(
    file_bytes: bytes,
    root_id: str,
    file_path: str,
    file_type: str,
) -> list[Chunk]:
    if file_type == "eml":
        headers, body = _extract_eml_parts(file_bytes)
        return _chunk_email_parts(headers, body, root_id, file_path, file_type)
    elif file_type == "msg":
        headers, body = _extract_msg_parts(file_bytes)
        return _chunk_email_parts(headers, body, root_id, file_path, file_type)
    elif file_type == "vcf":
        return _chunk_record_texts(_vcf_record_texts(file_bytes), root_id, file_path, file_type)
    elif file_type == "ics":
        return _chunk_record_texts(_ics_record_texts(file_bytes), root_id, file_path, file_type)
    else:
        text = file_bytes.decode("utf-8", errors="replace")
        return chunk_markdown(text, root_id, file_path, file_type)


def _extract_eml_text(file_bytes: bytes) -> str:
    headers, body = _extract_eml_parts(file_bytes)
    return "\n".join(part for part in ("\n".join(headers), body) if part).strip()


def _extract_eml_parts(file_bytes: bytes) -> tuple[list[str], str]:
    msg = email.message_from_bytes(file_bytes, policy=policy.default)
    return _email_header_lines(msg), _email_body_text(msg)


def _email_header_lines(msg) -> list[str]:
    lines: list[str] = []
    for name in ("Subject", "From", "To", "Cc", "Date"):
        value = msg.get(name)
        if value:
            lines.append(f"{name}: {value}")
    return lines


def _email_body_text(msg) -> str:
    plain_parts: list[str] = []
    html_parts: list[str] = []
    for part in msg.walk() if msg.is_multipart() else [msg]:
        if part.is_multipart():
            continue
        disposition = part.get_content_disposition()
        if disposition == "attachment":
            continue
        content_type = part.get_content_type()
        try:
            content = part.get_content()
        except LookupError:
            payload = part.get_payload(decode=True) or b""
            content = payload.decode(part.get_content_charset() or "utf-8", errors="replace")
        if content_type == "text/plain":
            plain_parts.append(str(content).strip())
        elif content_type == "text/html":
            html_parts.append(_html_to_text(str(content)).strip())
    return "\n\n".join(part for part in plain_parts if part) or "\n\n".join(part for part in html_parts if part)


def _html_to_text(html: str) -> str:
    extractor = _TextExtractor()
    extractor.feed(html)
    return extractor.text()


def _extract_msg_text(file_bytes: bytes) -> str:
    headers, body = _extract_msg_parts(file_bytes)
    return "\n".join(part for part in ("\n".join(headers), body) if part).strip()


def _extract_msg_parts(file_bytes: bytes) -> tuple[list[str], str]:
    import extract_msg

    with tempfile.TemporaryDirectory() as tmpdir:
        path = os.path.join(tmpdir, "message.msg")
        with open(path, "wb") as f:
            f.write(file_bytes)
        msg = extract_msg.Message(path)
        headers: list[str] = []
        for name, value in (
            ("Subject", msg.subject),
            ("From", msg.sender),
            ("To", msg.to),
            ("Cc", msg.cc),
            ("Date", msg.date),
        ):
            if value:
                headers.append(f"{name}: {value}")
        body = msg.body or msg.htmlBody or ""
        if isinstance(body, bytes):
            body = body.decode("utf-8", errors="replace")
        if msg.htmlBody and not msg.body:
            body = _html_to_text(str(body))
        msg.close()
    return headers, str(body).strip()


def _extract_vcf_text(file_bytes: bytes) -> str:
    return "\n\n".join(_vcf_record_texts(file_bytes)).strip()


def _vcf_record_texts(file_bytes: bytes) -> list[str]:
    cards = _split_records(_decode_text_file(file_bytes), "BEGIN:VCARD", "END:VCARD")
    if not cards:
        cards = [_decode_text_file(file_bytes)]
    out: list[str] = []
    for idx, card in enumerate(cards, start=1):
        fields = _parse_structured_lines(card)
        lines = [f"Contact {idx}"]
        for key in ("FN", "N", "ORG", "TITLE", "EMAIL", "TEL", "ADR", "URL", "NOTE", "CATEGORIES"):
            for value in fields.get(key, []):
                lines.append(f"{_friendly_vcf_name(key)}: {value}")
        out.append("\n".join(lines))
    return out


def _extract_ics_text(file_bytes: bytes) -> str:
    return "\n\n".join(_ics_record_texts(file_bytes)).strip()


def _ics_record_texts(file_bytes: bytes) -> list[str]:
    text = _decode_text_file(file_bytes)
    events = _split_records(text, "BEGIN:VEVENT", "END:VEVENT")
    if not events:
        events = _split_records(text, "BEGIN:VTODO", "END:VTODO")
    if not events:
        events = [text]
    out: list[str] = []
    for idx, event in enumerate(events, start=1):
        fields = _parse_structured_lines(event)
        lines = [f"Calendar item {idx}"]
        for key in ("SUMMARY", "DTSTART", "DTEND", "DUE", "LOCATION", "DESCRIPTION", "ORGANIZER", "ATTENDEE", "STATUS", "URL"):
            for value in fields.get(key, []):
                lines.append(f"{_friendly_ics_name(key)}: {value}")
        out.append("\n".join(lines))
    return out


def _chunk_email_parts(
    headers: list[str],
    body: str,
    root_id: str,
    file_path: str,
    file_type: str,
) -> list[Chunk]:
    prelude = "\n".join(headers).strip()
    body = body.strip()
    if not body:
        return _chunks_from_texts([prelude], root_id, file_path, file_type)

    max_body_chars = max(MAX_SECTION_CHARS - len(prelude) - 2, MAX_SECTION_CHARS // 2)
    pieces = _split_large(body, max_body_chars, min(SECTION_OVERLAP_CHARS, max_body_chars // 4))
    texts = [f"{prelude}\n\n{piece}".strip() if prelude else piece for piece in pieces]
    return _chunks_from_texts(texts, root_id, file_path, file_type)


def _chunk_record_texts(
    records: list[str],
    root_id: str,
    file_path: str,
    file_type: str,
) -> list[Chunk]:
    texts: list[str] = []
    current: list[str] = []
    current_len = 0

    def flush_current() -> None:
        nonlocal current, current_len
        if current:
            texts.append("\n\n".join(current))
            current = []
            current_len = 0

    for record in records:
        record = record.strip()
        if not record:
            continue
        if len(record) > MAX_SECTION_CHARS:
            flush_current()
            texts.extend(_split_large(record, MAX_SECTION_CHARS, SECTION_OVERLAP_CHARS))
            continue
        projected = current_len + len(record) + (2 if current else 0)
        if current and projected > MAX_SECTION_CHARS:
            flush_current()
        current.append(record)
        current_len += len(record) + (2 if current_len else 0)
    flush_current()
    return _chunks_from_texts(texts, root_id, file_path, file_type)


def _chunks_from_texts(texts: list[str], root_id: str, file_path: str, file_type: str) -> list[Chunk]:
    chunks: list[Chunk] = []
    for idx, text in enumerate(text for text in texts if text.strip()):
        content = text.strip()
        chunks.append(
            Chunk(
                id=Chunk.make_id(root_id, file_path, idx),
                root_id=root_id,
                file_path=file_path,
                chunk_index=idx,
                content=content,
                content_hash=Chunk.hash_content(content),
                file_type=file_type,
            )
        )
    return chunks


def _decode_text_file(file_bytes: bytes) -> str:
    return file_bytes.decode("utf-8-sig", errors="replace")


def _split_records(text: str, begin: str, end: str) -> list[str]:
    records: list[str] = []
    current: list[str] = []
    in_record = False
    for line in _unfold_ical_lines(text):
        upper = line.upper()
        if upper == begin:
            current = [line]
            in_record = True
        elif upper == end and in_record:
            current.append(line)
            records.append("\n".join(current))
            current = []
            in_record = False
        elif in_record:
            current.append(line)
    return records


def _unfold_ical_lines(text: str) -> list[str]:
    lines: list[str] = []
    for raw in text.replace("\r\n", "\n").replace("\r", "\n").split("\n"):
        if raw.startswith((" ", "\t")) and lines:
            lines[-1] += raw[1:]
        else:
            lines.append(raw)
    return lines


def _parse_structured_lines(text: str) -> dict[str, list[str]]:
    fields: dict[str, list[str]] = {}
    for line in _unfold_ical_lines(text):
        if ":" not in line:
            continue
        raw_key, raw_value = line.split(":", 1)
        key = raw_key.split(";", 1)[0].upper()
        value = _clean_structured_value(raw_value)
        if value:
            fields.setdefault(key, []).append(value)
    return fields


def _clean_structured_value(value: str) -> str:
    value = quopri.decodestring(value).decode("utf-8", errors="replace")
    value = value.replace("\\n", "\n").replace("\\,", ",").replace("\\;", ";")
    value = value.replace(";", " ")
    value = re.sub(r"\s+", " ", value)
    return value.strip()


def _friendly_vcf_name(key: str) -> str:
    return {
        "FN": "Name",
        "N": "Name",
        "ORG": "Organization",
        "TITLE": "Title",
        "EMAIL": "Email",
        "TEL": "Phone",
        "ADR": "Address",
        "URL": "URL",
        "NOTE": "Note",
        "CATEGORIES": "Categories",
    }.get(key, key.title())


def _friendly_ics_name(key: str) -> str:
    return {
        "SUMMARY": "Title",
        "DTSTART": "Start",
        "DTEND": "End",
        "DUE": "Due",
        "LOCATION": "Location",
        "DESCRIPTION": "Description",
        "ORGANIZER": "Organizer",
        "ATTENDEE": "Attendee",
        "STATUS": "Status",
        "URL": "URL",
    }.get(key, key.title())


# ---------------------------------------------------------------------------
# Audio / video chunking
# ---------------------------------------------------------------------------


MEDIA_WINDOW_SECONDS = 6 * 60
MEDIA_OVERLAP_SECONDS = 60


def chunk_media(
    file_bytes: bytes,
    root_id: str,
    file_path: str,
    file_type: str,
    gemini_api_key: str,
) -> list[Chunk]:
    # Media uses Gemini file upload processing today; image/page OCR can use
    # either Gemini or OpenAI through PUFFERFS_VLLM_MODELS.
    if not gemini_api_key:
        return [_placeholder_media_chunk(root_id, file_path, file_type)]

    with tempfile.TemporaryDirectory() as tmpdir:
        input_path = os.path.join(tmpdir, f"input{_media_input_extension(file_path, file_type)}")
        with open(input_path, "wb") as f:
            f.write(file_bytes)
        duration = _ffprobe_duration(input_path)
        chunks: list[Chunk] = []
        for idx, start, end in _media_time_ranges(duration):
            segment_path = _ffmpeg_extract_segment(input_path, tmpdir, idx, start, end, file_path, file_type)
            mime_type = _media_mime_type(segment_path, file_type)
            text = media_segment_to_text(segment_path, gemini_api_key, file_type, start, end, mime_type).strip()
            if not text:
                continue
            time_range = f"{_format_timestamp(start)}-{_format_timestamp(end)}"
            content = f"[{time_range}] {text}"
            chunks.append(
                Chunk(
                    id=Chunk.make_id(root_id, file_path, idx),
                    root_id=root_id,
                    file_path=file_path,
                    chunk_index=idx,
                    content=content,
                    content_hash=Chunk.hash_content(content),
                    file_type=file_type,
                )
            )
    if chunks:
        return chunks
    return [_placeholder_media_chunk(root_id, file_path, file_type)]


def _placeholder_media_chunk(root_id: str, file_path: str, file_type: str) -> Chunk:
    content = f"[{file_type.capitalize()} file: {file_path}]"
    return Chunk(
        id=Chunk.make_id(root_id, file_path, 0),
        root_id=root_id,
        file_path=file_path,
        chunk_index=0,
        content=content,
        content_hash=Chunk.hash_content(content),
        file_type=file_type,
    )


def _media_input_extension(file_path: str, file_type: str) -> str:
    _, ext = os.path.splitext(file_path.lower())
    if ext in (".mp3", ".wav", ".mp4", ".mov"):
        return ext
    return ".mp4" if file_type == "video" else ".mp3"


def _media_time_ranges(duration: float) -> list[tuple[int, float, float]]:
    if duration <= 0:
        return [(0, 0.0, float(MEDIA_WINDOW_SECONDS))]
    ranges: list[tuple[int, float, float]] = []
    start = 0.0
    idx = 0
    stride = MEDIA_WINDOW_SECONDS - MEDIA_OVERLAP_SECONDS
    while start < duration:
        end = min(start + MEDIA_WINDOW_SECONDS, duration)
        ranges.append((idx, start, end))
        if end >= duration:
            break
        start += stride
        idx += 1
    return ranges


def _ffprobe_duration(input_path: str) -> float:
    result = subprocess.run(
        [
            "ffprobe",
            "-v",
            "error",
            "-show_entries",
            "format=duration",
            "-of",
            "default=noprint_wrappers=1:nokey=1",
            input_path,
        ],
        check=True,
        capture_output=True,
        text=True,
        timeout=60,
    )
    try:
        return max(float(result.stdout.strip()), 0.0)
    except ValueError:
        return 0.0


def _ffmpeg_extract_segment(
    input_path: str,
    tmpdir: str,
    idx: int,
    start: float,
    end: float,
    file_path: str,
    file_type: str,
) -> str:
    ext = _media_input_extension(file_path, file_type)
    output_path = os.path.join(tmpdir, f"segment-{idx:04d}{ext}")
    duration = max(end - start, 0.1)
    copy_cmd = [
        "ffmpeg",
        "-hide_banner",
        "-loglevel",
        "error",
        "-y",
        "-ss",
        str(start),
        "-t",
        str(duration),
        "-i",
        input_path,
        "-map",
        "0",
        "-c",
        "copy",
        output_path,
    ]
    if _run_ffmpeg(copy_cmd, output_path):
        return output_path

    if file_type == "video":
        output_path = os.path.join(tmpdir, f"segment-{idx:04d}.mp4")
        fallback_cmd = [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-y",
            "-ss",
            str(start),
            "-t",
            str(duration),
            "-i",
            input_path,
            "-c:v",
            "libx264",
            "-c:a",
            "aac",
            "-movflags",
            "+faststart",
            output_path,
        ]
    else:
        output_path = os.path.join(tmpdir, f"segment-{idx:04d}.wav")
        fallback_cmd = [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-y",
            "-ss",
            str(start),
            "-t",
            str(duration),
            "-i",
            input_path,
            "-vn",
            "-c:a",
            "pcm_s16le",
            output_path,
        ]
    subprocess.run(fallback_cmd, check=True, capture_output=True, timeout=180)
    return output_path


def _run_ffmpeg(cmd: list[str], output_path: str) -> bool:
    result = subprocess.run(cmd, capture_output=True, timeout=180)
    return result.returncode == 0 and os.path.exists(output_path) and os.path.getsize(output_path) > 0


def _media_mime_type(path: str, file_type: str) -> str:
    _, ext = os.path.splitext(path.lower())
    if file_type == "audio":
        return "audio/wav" if ext == ".wav" else "audio/mp3"
    if ext == ".mov":
        return "video/quicktime"
    return "video/mp4"


def _format_timestamp(seconds: float) -> str:
    total = max(int(round(seconds)), 0)
    hours, rem = divmod(total, 3600)
    minutes, secs = divmod(rem, 60)
    if hours:
        return f"{hours:02d}:{minutes:02d}:{secs:02d}"
    return f"{minutes:02d}:{secs:02d}"


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

FILE_TYPE_MAP: dict[str, str] = {
    ".py": "python", ".js": "javascript", ".ts": "typescript",
    ".jsx": "javascript", ".tsx": "typescript",
    ".go": "go", ".rs": "rust", ".java": "java",
    ".c": "c", ".h": "c", ".cpp": "cpp", ".hpp": "cpp",
    ".cs": "csharp", ".rb": "ruby", ".php": "php",
    ".swift": "swift", ".kt": "kotlin", ".scala": "scala",
    ".sh": "shell", ".bash": "bash",
    ".lua": "lua", ".pl": "perl", ".r": "r",
    ".sql": "sql", ".html": "html", ".css": "css", ".scss": "scss",
    ".yaml": "yaml", ".yml": "yaml", ".toml": "toml",
    ".json": "json", ".xml": "xml",
    ".proto": "proto", ".graphql": "graphql",
    ".tf": "terraform", ".hcl": "hcl",
    ".md": "markdown", ".rst": "markdown", ".txt": "text",
    ".pdf": "pdf",
    ".docx": "docx", ".doc": "docx",
    ".pptx": "pptx", ".ppt": "pptx",
    ".png": "image", ".jpg": "image", ".jpeg": "image",
    ".gif": "image", ".svg": "image", ".webp": "image",
    ".bmp": "image",
    ".eml": "eml", ".msg": "msg", ".vcf": "vcf", ".ics": "ics",
    ".mp3": "audio", ".wav": "audio",
    ".mp4": "video", ".mov": "video",
}


def detect_file_type(file_path: str) -> str:
    import os
    _, ext = os.path.splitext(file_path.lower())
    base = os.path.basename(file_path.lower())
    if base in ("dockerfile", "makefile", "rakefile", "gemfile"):
        return "shell"
    return FILE_TYPE_MAP.get(ext, "text")


def _page_image_key(root_id: str, file_path: str, page_num: int) -> str:
    return f"chunks/{root_id}/{file_path}.{page_num}.jpg"


def _guess_image_content_type(file_path: str) -> str:
    import mimetypes

    mime, _ = mimetypes.guess_type(file_path)
    return mime or "application/octet-stream"
