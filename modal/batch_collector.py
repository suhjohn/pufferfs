"""Collect provider output into packed text artifacts and per-file index handoffs."""

import io
import json
import sys
from collections import defaultdict
from contextlib import closing

from file_runtime import database, work_id
from gemini_contract import result_chunks
from source_io import iter_chunks, write_chunks

ASSEMBLY_REQUESTS = 64
ASSEMBLY_MEMORY_BYTES = 64 * 1024 * 1024


def json_memory_size(value):
    """Conservative decoded JSON size, without reserializing retained chunks."""
    size = sys.getsizeof(value)
    if isinstance(value, dict):
        size += sum(json_memory_size(k) + json_memory_size(v) for k, v in value.items())
    elif isinstance(value, list):
        size += sum(json_memory_size(item) for item in value)
    return size


def collect_batch(batch_id, client, s3, bucket, *, connect=database):
    with connect() as conn:
        batch = conn.execute("SELECT * FROM provider_batches WHERE id=%s", (batch_id,)).fetchone()
        if batch is None or batch["status"] in {"complete", "failed"}:
            return
        if not batch["provider_job_id"]:
            return
        requests = conn.execute(
            """SELECT p.*,f.root_id,r.org_id FROM provider_requests p
               JOIN file_extractions e ON e.id=p.extraction_id JOIN file_versions v ON v.id=e.version_id
               JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
               WHERE p.batch_id=%s ORDER BY p.ordinal""", (batch_id,),
        ).fetchall()
    if not requests:
        return
    remote = client.batches.get(name=batch["provider_job_id"])
    state = remote.state.name
    if state not in {"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED"}:
        return
    outputs = {}
    if remote.dest and remote.dest.file_name:
        # Generated batch results are not uploaded Files API resources. Google
        # retains them for six weeks; deleting the batch does not erase the
        # downloadable result. Never register them as deletable uploads.
        # Submission caps batches at 64 requests and each request's output
        # tokens. The SDK downloads bytes; additionally reject oversized data.
        raw = client.files.download(file=remote.dest.file_name)
        if len(raw) > 64 * 1024 * 1024:
            raise ValueError("provider batch output exceeds 64 MiB")
        expected = {request["request_key"] for request in requests}
        for line in io.BytesIO(raw):
            if len(line) > 1024 * 1024:
                raise ValueError("provider result exceeds 1 MiB")
            if not line.strip():
                continue
            item = json.loads(line)
            key = item.get("key")
            if key not in expected or key in outputs:
                raise ValueError("unknown or duplicate provider result identity")
            outputs[key] = item
    successes, failures = [], []
    first = requests[0]
    prefix = f"extractions/{first['org_id']}/{first['root_id']}/{first['extraction_id']}/provider/{batch_id}"

    def records():
        for request in requests:
            key = request["request_key"]
            if request["status"] == "complete":
                continue
            item = outputs.get(key, {})
            try:
                if not item.get("response") or item.get("error"):
                    raise ValueError("provider request failed or missing")
                # Validate the entire page/clip before publishing any of it.
                chunks = list(result_chunks(item["response"], request["location"], key))
            except (ValueError, KeyError, TypeError) as error:
                failures.append((key, type(error).__name__))
                continue
            successes.append(key)
            for chunk in chunks:
                yield dict(chunk, request_key=key)

    artifact, _ = write_chunks(s3, bucket, prefix, records())
    with connect() as conn:
        for key in successes:
            conn.execute("""UPDATE provider_requests SET status='complete',result_ref=%s,error=''
                            WHERE request_key=%s AND batch_id=%s AND status<>'complete'""", (artifact, key, batch_id))
        for key, error in failures:
            conn.execute("""UPDATE provider_requests SET status='failed',error=%s
                            WHERE request_key=%s AND batch_id=%s AND status<>'complete'""", (error, key, batch_id))
        conn.execute("""UPDATE provider_batches SET status=%s,output_ref=%s,updated_at=NOW()
                        WHERE id=%s AND status='submitted'""", ("failed" if failures else "complete", artifact, batch_id))


