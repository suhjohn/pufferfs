"""Shared data models for PufferFs Modal functions."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field


@dataclass
class ChunkInput:
    """Input to file_to_chunks: a file to be chunked."""

    s3_key: str
    file_path: str  # relative path within root
    file_type: str  # e.g. "python", "pdf", "docx", "markdown", "image"
    root_id: str


@dataclass
class Chunk:
    """A single chunk of text extracted from a file."""

    id: str  # sha256(root_id:file_path)[:16]:{chunk_index}
    root_id: str
    file_path: str
    chunk_index: int
    content: str
    content_hash: str  # sha256 of content
    file_type: str
    page_number: int | None = None
    image_path: str | None = None  # S3 key for rendered page image

    @staticmethod
    def make_id(root_id: str, file_path: str, chunk_index: int) -> str:
        path_hash = hashlib.sha256(f"{root_id}:{file_path}".encode()).hexdigest()[:16]
        return f"{path_hash}:{chunk_index}"

    @staticmethod
    def hash_content(content: str) -> str:
        return hashlib.sha256(content.encode()).hexdigest()


@dataclass
class ChunkWithEmbedding:
    """A chunk with its embedding vector."""

    chunk: Chunk
    embedding: list[float] = field(default_factory=list)
