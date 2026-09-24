"""Segment output from whole-input document decoders without one giant artifact.

These decoders still need their whole source/container. Completed segment
artifacts survive a crash; replay validates their deterministic output and only
uploads new segments. Indexing is scheduled in bounded turns after decoding.
"""
from contextlib import closing

from file_runtime import database
from segment_io import segment_prefix, write_segment, write_segment_manifest
from segment_runtime import store_segments
from source_io import iter_chunks


def prepare_decoded_segments(job, s3, bucket, chunks, lock_owner, *, start_ordinal=0, manifest=True):
    pending, position = [], start_ordinal
    saved_count = job["chunk_count"]
    if not 0 <= start_ordinal <= saved_count:
        raise ValueError("invalid decoded segment starting position")

    def save(records):
        nonlocal position
        if [r["chunk_index"] for r in records] != list(range(position, position + len(records))):
            raise ValueError("decoded chunk ordinals are not contiguous")
        if position < saved_count:
            with database() as conn:
                segment = conn.execute("""SELECT s.chunks_ref,s.chunk_count FROM extraction_segments m
                    JOIN file_segments s ON s.id=m.segment_id WHERE m.extraction_id=%s
                      AND m.ordinal_start=%s AND s.retired_at IS NULL""", (job["extraction_id"], position)).fetchone()
            if segment is None or segment["chunk_count"] != len(records):
                raise ValueError("decoded segment checkpoint changed")
            with closing(iter_chunks(s3, bucket, segment["chunks_ref"])) as previous:
                if list(previous) != records:
                    raise ValueError("decoder output changed while resuming extraction")
        else:
            segment = write_segment(s3, bucket, job, records)
            with database() as conn:
                lock_owner(conn, job)
                count = store_segments(conn, job, [segment])
                conn.execute("UPDATE file_extractions SET chunk_count=%s,updated_at=NOW() WHERE id=%s", (count, job["extraction_id"]))
            job["chunk_count"] = count
        position += len(records)

    for chunk in chunks:
        pending.append(chunk)
        while length := segment_prefix(pending):
            save(pending[:length])
            del pending[:length]
    if pending:
        save(pending)
    if position != job["chunk_count"]:
        raise ValueError("decoded extraction ended before its saved prefix")
    return write_segment_manifest(s3, bucket, job, []) if manifest else "", position
