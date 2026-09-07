"""Collect provider output into packed text artifacts and per-file index handoffs."""

import io
import json
import sys
from collections import defaultdict
from contextlib import closing

from file_runtime import database, work_id
from provider_manifests import read_manifest, write_manifest
from provider_retry import MAX_REQUEST_ATTEMPTS
from gemini_contract import result_chunks
from source_io import iter_chunks, write_chunks

ASSEMBLY_MEMORY_BYTES = 64 * 1024 * 1024


def json_memory_size(value):
    """Conservative decoded JSON size, without reserializing retained chunks."""
    size = sys.getsizeof(value)
    if isinstance(value, dict):
        size += sum(json_memory_size(k) + json_memory_size(v) for k, v in value.items())
    elif isinstance(value, list):
        size += sum(json_memory_size(item) for item in value)
    return size


def collect_batch(batch, client, s3, bucket, *, connect=database):
    if batch["status"] != "submitted" or not batch["provider_job_id"]:
        return
    remote = client.batches.get(name=batch["provider_job_id"])
    if remote.state.name not in {"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED"}:
        return
    manifest = read_manifest(s3, bucket, batch, batch["input_ref"], "input")
    if manifest["attempt"] != batch["attempt_count"]:
        raise ValueError("provider result attempt mismatch")
    requests = manifest["requests"]
    expected = {r["request_key"] for r in requests if r["status"] != "complete"}
    outputs = {}
    if remote.dest and remote.dest.file_name:
        # Provider-generated results are not deletable Files API uploads.
        raw = client.files.download(file=remote.dest.file_name)
        if len(raw) > ASSEMBLY_MEMORY_BYTES:
            raise ValueError("provider batch output exceeds 64 MiB")
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
    successes, failures = set(), set()
    prefix = f"extractions/{batch['org_id']}/{batch['root_id']}/{batch['extraction_id']}/provider/{batch['id']}"

    def records():
        for request in requests:
            if request["status"] == "complete":
                continue
            key = request["request_key"]
            item = outputs.pop(key, {})
            try:
                if not item.get("response") or item.get("error"):
                    raise ValueError("provider request failed or missing")
                chunks = list(result_chunks(item["response"], request["location"], key))
            except (ValueError, KeyError, TypeError) as error:
                request.update(status="failed", error=type(error).__name__)
                failures.add(key)
                continue
            successes.add(key)
            for chunk in chunks:
                yield dict(chunk, request_key=key)

    artifact, _ = write_chunks(s3, bucket, prefix, records())
    for request in requests:
        if request["request_key"] in successes:
            request.update(status="complete", result_ref=artifact, error="")
    output_ref = write_manifest(s3, bucket, batch, "result", {
        "attempt": batch["attempt_count"], "requests": requests, "input_ref": batch["input_ref"],
        "provider_job_id": batch["provider_job_id"], "previous": batch["output_ref"],
    })
    status = ("retry" if batch["attempt_count"] < MAX_REQUEST_ATTEMPTS else "failed") if failures else "complete"
    with connect() as conn:
        updated = conn.execute("""UPDATE provider_batches SET status=%s,output_ref=%s,updated_at=NOW()
            WHERE id=%s AND status='submitted' AND lease_token=%s AND lease_until>NOW()
              AND attempt_count=%s AND input_ref=%s RETURNING *""",
            (status, output_ref, batch["id"], batch["lease_token"], batch["attempt_count"], batch["input_ref"])).fetchone()
        if updated is None:
            raise RuntimeError("provider result publication lost ownership")
    batch.update(updated)


def assemble_extraction(extraction_id, s3, bucket, token, *, connect=database):
    with connect() as conn:
        extraction = conn.execute("""SELECT e.*,f.root_id,r.org_id,r.deleting_at,
            f.captured_version_id,f.deleted FROM file_extractions e
            JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
            JOIN roots r ON r.id=f.root_id WHERE e.id=%s""", (extraction_id,)).fetchone()
        if extraction is None or extraction["status"] != "waiting_provider":
            return False
        stale = (extraction["deleting_at"] or extraction["deleted"]
                 or extraction["captured_version_id"] != extraction["version_id"])
        summary = conn.execute("""SELECT SUM(request_count) AS count,MIN(ordinal_start) AS first,
            MAX(ordinal_start+request_count) AS last,BOOL_AND(status='complete' AND output_ref<>'') AS ready,
            BOOL_OR(status='failed') AS failed FROM provider_batches WHERE extraction_id=%s""",
            (extraction_id,)).fetchone()
        if stale or summary["failed"]:
            status = "superseded" if stale else "failed"
            conn.execute("UPDATE file_extractions SET status=%s,updated_at=NOW() WHERE id=%s AND status='waiting_provider'",
                         (status, extraction_id))
            conn.execute("""UPDATE file_work SET status=%s,lease_until=NULL,updated_at=NOW()
                WHERE extraction_id=%s AND stage='transform' AND status='waiting_provider'""", (status, extraction_id))
            return False
    expected = extraction["prepared_request_count"]
    if (type(expected) is not int or expected < 1 or summary["count"] != expected
            or summary["first"] != 0 or summary["last"] != expected or not summary["ready"]):
        return False

    def chunks():
        ordinal, start = 0, 0
        while start < expected:
            with connect() as conn:
                batches = conn.execute("""SELECT * FROM provider_batches WHERE extraction_id=%s
                    AND ordinal_start>=%s ORDER BY ordinal_start LIMIT 16""", (extraction_id, start)).fetchall()
            if not batches:
                raise RuntimeError("provider result ranges are missing")
            for batch in batches:
                if batch["ordinal_start"] != start or batch["status"] != "complete":
                    raise RuntimeError("provider result ranges changed during assembly")
                manifest = read_manifest(s3, bucket, batch, batch["output_ref"], "result")
                requests = manifest["requests"]
                if any(r["status"] != "complete" for r in requests):
                    raise ValueError("incomplete provider result manifest")
                by_ref, selected = defaultdict(set), {}
                for request in requests:
                    by_ref[request["result_ref"]].add(request["request_key"])
                    selected[request["request_key"]] = []
                retained = 0
                for ref, keys in by_ref.items():
                    with closing(iter_chunks(s3, bucket, ref)) as records:
                        for chunk in records:
                            key = chunk.pop("request_key")
                            if key not in keys:
                                continue
                            retained += json_memory_size(chunk) + sys.getsizeof(None)
                            if retained > ASSEMBLY_MEMORY_BYTES:
                                raise ValueError("provider assembly window exceeds 64 MiB")
                            selected[key].append(chunk)
                for request in requests:
                    for chunk in selected.pop(request["request_key"]):
                        chunk["chunk_index"] = ordinal
                        ordinal += 1
                        yield chunk
                start += batch["request_count"]

    prefix = f"extractions/{extraction['org_id']}/{extraction['root_id']}/{extraction_id}/chunks"
    artifact, count = write_chunks(s3, bucket, prefix, chunks())
    with connect() as conn:
        # Lock root against deletion only for this short publication transaction.
        root = conn.execute("SELECT deleting_at FROM roots WHERE id=%s FOR UPDATE", (extraction["root_id"],)).fetchone()
        if root is None or root["deleting_at"]:
            return False
        owner = conn.execute("""SELECT id FROM file_work WHERE extraction_id=%s AND stage='transform'
            AND status='waiting_provider' AND attempt_token=%s AND lease_until>NOW() FOR UPDATE""",
            (extraction_id, token)).fetchone()
        if owner is None:
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
