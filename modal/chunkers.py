"""File type-specific chunking strategies."""

from __future__ import annotations

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
# Gemini vision: image → text
# ---------------------------------------------------------------------------

GEMINI_MODELS = ["gemini-2.5-flash-lite", "gemini-2.5-flash"]


def image_to_text(image_bytes: bytes, gemini_api_key: str, mime_type: str = "image/jpeg") -> str:
    """Call Gemini vision to extract text / describe an image."""
    from google import genai
    from google.genai.types import Part

    client = genai.Client(api_key=gemini_api_key)
    model = random.choice(GEMINI_MODELS)

    response = client.models.generate_content(
        model=model,
        contents=[
            Part.from_bytes(data=image_bytes, mime_type=mime_type),
            "Extract all text from this image. If it contains a document page, "
            "return the full text content preserving structure. If it is a photo "
            "or diagram, describe what you see in detail. Return only the extracted "
            "text or description, no preamble.",
        ],
    )
    return response.text or ""


def media_segment_to_text(
    segment_path: str,
    gemini_api_key: str,
    media_type: str,
    start_seconds: float,
    end_seconds: float,
    mime_type: str,
) -> str:
    """Call Gemini to describe an audio/video segment for retrieval."""
    from google import genai

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

    model = random.choice(GEMINI_MODELS)
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

MIN_USABLE_EXTRACTED_TEXT_CHARS = 40


def extracted_text_is_usable(text: str, text_coverage: float | None = None) -> bool:
    """Return true when native/OCR-layer PDF text is good enough to skip Gemini."""
    stripped = text.strip()
    if not stripped:
        return False

    alnum_chars = sum(1 for ch in stripped if ch.isalnum())
    if alnum_chars < MIN_USABLE_EXTRACTED_TEXT_CHARS:
        return False

    replacement_chars = stripped.count("\ufffd")
    printable_chars = sum(1 for ch in stripped if ch.isprintable() or ch.isspace())
    if printable_chars / max(len(stripped), 1) < 0.9:
        return False
    if replacement_chars / max(len(stripped), 1) > 0.02:
        return False

    if _looks_garbled(stripped):
        return False

    if text_coverage is not None and text_coverage < 0.001 and alnum_chars < 200:
        return False

    return True


def _looks_garbled(text: str) -> bool:
    letters = 0
    vowels = 0
    for ch in text:
        if ch.isascii() and ch.isalpha():
            letters += 1
            if ch.lower() in "aeiou":
                vowels += 1
    if letters < 30:
        return False
    return vowels * 5 < letters


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
    import concurrent.futures
    import fitz  # pymupdf
    import os
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
    pages: list[tuple[int, bytes, str, bool]] = []
    for page_num in range(len(doc)):
        page = doc[page_num]
        pix = page.get_pixmap(dpi=200)
        img_bytes = pix.tobytes("jpeg")
        extracted_text = page.get_text("text")
        text_coverage = _page_text_coverage(page)
        pages.append(
            (
                page_num,
                img_bytes,
                extracted_text,
                extracted_text_is_usable(extracted_text, text_coverage),
            )
        )
    doc.close()
    print(
        f"timing file={file_path} stage=pdf_render pages={len(pages)} elapsed={time.perf_counter() - render_start:.3f}s",
        flush=True,
    )

    def page_to_chunk(page_data: tuple[int, bytes, str, bool]) -> Chunk:
        page_start = time.perf_counter()
        page_num, img_bytes, extracted_text, use_extracted_text = page_data
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
        if use_extracted_text:
            text = extracted_text
            source = "native_text"
        elif gemini_api_key:
            text = image_to_text(img_bytes, gemini_api_key, mime_type="image/jpeg")
            source = "gemini"
        else:
            text = extracted_text
            source = "native_fallback"
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


def _page_text_coverage(page) -> float:
    page_area = max(float(page.rect.width * page.rect.height), 1.0)
    text_area = 0.0
    for block in page.get_text("blocks"):
        if len(block) >= 7 and block[6] != 0:
            continue
        text = block[4] if len(block) >= 5 else ""
        if not str(text).strip():
            continue
        x0, y0, x1, y1 = block[:4]
        text_area += max(float(x1 - x0), 0.0) * max(float(y1 - y0), 0.0)
    return min(text_area / page_area, 1.0)


# ---------------------------------------------------------------------------
# Standalone image chunking: image → Gemini → text
# ---------------------------------------------------------------------------


def chunk_image(
    file_bytes: bytes,
    root_id: str,
    file_path: str,
    s3_client,
    bucket: str,
    gemini_api_key: str,
) -> list[Chunk]:
    """For images: optionally upload to S3, call Gemini for text extraction/captioning."""
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

    # Call Gemini for text extraction / captioning
    if gemini_api_key:
        content = image_to_text(file_bytes, gemini_api_key, mime_type=content_type)
    else:
        content = f"[Image: {file_path}]"

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
