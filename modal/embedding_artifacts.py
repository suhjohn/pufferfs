"""Bounded Nomic vector packs in S3; Postgres stores compact pack directories."""

import hashlib
import math
import struct
import uuid

from file_runtime import database
from nomic_model import MODEL, MODEL_REVISION, CODE_REVISION, DIMENSIONS
from worker_metrics import timed, count

CACHE_REVISION = f"{MODEL}@{MODEL_REVISION}:{CODE_REVISION}:search_document:normalized:f32"
VECTOR_BYTES = DIMENSIONS * 4
MAX_ROWS = 512


def embedding_vectors(org_id, chunks, encode, s3, bucket, *, connect=database):
    """Resolve at most 512 packed float32 vectors; encode receives only misses.

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
    with timed("cache_lookup"), connect() as conn:
        packs = conn.execute("""SELECT object_key,content_hashes,dimensions FROM embedding_packs
            WHERE org_id=%s AND model_revision=%s AND retired_at IS NULL
              AND content_hashes && %s::text[]""", (org_id, CACHE_REVISION, list(texts))).fetchall()
    vectors = {}
    # Concurrent cold misses may publish overlapping packs. Prefer the pack
    # covering the most requested hashes; never depend on a single writer or
    # on process-local cache state for correctness.
    packs.sort(key=lambda row: (-len(set(row["content_hashes"]) & texts.keys()), row["object_key"]))
    for row in packs:
        hashes = row["content_hashes"]
        if row["dimensions"] != DIMENSIONS or not 1 <= len(hashes) <= MAX_ROWS:
            raise ValueError("invalid embedding pack directory")
        positions = [(i, digest) for i, digest in enumerate(hashes) if digest in texts and digest not in vectors]
        if not positions:
            continue
        start, end = positions[0][0] * VECTOR_BYTES, (positions[-1][0] + 1) * VECTOR_BYTES
        with timed("cache_read"), connect() as conn:
            # Hold the pack lock through its bounded S3 read. Retirement can
            # win before this statement, but cannot delete a pack being read.
            live = conn.execute("""UPDATE embedding_packs SET last_used_at=NOW()
                WHERE object_key=%s AND org_id=%s AND retired_at IS NULL RETURNING object_key""",
                (row["object_key"], org_id)).fetchone()
            if live is None:
                continue
            response = s3.get_object(Bucket=bucket, Key=row["object_key"], Range=f"bytes={start}-{end - 1}")
            with response["Body"] as body:
                data = body.read(end - start + 1)
        if len(data) != end - start:
            raise ValueError("embedding pack truncated or oversized")
        for position, digest in positions:
            offset = position * VECTOR_BYTES - start
            vector = data[offset:offset + VECTOR_BYTES]
            if not all(math.isfinite(value) for value, in struct.iter_unpack("<f", vector)):
                raise ValueError("nonfinite cached embedding")
            vectors[digest] = vector
    missing = [digest for digest in texts if digest not in vectors]
    count("cache_hits", len(vectors))
    count("cache_misses", len(missing))
    if missing:
        with timed("encode"):
            encoded = encode([texts[digest] for digest in missing])
        if len(encoded) != len(missing):
            raise ValueError("embedding count mismatch")
        pack = bytearray()
        for digest, vector in zip(missing, encoded):
            if len(vector) != DIMENSIONS or not all(math.isfinite(value) for value in vector):
                raise ValueError("invalid embedding vector")
            packed = struct.pack(f"<{DIMENSIONS}f", *vector)
            # Both cache hits and misses use the same float32 representation.
            vectors[digest] = packed
            pack.extend(packed)
        key = f"embeddings/{org_id}/{hashlib.sha256(CACHE_REVISION.encode()).hexdigest()}/{hashlib.sha256(pack).hexdigest()}-{uuid.uuid4().hex}.f32"
        # Persist a cleanup target before the PUT, but expose no cache hashes
        # until the entire pack is durable. One directory row, never one row per
        # vector. The bounded float32 body is at most 1.5 MiB.
        with connect() as conn:
            conn.execute("""INSERT INTO embedding_packs(object_key,org_id,model_revision,dimensions)
                VALUES(%s,%s,%s,%s)""", (key, org_id, CACHE_REVISION, DIMENSIONS))
        with timed("cache_store"), connect() as conn:
            live = conn.execute("""SELECT object_key FROM embedding_packs
                WHERE object_key=%s AND retired_at IS NULL FOR UPDATE""", (key,)).fetchone()
            if live is None:
                raise RuntimeError("embedding upload lost its cache allocation")
            s3.put_object(Bucket=bucket, Key=key, Body=bytes(pack), ContentType="application/octet-stream")
            conn.execute("UPDATE embedding_packs SET content_hashes=%s,last_used_at=NOW() WHERE object_key=%s", (missing, key))
        count("cache_packs_written")
    return [vectors[chunk["content_hash"]] for chunk in chunks]
