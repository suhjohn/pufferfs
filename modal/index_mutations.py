"""Build version-isolated index rows. External publication lives in the worker."""

import json
import posixpath

from file_runtime import stable_id
from extraction import indexed_file_type


def deletion_mutation(job):
    if not job["version_deleted"] or type(job["sequence"]) is not int or job["sequence"] < 1:
        raise ValueError("deletion requires a valid tombstone version")
    # Newer incarnations survive even if this request finishes after their
    # publication. Legacy rows lack a file ID and are scoped by root + path.
    return {"delete_by_filter": ["And", [
        ["root_id", "Eq", job["root_id"]],
        ["file_path", "Eq", job["file_path"]],
        ["Or", [
            ["And", [["file_id", "Eq", job["file_id"]], ["version_sequence", "Lte", job["sequence"]]]],
            ["file_id", "Eq", None],
        ]],
    ]], "delete_by_filter_allow_partial": True}


def index_row(job, chunk, vector=None):
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
    if vector is not None:
        row["vector"] = vector
    return row


def mutation_batches(rows, *, max_rows=512, max_bytes=8 * 1024 * 1024):
    """Bounded JSON write payloads, suitable for persisting before application."""
    if max_rows < 1 or max_bytes < 32:
        raise ValueError("invalid mutation bounds")
    batch, size = [], len(b'{"upsert_rows":[]}')
    for row in rows:
        length = len(json.dumps(row, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode())
        if length + len(b'{"upsert_rows":[]}') > max_bytes:
            raise ValueError("index row exceeds mutation byte limit")
        if batch and (len(batch) >= max_rows or size + length + 1 > max_bytes):
            yield {"upsert_rows": batch}
            batch, size = [], len(b'{"upsert_rows":[]}')
        size += length + int(bool(batch))
        batch.append(row)
    if batch:
        yield {"upsert_rows": batch}
