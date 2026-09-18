"""Ordinary format selection and bounded chunk construction, without IO effects."""

from __future__ import annotations

import hashlib
from collections import deque
from collections.abc import Iterable, Iterator
from pathlib import PurePath

from base64_redaction import redacted_spans

CHUNK_BYTES = 6000

FORMATS = {
    "pdf": {".pdf"},
    "document": {".doc", ".docx", ".docm", ".dot", ".dotx", ".dotm", ".rtf", ".odt", ".ott", ".fodt"},
    "presentation": {".ppt", ".pptx", ".pptm", ".pps", ".ppsx", ".ppsm", ".pot", ".potx", ".potm", ".odp", ".otp", ".fodp"},
    "spreadsheet": {".xls", ".xlsx", ".xlsm", ".xlsb", ".xlt", ".xltx", ".xltm", ".ods", ".ots", ".fods", ".csv", ".tsv"},
    "image": {".png", ".jpg", ".jpeg", ".jfif", ".webp", ".gif", ".bmp", ".tiff", ".tif", ".heic", ".heif", ".avif", ".svg", ".apng", ".jp2", ".jpx", ".j2k"},
    "audio": {".mp3", ".wav", ".m4a", ".m4b", ".aac", ".flac", ".ogg", ".oga", ".opus", ".aif", ".aiff", ".wma", ".amr"},
    "video": {".mp4", ".mov", ".m4v", ".mkv", ".webm", ".avi", ".mpeg", ".mpg", ".wmv", ".flv", ".3gp", ".mts", ".m2ts", ".mxf"},
    "jsonl": {".jsonl", ".ndjson"},
    "structured": {".eml", ".msg", ".vcf", ".ics"},
    "text": {".txt", ".md", ".rst", ".log", ".ini", ".cfg", ".conf", ".py", ".js", ".jsx", ".ts", ".tsx", ".go", ".rs", ".java", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".rb", ".php", ".swift", ".kt", ".scala", ".sh", ".bash", ".lua", ".pl", ".r", ".sql", ".html", ".css", ".scss", ".yaml", ".yml", ".toml", ".json", ".xml", ".proto", ".graphql", ".tf", ".hcl"},
}
FORMAT_BY_SUFFIX = {suffix: family for family, suffixes in FORMATS.items() for suffix in suffixes}


def file_family(path: str) -> str:
    name = PurePath(path).name.lower()
    if name in {"dockerfile", "makefile", "rakefile", "gemfile"}:
        return "text"
    return FORMAT_BY_SUFFIX.get(PurePath(name).suffix, "unknown")


def indexed_file_type(path: str) -> str:
    """Public search/read labels, independent of extraction dispatch.

    Preserve existing language and Office labels; expanded formats share their
    family's label. This metadata never selects a parser or provider.
    """
    name = PurePath(path).name.lower()
    suffix = PurePath(name).suffix.removeprefix(".")
    if name in {"dockerfile", "makefile", "rakefile", "gemfile"}:
        return "shell"
    family = file_family(path)
    if family == "structured":
        return suffix
    if family == "text":
        return {
            "py": "python", "js": "javascript", "jsx": "javascript",
            "ts": "typescript", "tsx": "typescript", "rs": "rust",
            "h": "c", "cc": "cpp", "hpp": "cpp", "cs": "csharp",
            "rb": "ruby", "kt": "kotlin", "sh": "shell", "pl": "perl",
            "yml": "yaml", "tf": "terraform", "md": "markdown", "rst": "markdown",
            "txt": "text", "log": "text", "ini": "text", "cfg": "text", "conf": "text",
        }.get(suffix, suffix)
    return {"document": "docx", "presentation": "pptx", "unknown": "text"}.get(family, family)


def chunk_record(content: str, location: dict, ordinal: int) -> dict:
    return {
        "chunk_index": ordinal,
        "content": content,
        "content_hash": hashlib.sha256(content.encode("utf-8")).hexdigest(),
        "location": location,
    }


