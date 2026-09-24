"""Ordinary format selection and bounded chunk construction, without IO effects."""

from __future__ import annotations

import hashlib
from collections.abc import Iterable, Iterator
from pathlib import PurePath


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
    """Stream bounded text chunks; segment workers can persist the same parser."""
    from text_stream import TextStream

    stream = TextStream(target_bytes=target_bytes, byte_start=byte_start, line_start=line_start)
    for block in blocks:
        for offset in range(0, len(block), 65536):
            yield from stream.feed(block[offset:offset + 65536])
    yield from stream.feed(None)


def page_chunks(markdown: str, page_number: int, *, target_bytes: int = CHUNK_BYTES) -> Iterator[dict]:
    if page_number < 0:
        raise ValueError("negative rendered page number")
    # Page-local parts retain one rendered page anchor, even when a table or
    # dense page needs more than one searchable chunk.
    for part, chunk in enumerate(text_chunks([markdown.encode("utf-8")], target_bytes=target_bytes)):
        chunk["location"] = {"page_number": page_number, "page_part": part}
        yield chunk
