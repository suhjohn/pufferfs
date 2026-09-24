"""Bounded native transformation turns with source/parser checkpoints."""
from segment_io import (SEGMENT_BYTES, SEGMENT_CHUNKS, read_checkpoint,
    write_checkpoint, write_segment, write_segment_manifest, record_size, segment_prefix)
from segment_runtime import begin_segmented_extraction, save_segment_turn
from source_digest import SourceDigest
from source_io import iter_source_range
from text_stream import TextStream
from worker_metrics import count
from append_segments import seed_append

SOURCE_TURN_BYTES = 4 * 1024 * 1024
TEXT_CONTRACT = "text-stream-v1"


def load_native_state(job, s3, bucket):
    if not job["transform_checkpoint_ref"]:
        if job["transform_cursor"] or job["chunk_count"]:
            raise ValueError("native extraction is missing its checkpoint")
        return TextStream(), [], None
    state = read_checkpoint(s3, bucket, job, job["transform_checkpoint_ref"])
    if (state.get("contract") != TEXT_CONTRACT or state.get("source_manifest_ref") != job["source_manifest_ref"]
            or state.get("source_hash") != job["content_hash"] or state.get("source_size") != job["size_bytes"]
            or state.get("input_offset") != job["transform_cursor"]):
        raise ValueError("native checkpoint does not match its immutable input")
    stream = TextStream(state=state["parser"])
    pending = state["pending"]
    if (not isinstance(pending, list) or len(pending) >= SEGMENT_CHUNKS
            or sum(record_size(row) for row in pending) > SEGMENT_BYTES
            or [row["chunk_index"] for row in pending] != list(range(job["chunk_count"], stream.ordinal))
            or stream.source_cursor + len(stream.redactor.pending) + stream.redactor.removed != job["transform_cursor"]):
        raise ValueError("invalid native checkpoint prefix")
    return stream, pending, state["digest"]["state"]


def transform_native_segments(job, s3, bucket, manifest, check_lease, stopping):
    begin_segmented_extraction(job)
    seed_append(job, s3, bucket, manifest, TEXT_CONTRACT)
    stream, pending, digest_state = load_native_state(job, s3, bucket)
    cursor = job["transform_cursor"]
    end = min(job["size_bytes"], cursor + SOURCE_TURN_BYTES)
    prepared = []

    def flush(*, final=False):
        while length := segment_prefix(pending, final=final):
            check_lease()
            prepared.append(write_segment(s3, bucket, job, pending[:length]))
            del pending[:length]

    with SourceDigest(digest_state) as digest:
        source = iter_source_range(s3, bucket, manifest, cursor, end)
        try:
            for block in source:
                check_lease()
                digest.update(block)
                pending.extend(stream.feed(block))
                cursor += len(block)
                flush()
                if stopping is not None and stopping.is_set():
                    break
        finally:
            source.close()
        digest_snapshot = digest.snapshot()
        complete = cursor == job["size_bytes"]
        if complete and digest_snapshot["hash"] != job["content_hash"]:
            raise ValueError("captured source hash mismatch")
        # This is deliberately before EOF finalization. A later append can
        # continue a partial UTF-8 line, data URL or unfinished final chunk.
        checkpoint = {"contract": TEXT_CONTRACT, "source_manifest_ref": job["source_manifest_ref"],
            "source_hash": job["content_hash"], "source_size": job["size_bytes"],
            "input_offset": cursor, "parser": stream.snapshot(), "pending": pending,
            "digest": digest_snapshot}
        checkpoint_ref = write_checkpoint(s3, bucket, job, checkpoint)
    append_count = job["chunk_count"] + sum(segment["chunk_count"] for segment in prepared)
    manifest_ref = ""
    if complete:
        pending.extend(stream.feed(None))
        flush(final=True)
        manifest_ref = write_segment_manifest(s3, bucket, job, prepared)
    check_lease()
    count("source_resumed_bytes", job["transform_cursor"])
    count("prepared_segments", len(prepared))
    return save_segment_turn(job, prepared, checkpoint_ref, cursor, complete=complete,
        chunks_ref=manifest_ref, append_checkpoint_ref=checkpoint_ref if complete else "",
        append_chunk_count=append_count if complete else 0)
