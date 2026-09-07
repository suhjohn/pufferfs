"""Prepare durable index mutations; never issue Turbopuffer writes here."""

from itertools import islice

from embedding_artifacts import MAX_ROWS, embedding_vectors
from file_runtime import database
from index_mutations import deletion_mutation, index_row, mutation_batches
from source_io import iter_chunks, write_chunks

MUTATION_RECORD_BYTES = 8 * 1024 * 1024


def prepare_mutations(job, namespace, encode, s3, bucket, *, connect=database):
    if job["stage"] != "index":
        raise ValueError("only index work can prepare mutations")
    if job["mutation_ref"]:
        if job["mutation_batch_count"] is None:
            raise ValueError("mutation artifact missing batch count")
        return job["mutation_ref"], job["mutation_batch_count"]

    def rows():
        if job["version_deleted"]:
            return
        source = iter_chunks(s3, bucket, job["chunks_ref"])
        seen = 0
        try:
            while batch := list(islice(source, MAX_ROWS)):
                # The index worker renews its lease in the background. The
                # mutation registration below fences ownership before publish;
                # do not write a heartbeat for every embedding/cache batch.
                for chunk in batch:
                    if chunk["chunk_index"] != seen:
                        raise ValueError("noncontiguous extraction chunk ordinals")
                    seen += 1
                vectors = ([None] * len(batch) if job["vector_disabled"] else
                           embedding_vectors(job["org_id"], batch, encode, s3, bucket, connect=connect))
                for chunk, vector in zip(batch, vectors):
                    yield index_row(job, chunk, vector)
        finally:
            source.close()
        if seen != job["chunk_count"]:
            raise ValueError("extraction chunk count mismatch")

    # Leave room for the namespace envelope in each artifact record. All rows
    # for a file have the same namespace under the existing routing scheme.
    records = ({"namespace": namespace, "write": mutation} for mutation in
               mutation_batches(rows(), max_bytes=7 * 1024 * 1024))
    if job["version_deleted"]:
        records = iter([{"namespace": namespace, "write": deletion_mutation(job)}])
    prefix = f"mutations/{job['org_id']}/{job['root_id']}/{job['extraction_id']}"
    ref, count = write_chunks(s3, bucket, prefix, records, max_record_bytes=MUTATION_RECORD_BYTES)
    with connect() as conn:
        updated = conn.execute("""UPDATE file_work SET mutation_ref=%s,mutation_batch_count=%s,updated_at=NOW()
            WHERE id=%s AND attempt_token=%s AND status='running' AND lease_until>NOW()
            AND mutation_ref='' RETURNING id""", (ref, count, job["id"], job["attempt_token"])).fetchone()
        if updated is None:
            raise RuntimeError("mutation preparation lost ownership")
    return ref, count