def assemble_extraction(extraction_id, s3, bucket, *, connect=database):
    with connect() as conn:
        extraction = conn.execute(
            """SELECT e.*,f.root_id,r.org_id,r.deleting_at FROM file_extractions e
               JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
               JOIN roots r ON r.id=f.root_id WHERE e.id=%s""", (extraction_id,),
        ).fetchone()
        if extraction is None or extraction["status"] != "waiting_provider" or extraction["deleting_at"]:
            return False
        summary = conn.execute("""SELECT COUNT(*) AS count,MIN(ordinal) AS first,MAX(ordinal) AS last,
            BOOL_AND(status='complete' AND result_ref<>'') AS ready
            FROM provider_requests WHERE extraction_id=%s""", (extraction_id,)).fetchone()
    expected = extraction["prepared_request_count"]
    if (type(expected) is not int or expected < 1 or summary["count"] != expected
            or summary["first"] != 0 or summary["last"] != expected - 1 or not summary["ready"]):
        return False

    def chunks():
        ordinal = 0
        for start in range(0, expected, ASSEMBLY_REQUESTS):
            size = min(ASSEMBLY_REQUESTS, expected - start)
            with connect() as conn:
                requests = conn.execute("""SELECT request_key,ordinal,status,result_ref
                    FROM provider_requests WHERE extraction_id=%s AND ordinal>=%s
                    ORDER BY ordinal LIMIT %s""", (extraction_id, start, size)).fetchall()
            if (len(requests) != size or any(r["ordinal"] != start + i or r["status"] != "complete"
                    or not r["result_ref"] for i, r in enumerate(requests))):
                raise RuntimeError("provider request manifest changed during assembly")
            by_ref, selected = defaultdict(set), {}
            for request in requests:
                key = request["request_key"]
                by_ref[request["result_ref"]].add(key)
                selected[key] = []
            retained = 0
            # Retries interleave result references in source order. Reading by
            # reference first prevents one download/decode per alternating page.
            # Only this bounded window is retained; no document-sized cache.
            for ref, keys in by_ref.items():
                with closing(iter_chunks(s3, bucket, ref)) as records:
                    for chunk in records:
                        key = chunk.pop("request_key")
                        if key not in keys:
                            continue
                        retained += json_memory_size(chunk) + sys.getsizeof(None)  # list slot allowance
                        if retained > ASSEMBLY_MEMORY_BYTES:
                            raise ValueError("provider assembly window exceeds 64 MiB")
                        selected[key].append(chunk)
            for request in requests:
                for chunk in selected.pop(request["request_key"]):
                    chunk["chunk_index"] = ordinal
                    ordinal += 1
                    yield chunk

    prefix = f"extractions/{extraction['org_id']}/{extraction['root_id']}/{extraction_id}/chunks"
    artifact, count = write_chunks(s3, bucket, prefix, chunks())
    with connect() as conn:
        # Lock root against deletion only for this short publication transaction.
        root = conn.execute("SELECT deleting_at FROM roots WHERE id=%s FOR UPDATE", (extraction["root_id"],)).fetchone()
        if root is None or root["deleting_at"]:
            return False
        updated = conn.execute("""UPDATE file_extractions SET status='complete',chunks_ref=%s,
                                  chunk_count=%s,updated_at=NOW() WHERE id=%s AND status='waiting_provider'
                                  RETURNING id""", (artifact, count, extraction_id)).fetchone()
        if updated is None:
            return False
        conn.execute("""UPDATE file_work SET status='complete',updated_at=NOW()
                        WHERE extraction_id=%s AND stage='transform' AND status='waiting_provider'""", (extraction_id,))
        conn.execute("""INSERT INTO file_work(id,extraction_id,stage) VALUES(%s,%s,'index')
                        ON CONFLICT(extraction_id,stage) DO NOTHING""", (work_id(extraction_id, "index"), extraction_id))
    return True
