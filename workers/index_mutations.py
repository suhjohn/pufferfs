"""Build version-isolated index rows. External publication lives in the worker."""

import json
import posixpath

from file_runtime import stable_id
from extraction import indexed_file_type


def deletion_mutation(job):
    if not job["version_deleted"] or type(job["sequence"]) is not int or job["sequence"] < 1:
        raise ValueError("deletion requires a valid tombstone version")
    # Newer incarnations survive even if this request finishes after publication.
    return {"delete_by_filter": ["And", [
        ["root_id", "Eq", job["root_id"]],
        ["file_path", "Eq", job["file_path"]],
        ["file_id", "Eq", job["file_id"]],
        ["version_sequence", "Lte", job["sequence"]],
    ]], "delete_by_filter_allow_partial": True}


def index_row(job, chunk, segment=None):
    ordinal = chunk["chunk_index"]
    if type(ordinal) is not int or ordinal < 0:
        raise ValueError("invalid chunk ordinal")
    row = {
        "id": stable_id(job["org_id"], job["root_id"], job["file_id"], job["extraction_id"], str(ordinal)),
        "root_id": job["root_id"], "file_id": job["file_id"], "version_id": job["version_id"],
        "extraction_id": job["extraction_id"], "version_sequence": job["sequence"],
        "extraction_sequence": job["extraction_sequence"],
        "file_path": job["file_path"], "absolute_path": posixpath.join(job["source_path"], job["file_path"]),
        "file_hash": job["content_hash"], "content_hash": chunk["content_hash"],
        "file_type": indexed_file_type(job["file_path"]),
        "content": chunk["content"], "chunk_index": ordinal,
        "source_manifest_ref": job["source_manifest_ref"],
        "location_json": json.dumps(chunk["location"], sort_keys=True, separators=(",", ":")),
    }
    for key in ("page_number", "line_start", "line_end"):
        if key in chunk["location"]:
            row[key] = chunk["location"][key]
    if segment is not None:
        if (job["row_format"] != 2 or segment["owner_extraction_id"] != job["extraction_id"]
                or not segment["ordinal_start"] <= ordinal < segment["ordinal_start"] + segment["chunk_count"]):
            raise ValueError("index row does not belong to its immutable segment")
        row["segment_id"] = segment["id"]
        row["id"] = stable_id(job["org_id"], job["root_id"], job["file_id"], segment["id"], str(ordinal))
    return row


def mutation_batches(rows, *, max_rows=512, max_bytes=8 * 1024 * 1024, max_tokens=None):
    """Bounded JSON write payloads regenerated from canonical chunks on retry."""
    if max_rows < 1 or max_bytes < 32:
        raise ValueError("invalid mutation bounds")
    batch, size, tokens = [], len(b'{"upsert_rows":[]}'), 0
    for row in rows:
        length = len(json.dumps(row, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode())
        if length + len(b'{"upsert_rows":[]}') > max_bytes:
            raise ValueError("index row exceeds mutation byte limit")
        cost = len(row["content"].encode()) + 128
        if max_tokens is not None and cost > max_tokens:
            raise ValueError("index row exceeds embedding token budget")
        if batch and (len(batch) >= max_rows or size + length + 1 > max_bytes
                or (max_tokens is not None and tokens + cost > max_tokens)):
            yield {"upsert_rows": batch}
            batch, size, tokens = [], len(b'{"upsert_rows":[]}'), 0
        size += length + int(bool(batch))
        tokens += cost
        batch.append(row)
    if batch:
        yield {"upsert_rows": batch}
