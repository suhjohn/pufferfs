"""Reuse a verified native prefix only when immutable source extents prove an append."""
from file_runtime import database
from segment_io import read_checkpoint, write_checkpoint
from segment_runtime import lock_transform
from source_io import read_manifest
from worker_metrics import count


def source_prefix(previous, current):
    if previous["size"] > current["size"]:
        return False
    old, new = previous.get("extents") or [], current.get("extents") or []
    left = right = left_offset = right_offset = 0
    # Physical extent boundaries may differ; compare the referenced byte ranges.
    while left < len(old):
        if right == len(new):
            return False
        a, b = old[left], new[right]
        if a["object_key"] != b["object_key"] or a["offset"] + left_offset != b["offset"] + right_offset:
            return False
        length = min(a["length"] - left_offset, b["length"] - right_offset)
        left_offset += length
        right_offset += length
        if left_offset == a["length"]:
            left, left_offset = left + 1, 0
        if right_offset == b["length"]:
            right, right_offset = right + 1, 0
    return True


def seed_append(job, s3, bucket, manifest, contract):
    if job["transform_checkpoint_ref"] or job["transform_cursor"] or job["chunk_count"] or not job["previous_version_id"]:
        return
    with database() as conn:
        previous = conn.execute("""SELECT e.id AS extraction_id,e.revision,e.append_checkpoint_ref,
                e.append_chunk_count,v.source_manifest_ref,v.content_hash,v.size_bytes
            FROM file_catalog f JOIN file_extractions e ON e.id=f.indexed_extraction_id
            JOIN file_versions v ON v.id=e.version_id
            WHERE f.id=%s AND f.indexed_version_id=%s AND NOT v.deleted
              AND e.row_format=2 AND e.status='complete' AND e.source_verified
              AND e.append_checkpoint_ref<>'' AND e.artifacts_retired_at IS NULL AND e.revision=%s""",
            (job["file_id"], job["previous_version_id"], job["revision"])).fetchone()
    if previous is None:
        return
    previous.update(org_id=job["org_id"], root_id=job["root_id"])
    old_manifest = read_manifest(s3, bucket, previous)
    if not source_prefix(old_manifest, manifest):
        return
    state = read_checkpoint(s3, bucket, previous, previous["append_checkpoint_ref"])
    if state.get("contract") != contract:
        return
    if (state.get("source_hash") != previous["content_hash"]
            or state.get("source_size") != previous["size_bytes"]
            or state.get("source_manifest_ref") != previous["source_manifest_ref"]
            or state.get("input_offset") != previous["size_bytes"]
            or state.get("digest", {}).get("hash") != previous["content_hash"]):
        raise ValueError("append prefix is not verified")
    state.update(source_manifest_ref=job["source_manifest_ref"], source_hash=job["content_hash"],
                 source_size=job["size_bytes"])
    checkpoint = write_checkpoint(s3, bucket, job, state)
    prefix_count = previous["append_chunk_count"]
    with database() as conn:
        # Publication uses file -> work order. No transaction spans S3 IO.
        file = conn.execute("SELECT indexed_extraction_id FROM file_catalog WHERE id=%s FOR SHARE", (job["file_id"],)).fetchone()
        if file is None or file["indexed_extraction_id"] != previous["extraction_id"]:
            return
        lock_transform(conn, job)
        copied = conn.execute("""WITH reusable AS MATERIALIZED (
            SELECT s.id,m.ordinal_start,s.chunk_count
            FROM extraction_segments m JOIN file_segments s ON s.id=m.segment_id
            WHERE m.extraction_id=%s AND m.ordinal_start<%s AND s.file_id=%s
              AND s.retired_at IS NULL AND s.indexed_at IS NOT NULL
              AND m.ordinal_start+s.chunk_count<=%s
            ORDER BY s.id FOR SHARE OF s
        ), inserted AS (
            INSERT INTO extraction_segments(extraction_id,ordinal_start,segment_id)
            SELECT %s,ordinal_start,id FROM reusable RETURNING ordinal_start,segment_id
        ) SELECT COALESCE(SUM(r.chunk_count),0) AS chunks,COALESCE(MIN(r.ordinal_start),0) AS first,
            COALESCE(MAX(r.ordinal_start+r.chunk_count),0) AS last
            FROM inserted i JOIN reusable r ON r.id=i.segment_id""",
            (previous["extraction_id"], prefix_count, job["file_id"], prefix_count, job["extraction_id"])).fetchone()
        if copied["chunks"] != prefix_count or copied["first"] != 0 or copied["last"] != prefix_count:
            raise ValueError("append prefix segments are not complete")
        conn.execute("UPDATE file_extractions SET chunk_count=%s,updated_at=NOW() WHERE id=%s",
            (prefix_count, job["extraction_id"]))
        conn.execute("""UPDATE file_work SET transform_cursor=%s,transform_checkpoint_ref=%s,
            index_cursor=%s,updated_at=NOW() WHERE id=%s""",
            (previous["size_bytes"], checkpoint, prefix_count, job["id"]))
    job.update(chunk_count=prefix_count, index_cursor=prefix_count,
               transform_cursor=previous["size_bytes"], transform_checkpoint_ref=checkpoint)
    count("reused_chunks", prefix_count)
    count("reused_source_bytes", previous["size_bytes"])
