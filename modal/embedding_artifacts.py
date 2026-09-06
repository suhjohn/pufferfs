"""Bounded Nomic vector packs in S3; Postgres stores only their locators."""

import hashlib
import math
from pathlib import Path
import struct
import tempfile
import uuid

from file_runtime import database
from nomic_model import MODEL, MODEL_REVISION, CODE_REVISION, DIMENSIONS

CACHE_REVISION = f"{MODEL}@{MODEL_REVISION}:{CODE_REVISION}:search_document:normalized:f32"
VECTOR_BYTES = DIMENSIONS * 4
MAX_ROWS = 64


def embedding_vectors(org_id, chunks, encode, s3, bucket, *, connect=database):
    """Resolve at most 64 text vectors; encode receives only cache misses.

    Caller owns the Nomic model, search_document prefix and normalization.
    Duplicate content within the batch is encoded once. Tenant-scoped cache
    locators prevent cross-organization content-presence disclosure.
    """
    if not 1 <= len(chunks) <= MAX_ROWS:
        raise ValueError("invalid embedding batch size")
    texts = {}
    for chunk in chunks:
        digest = hashlib.sha256(chunk["content"].encode()).hexdigest()
        if digest != chunk["content_hash"]:
            raise ValueError("chunk content hash mismatch")
        texts[digest] = chunk["content"]
    with connect() as conn:
        rows = conn.execute("""SELECT l.* FROM embedding_locations l
            JOIN embedding_packs p ON p.object_key=l.object_key
            WHERE l.org_id=%s AND l.model_revision=%s AND l.content_hash=ANY(%s)
              AND p.retired_at IS NULL""",
            (org_id, CACHE_REVISION, list(texts))).fetchall()
    vectors = {}
    packs = {}
    for row in rows:
        packs.setdefault(row["object_key"], []).append(row)
    for key, locations in packs.items():
        # Packs contain at most MAX_ROWS vectors. Read a contiguous range once
        # for all hits from the same pack, not a GET for each individual vector.
        start = min(row["byte_offset"] for row in locations)
        end = max(row["byte_offset"] + row["byte_length"] for row in locations)
        if start < 0 or end - start > MAX_ROWS * VECTOR_BYTES:
            raise ValueError("invalid embedding pack range")
        with connect() as conn:
            # Refresh and lock one whole pack through the bounded range read.
            # If cleanup won since the locator snapshot, these are cache misses.
            live = conn.execute("""UPDATE embedding_packs SET last_used_at=NOW()
                WHERE object_key=%s AND org_id=%s AND retired_at IS NULL RETURNING object_key""", (key, org_id)).fetchone()
            if live is None:
                continue
            response = s3.get_object(Bucket=bucket, Key=key, Range=f"bytes={start}-{end - 1}")
            with response["Body"] as body:
                data = body.read(end - start + 1)
        if len(data) != end - start:
            raise ValueError("embedding pack truncated or oversized")
        for row in locations:
            if row["dimensions"] != DIMENSIONS or row["byte_length"] != VECTOR_BYTES:
                raise ValueError("embedding dimensions mismatch")
            offset = row["byte_offset"] - start
            vector = list(struct.unpack(f"<{DIMENSIONS}f", data[offset:offset + VECTOR_BYTES]))
            if not all(math.isfinite(value) for value in vector):
                raise ValueError("nonfinite cached embedding")
            vectors[row["content_hash"]] = vector
    missing = [digest for digest in texts if digest not in vectors]
    if missing:
        encoded = encode([texts[digest] for digest in missing])
        if len(encoded) != len(missing):
            raise ValueError("embedding count mismatch")
        pack = bytearray()
        for digest, vector in zip(missing, encoded):
            if len(vector) != DIMENSIONS or not all(math.isfinite(value) for value in vector):
                raise ValueError("invalid embedding vector")
            packed = struct.pack(f"<{DIMENSIONS}f", *vector)
            # Both cache hits and misses use the same float32 representation.
            vectors[digest] = list(struct.unpack(f"<{DIMENSIONS}f", packed))
            pack.extend(packed)
        key = f"embeddings/{org_id}/{hashlib.sha256(CACHE_REVISION.encode()).hexdigest()}/{hashlib.sha256(pack).hexdigest()}-{uuid.uuid4().hex}.f32"
        # Register before network IO so even an interrupted/ambiguous upload has
        # a cleanup target. A retired identity is never reused by another PUT.
        with connect() as conn:
            conn.execute("INSERT INTO embedding_packs(object_key,org_id) VALUES(%s,%s)", (key, org_id))
        with tempfile.TemporaryDirectory(prefix="pufferfs-vectors-") as directory:
            path = Path(directory) / "vectors.f32"
            path.write_bytes(pack)
            with connect() as conn:
                live = conn.execute("""UPDATE embedding_packs SET last_used_at=NOW()
                    WHERE object_key=%s AND retired_at IS NULL RETURNING object_key""", (key,)).fetchone()
                if live is None:
                    raise RuntimeError("embedding upload lost its cache allocation")
                s3.upload_file(str(path), bucket, key, ExtraArgs={"ContentType": "application/octet-stream"})
                for index, digest in enumerate(missing):
                    conn.execute("""INSERT INTO embedding_locations(org_id,model_revision,content_hash,
                        object_key,byte_offset,byte_length,dimensions) VALUES(%s,%s,%s,%s,%s,%s,%s)
                        ON CONFLICT(org_id,model_revision,content_hash) DO NOTHING""",
                        (org_id, CACHE_REVISION, digest, key, index * VECTOR_BYTES, VECTOR_BYTES, DIMENSIONS))
    return [vectors[chunk["content_hash"]] for chunk in chunks]
