"""Owned immutable checkpoints and bounded canonical chunk segments."""
import hashlib
import json
import re

from file_runtime import database, stable_id
from source_io import write_chunks

SEGMENT_CHUNKS = 64
SEGMENT_BYTES = 1024 * 1024
CHECKPOINT_BYTES = 2 * 1024 * 1024


def record_size(record):
    return len(json.dumps(record, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode()) + 1


def segment_prefix(records, *, final=False):
    """Leave a bounded unfinished segment unless its count/bytes are full."""
    total, length = 0, 0
    for record in records:
        size = record_size(record)
        if size > SEGMENT_BYTES:
            raise ValueError("chunk exceeds segment bounds")
        if length == SEGMENT_CHUNKS or total + size > SEGMENT_BYTES:
            return length
        total += size
        length += 1
    return length if final or length == SEGMENT_CHUNKS else 0


def extraction_prefix(job):
    owners = [job[key] for key in ("org_id", "root_id", "extraction_id")]
    if any(not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_-]+", value) for value in owners):
        raise ValueError("invalid extraction owner")
    return "extractions/" + "/".join(owners) + "/"


def write_checkpoint(s3, bucket, job, state):
    data = json.dumps(state, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()
    if len(data) > CHECKPOINT_BYTES:
        raise ValueError("transform checkpoint exceeds two MiB")
    key = extraction_prefix(job) + "checkpoints/" + hashlib.sha256(data).hexdigest() + ".json"
    s3.put_object(Bucket=bucket, Key=key, Body=data, ContentType="application/json")
    return key


def read_checkpoint(s3, bucket, job, key):
    prefix = extraction_prefix(job) + "checkpoints/"
    if not isinstance(key, str) or not re.fullmatch(re.escape(prefix) + r"[0-9a-f]{64}\.json", key):
        raise ValueError("checkpoint is outside its extraction owner")
    response = s3.get_object(Bucket=bucket, Key=key)
    with response["Body"] as body:
        data = body.read(CHECKPOINT_BYTES + 1)
    if len(data) > CHECKPOINT_BYTES or hashlib.sha256(data).hexdigest() != key[len(prefix):-5]:
        raise ValueError("transform checkpoint checksum or size mismatch")
    state = json.loads(data)
    if not isinstance(state, dict):
        raise ValueError("invalid transform checkpoint document")
    return state


def write_segment(s3, bucket, job, records):
    if not 1 <= len(records) <= SEGMENT_CHUNKS:
        raise ValueError("invalid segment record count")
    start = records[0]["chunk_index"]
    if type(start) is not int or start < 0 or [r["chunk_index"] for r in records] != list(range(start, start + len(records))):
        raise ValueError("segment ordinals are not contiguous")
    size = sum(record_size(row) for row in records)
    if size > SEGMENT_BYTES:
        raise ValueError("segment exceeds one MiB")
    prefix = extraction_prefix(job) + "segments"
    key, count = write_chunks(s3, bucket, prefix, records)
    locations = [record["location"] for record in records]
    return {"id": stable_id(job["extraction_id"], str(start)), "ordinal_start": start,
        "chunk_count": count, "chunks_ref": key,
        "line_start": min((loc["line_start"] for loc in locations if "line_start" in loc), default=None),
        "line_end": max((loc["line_end"] for loc in locations if "line_end" in loc), default=None),
        "page_start": min((loc["page_number"] for loc in locations if "page_number" in loc), default=None),
        "page_end": max((loc["page_number"] for loc in locations if "page_number" in loc), default=None)}


def write_segment_manifest(s3, bucket, job, added):
    count = job["chunk_count"] + sum(s["chunk_count"] for s in added)

    def records():
        yield {"format": 2, "kind": "segment_manifest", "chunk_count": count}
        after = -1
        while True:
            with database() as conn:
                existing = conn.execute("""SELECT m.ordinal_start,s.chunk_count,s.chunks_ref,s.id
                    FROM extraction_segments m JOIN file_segments s ON s.id=m.segment_id
                    WHERE m.extraction_id=%s AND m.ordinal_start>%s
                    ORDER BY m.ordinal_start LIMIT 256""", (job["extraction_id"], after)).fetchall()
            for segment in existing:
                yield segment
                after = segment["ordinal_start"]
            if len(existing) < 256:
                break
        for segment in added:
            yield {name: segment[name] for name in ("ordinal_start", "chunk_count", "chunks_ref", "id")}

    key, _ = write_chunks(s3, bucket, extraction_prefix(job) + "manifests", records())
    return key
