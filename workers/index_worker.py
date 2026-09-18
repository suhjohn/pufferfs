"""Canonical extracted text -> bounded provider writes -> catalog publication."""

import hashlib
from contextlib import closing

from index_mutations import deletion_mutation, index_row, mutation_batches
from index_publish import publish_head
from index_client import EMBEDDING_BATCH_DOCUMENTS, write_options
from source_io import iter_chunks
from worker_metrics import profile_work, timed, count


@profile_work("index")
def publish_extraction(job, s3, bucket, tp, check_lease):
    if job["stage"] != "index" or job["row_format"] != 1:
        raise ValueError("unsupported publication phase or row format")
    namespace = job["namespace"]
    if not namespace:
        raise ValueError("missing root namespace")
    count("chunks", job["chunk_count"])
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
        def rows():
            seen = 0
            with closing(iter_chunks(s3, bucket, job["chunks_ref"])) as chunks:
                for chunk in chunks:
                    if chunk["chunk_index"] != seen:
                        raise ValueError("noncontiguous extraction chunk ordinals")
                    if hashlib.sha256(chunk["content"].encode()).hexdigest() != chunk["content_hash"]:
                        raise ValueError("chunk content hash mismatch")
                    seen += 1
                    yield index_row(job, chunk)
            if seen != job["chunk_count"]:
                raise ValueError("extraction chunk count mismatch")

        max_rows = 512 if job["vector_disabled"] else EMBEDDING_BATCH_DOCUMENTS
        for mutation in mutation_batches(rows(), max_rows=max_rows, max_bytes=7 * 1024 * 1024):
            check_lease()
            with timed("search_write"):
                tp.namespace(namespace).write(**mutation, **write_options(job["vector_disabled"]))
    check_lease()
    return publish_head(job)
