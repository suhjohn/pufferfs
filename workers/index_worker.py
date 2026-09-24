"""Canonical extracted text -> bounded provider writes -> catalog publication."""

import hashlib
from contextlib import closing

from index_mutations import deletion_mutation, index_row, mutation_batches
from index_publish import publish_head
from index_client import EMBEDDING_BATCH_DOCUMENTS, write_options
from file_runtime import checkpoint_index, yield_index
from source_io import iter_chunks
from worker_metrics import profile_work, timed, count
from provider_capacity import ProviderDeferred, write_embedding, limits as embedding_limits
from segment_runtime import prepared_segments, finish_index_turn


@profile_work("index")
def publish_extraction(job, s3, bucket, tp, check_lease, stopping):
    if job["stage"] != "index" or job["row_format"] not in (1, 2):
        raise ValueError("unsupported publication phase or row format")
    namespace = job["namespace"]
    if not namespace:
        raise ValueError("missing root namespace")
    count("chunks", job["chunk_count"])
    if stopping.is_set():
        return yield_index(job)
    if job["version_deleted"]:
        for _ in range(100):
            check_lease()
            result = tp.namespace(namespace).write(**deletion_mutation(job))
            remaining = getattr(result, "rows_remaining", None)
            if remaining is False or remaining is None:
                break
            if remaining is not True:
                raise ValueError("invalid deletion progress response")
        else:
            raise RuntimeError("deletion requires another bounded attempt")
    else:
        cursor = job["index_cursor"]
        if not 0 <= cursor <= job["chunk_count"]:
            raise ValueError("invalid saved index progress")
        count("resumed_chunks", cursor)

        def rows():
            if job["row_format"] == 2:
                position = job["index_cursor"]
                for segment in prepared_segments(job, limit=8 if job["vector_disabled"] else 1):
                    start, length = segment["ordinal_start"], segment["chunk_count"]
                    if not start <= position < start + length:
                        raise ValueError("prepared segment prefix is not contiguous")
                    seen = start
                    with closing(iter_chunks(s3, bucket, segment["chunks_ref"])) as chunks:
                        for chunk in chunks:
                            if (chunk["chunk_index"] != seen or seen >= start + length
                                    or hashlib.sha256(chunk["content"].encode()).hexdigest() != chunk["content_hash"]):
                                raise ValueError("invalid segment chunk identity")
                            seen += 1
                            if chunk["chunk_index"] >= job["index_cursor"]:
                                yield index_row(job, chunk, segment)
                    if seen != start + length:
                        raise ValueError("segment chunk count mismatch")
                    position = seen
                if position == job["index_cursor"] and position != job["chunk_count"]:
                    raise ValueError("prepared segments are missing")
                return
            seen = 0
            with closing(iter_chunks(s3, bucket, job["chunks_ref"])) as chunks:
                for chunk in chunks:
                    if chunk["chunk_index"] != seen:
                        raise ValueError("noncontiguous extraction chunk ordinals")
                    if hashlib.sha256(chunk["content"].encode()).hexdigest() != chunk["content_hash"]:
                        raise ValueError("chunk content hash mismatch")
                    seen += 1
                    if chunk["chunk_index"] >= job["index_cursor"]:
                        yield index_row(job, chunk)
            if seen != job["chunk_count"]:
                raise ValueError("extraction chunk count mismatch")

        max_rows = 512 if job["vector_disabled"] else EMBEDDING_BATCH_DOCUMENTS
        token_limit = None if job["vector_disabled"] else embedding_limits()[1]
        for mutation in mutation_batches(rows(), max_rows=max_rows, max_bytes=7 * 1024 * 1024, max_tokens=token_limit):
            pending = mutation["upsert_rows"]
            while pending:
                check_lease()
                if stopping.is_set():
                    return yield_index(job)
                with timed("search_write"):
                    if job["vector_disabled"]:
                        tp.namespace(namespace).write(upsert_rows=pending, **write_options(True))
                        written = len(pending)
                    else:
                        try:
                            written = write_embedding(job, tp, namespace, {"upsert_rows": pending}, write_options(False))
                        except ProviderDeferred as capacity:
                            return yield_index(job, delay=capacity.delay)
                end = cursor + written
                checkpoint_index(job, cursor, end)
                count("confirmed_chunks", written)
                cursor = end
                pending = pending[written:]
                # Save the confirmed response, then stop this attempt if its
                # heartbeat failed while the provider request was in flight.
                # This check must precede the bounded-turn ownership release.
                check_lease()
                if stopping.is_set() and cursor < job["chunk_count"]:
                    return yield_index(job)
        if job["row_format"] == 2 and finish_index_turn(job, cursor) != "publish":
            return "yielded"
    check_lease()
    return publish_head(job)