def utf8_boundary(data: bytes | bytearray, end: int) -> int:
    # Bytes at end belong to the next chunk. Keep continuation bytes together
    # even when an input read or a long JSONL record spans multiple chunks.
    while 0 < end < len(data) and data[end] & 0xC0 == 0x80:
        end -= 1
    return end


def text_chunks(blocks: Iterable[bytes], *, target_bytes: int = CHUNK_BYTES, byte_start: int = 0, line_start: int = 1) -> Iterator[dict]:
    """Redact explicit base64 data URLs before chunking, retaining source bounds.

    Newlines are unchanged. A chunk intersecting a replacement marker covers
    the original payload's byte range, even if the marker spans two chunks.
    """
    replacements = deque()
    shift = 0

    def redacted():
        source, output = byte_start, byte_start
        for data, consumed, changed in redacted_spans(blocks):
            if changed:
                replacements.append((output, output + len(data), source, source + consumed))
            source += consumed
            output += len(data)
            yield data

    def source_boundary(position, *, end):
        delta = shift
        for left, right, source_left, source_right in replacements:
            if position <= left:
                return position + delta
            if position < right:
                return source_right if end else source_left
            delta = source_right - right
        return position + delta

    for chunk in _text_chunks(redacted(), target_bytes=target_bytes, byte_start=byte_start, line_start=line_start):
        location = chunk["location"]
        left, right = location["byte_start"], location["byte_end"]
        location["byte_start"] = source_boundary(left, end=False)
        location["byte_end"] = source_boundary(right, end=True)
        if any(a < right and b > left for a, b, _, _ in replacements):
            location["redacted"] = True
        while replacements and replacements[0][1] <= right:
            _, output_end, _, source_end = replacements.popleft()
            shift = source_end - output_end
        yield chunk


def _text_chunks(blocks: Iterable[bytes], *, target_bytes: int, byte_start: int, line_start: int) -> Iterator[dict]:
    """Group whole lines/JSONL records where possible; split oversized records.

    No JSON parsing/re-serialization or session detection. The caller supplies
    text with any explicit base64 data-URL payloads already redacted.
    Invalid UTF-8 or binary data is an explicit extraction error.
    """
    if target_bytes < 4 or byte_start < 0 or line_start < 1:
        raise ValueError("invalid text chunk boundaries")
    pending = bytearray()
    ordinal = 0

    def emit(end: int) -> dict:
        nonlocal byte_start, line_start, ordinal
        piece = bytes(pending[:end])
        content = piece.decode("utf-8", errors="strict")
        if "\x00" in content:
            raise ValueError("binary input is not a text file")
        line_end = line_start + piece.count(b"\n") - int(piece.endswith(b"\n"))
        chunk = chunk_record(content, {
            "byte_start": byte_start, "byte_end": byte_start + end,
            "line_start": line_start, "line_end": max(line_start, line_end),
        }, ordinal)
        line_start += piece.count(b"\n")
        byte_start += end
        ordinal += 1
        del pending[:end]
        return chunk

    for block in blocks:
        # Do not duplicate an arbitrarily large caller-supplied block in the
        # pending buffer. Normal source IO yields 64 KiB blocks.
        for offset in range(0, len(block), target_bytes):
            pending.extend(block[offset:offset + target_bytes])
            while len(pending) > target_bytes:
                end = pending.rfind(b"\n", 0, target_bytes) + 1
                if not end:
                    end = utf8_boundary(pending, target_bytes)
                yield emit(end)
    if pending:
        yield emit(len(pending))


def page_chunks(markdown: str, page_number: int, *, target_bytes: int = CHUNK_BYTES) -> Iterator[dict]:
    if page_number < 0:
        raise ValueError("negative rendered page number")
    # Page-local parts retain one rendered page anchor, even when a table or
    # dense page needs more than one searchable chunk.
    for part, chunk in enumerate(text_chunks([markdown.encode("utf-8")], target_bytes=target_bytes)):
        chunk["location"] = {"page_number": page_number, "page_part": part}
        yield chunk
