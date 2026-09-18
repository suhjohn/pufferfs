"""Immutable source and artifact IO shared by the worker deployments.

No application-server round trips or source base64. Dependencies are supplied
by the role entrypoint, so the byte-flow contract can be tested without cloud
credentials or a Modal runtime.
"""

from __future__ import annotations

import gzip
import hashlib
import json
import os
import re
import tempfile
from collections.abc import Iterable, Iterator
from worker_metrics import timed, count as metric_count

READ_BYTES = 64 * 1024


def validate_manifest(manifest: dict) -> None:
    if not isinstance(manifest, dict) or type(manifest.get("format")) is not int or manifest["format"] != 1:
        raise ValueError("unsupported source manifest")
    size = manifest.get("size")
    digest = manifest.get("content_hash", "")
    if type(size) is not int or size < 0:
        raise ValueError("invalid source size")
    if not isinstance(digest, str) or len(digest) != 71 or not digest.startswith("sha256:"):
        raise ValueError("invalid source SHA-256")
    if any(char not in "0123456789abcdef" for char in digest[7:]):
        raise ValueError("noncanonical source SHA-256")
    extents = manifest.get("extents")
    if extents is None:
        extents = []
    if not isinstance(extents, list):
        raise ValueError("invalid source extents")
    remaining = size
    for extent in extents:
        if not isinstance(extent, dict):
            raise ValueError("invalid source extent")
        offset, length = extent.get("offset"), extent.get("length")
        if (
            not isinstance(extent.get("object_key"), str)
            or not extent["object_key"]
            or type(offset) is not int
            or type(length) is not int
            or offset < 0
            or length <= 0
            or length > remaining
            or offset + length > 2**63 - 1
        ):
            raise ValueError("invalid source extent")
        remaining -= length
    if remaining:
        raise ValueError("source extents do not cover declared size")


def read_manifest(s3, bucket: str, version: dict) -> dict:
    """Bind stored bytes to trusted catalog identity before reading any packs."""
    org, root = version["org_id"], version["root_id"]
    if any(not isinstance(value, str) or not value or "/" in value for value in (org, root)):
        raise ValueError("invalid source owner")
    prefix = f"sources/{org}/{root}/"
    ref = version["source_manifest_ref"]
    match = re.fullmatch(re.escape(prefix) +
        r"manifests/[0-9a-f]{64}\.jsonl#(0|[1-9][0-9]*):([1-9][0-9]*):([0-9a-f]{64})", ref)
    if match is None:
        raise ValueError("source manifest requires an owned pack range and checksum")
    offset, length, digest = int(match[1]), int(match[2]), match[3]
    limit = 16 * 1024 * 1024
    if length > limit or offset > limit - length:
        raise ValueError("source manifest range exceeds 16 MiB pack bounds")
    options = {"Bucket": bucket, "Key": ref.rsplit("#", 1)[0],
               "Range": f"bytes={offset}-{offset + length - 1}"}
    response = s3.get_object(**options)
    with response["Body"] as body:
        raw = body.read(length + 1)
    if len(raw) != length:
        raise ValueError("source manifest response length mismatch")
    if hashlib.sha256(raw).hexdigest() != digest:
        raise ValueError("stored source manifest hash mismatch")
    manifest = json.loads(raw)
    validate_manifest(manifest)
    if (manifest["content_hash"] != version["content_hash"]
            or type(version["size_bytes"]) is not int or manifest["size"] != version["size_bytes"]):
        raise ValueError("source manifest does not match catalog version")
    for extent in manifest.get("extents") or []:
        if not extent["object_key"].startswith((prefix + "packs/", prefix + "multipart/")):
            raise ValueError("source extent is outside its catalog owner")
    return manifest


def iter_source_bytes(s3, bucket: str, manifest: dict) -> Iterator[bytes]:
    """Read one extent at a time; successful exhaustion proves the file hash.

    Callers must exhaust this iterator before marking an extraction complete.
    A partial read is not verification of the source.
    """
    validate_manifest(manifest)
    digest = hashlib.sha256()
    for extent in manifest.get("extents") or []:
        offset, length = extent["offset"], extent["length"]
        response = s3.get_object(
            Bucket=bucket,
            Key=extent["object_key"],
            Range=f"bytes={offset}-{offset + length - 1}",
        )
        remaining = length
        with response["Body"] as body:
            while remaining:
                data = body.read(min(READ_BYTES, remaining))
                if not data:
                    raise ValueError("source extent was truncated")
                if len(data) > remaining:
                    raise ValueError("source response exceeded requested extent")
                remaining -= len(data)
                digest.update(data)
                yield data
    if "sha256:" + digest.hexdigest() != manifest["content_hash"]:
        raise ValueError("captured source hash mismatch")


def materialize_source(s3, bucket: str, manifest: dict, destination: str) -> None:
    with open(destination, "wb") as output:
        for data in iter_source_bytes(s3, bucket, manifest):
            output.write(data)


def write_chunks(s3, bucket: str, prefix: str, chunks: Iterable[dict], *, max_record_bytes: int = 1024 * 1024) -> tuple[str, int]:
    """Write bounded chunk records into one replayable, compressed artifact.

    The upload occurs only after extraction/source verification finishes. No
    partial artifact is published if an iterator raises halfway through.
    """
    count = 0
    with tempfile.TemporaryDirectory() as directory:
        path = os.path.join(directory, "chunks.jsonl.gz")
        with open(path, "wb") as raw:
            with gzip.GzipFile(fileobj=raw, mode="wb", filename="", mtime=0) as output:
                for chunk in chunks:
                    encoded = json.dumps(chunk, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode("utf-8")
                    if len(encoded) > max_record_bytes:
                        raise ValueError("artifact record exceeds size limit")
                    output.write(encoded + b"\n")
                    count += 1
        digest = hashlib.sha256()
        with open(path, "rb") as raw:
            while data := raw.read(READ_BYTES):
                digest.update(data)
        key = f"{prefix.rstrip('/')}/{digest.hexdigest()}.jsonl.gz"
        with timed("artifact_upload"):
            s3.upload_file(path, bucket, key, ExtraArgs={"ContentType": "application/gzip"})
        metric_count("artifact_records", count)
    return key, count


def iter_chunks(s3, bucket: str, key: str, *, max_record_bytes: int = 1024 * 1024) -> Iterator[dict]:
    response = s3.get_object(Bucket=bucket, Key=key)
    with response["Body"] as body, gzip.GzipFile(fileobj=body, mode="rb") as records:
        while raw := records.readline(max_record_bytes + 2):
            if len(raw) > max_record_bytes + 1 or not raw.endswith(b"\n"):
                raise ValueError("artifact record exceeds size limit or is truncated")
            yield json.loads(raw)
