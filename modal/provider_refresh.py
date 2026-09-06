"""Replace unusable temporary inputs before a provider batch can be submitted."""

from contextlib import closing
from pathlib import Path
import tempfile
import time

from extraction import file_family
from file_runtime import database
from media_prepare import media_clip_seconds, media_inputs
from provider_submission import MAX_BATCH_REQUESTS
from provider_cleanup import record_provider_files
from source_io import materialize_source, read_manifest
from visual_prepare import visual_inputs


def upload_prepared(client, item, key, extraction_id, *, connect=database):
    uploaded = client.files.upload(file=item["path"], config={
        "mime_type": item["mime_type"], "display_name": key,
    })
    record_provider_files([(uploaded.name, extraction_id, uploaded.expiration_time)], connect=connect)
    deadline = time.monotonic() + 30
    while uploaded.state and uploaded.state.name == "PROCESSING":
        if time.monotonic() >= deadline:
            raise TimeoutError("provider input still processing")
        time.sleep(1)
        uploaded = client.files.get(name=uploaded.name)
    if uploaded.state and uploaded.state.name != "ACTIVE":
        raise ValueError("provider input preprocessing failed")
    if not uploaded.name or not uploaded.uri:
        raise ValueError("provider upload returned no file identity")
    return uploaded


def refresh_batch_inputs(batch_id, client, *, path=None, s3=None, bucket=None, connect=database):
    with connect() as conn:
        batch = conn.execute("SELECT * FROM provider_batches WHERE id=%s", (batch_id,)).fetchone()
        if (batch is None or batch["status"] != "preparing" or batch["provider_job_id"]
                or batch["submission_started_at"] is not None):
            return
        requests = conn.execute("SELECT * FROM provider_requests WHERE batch_id=%s ORDER BY ordinal", (batch_id,)).fetchall()
        source = conn.execute("""SELECT v.source_manifest_ref,v.content_hash,v.size_bytes,
            f.root_id,r.org_id,f.path,f.deleted,r.deleting_at,e.version_id,e.revision,f.captured_version_id FROM provider_requests p
            JOIN file_extractions e ON e.id=p.extraction_id JOIN file_versions v ON v.id=e.version_id
            JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
            WHERE p.batch_id=%s LIMIT 1""", (batch_id,)).fetchone()
    if not 1 <= len(requests) <= MAX_BATCH_REQUESTS:
        raise ValueError("invalid persisted provider batch size")
    if source is None:
        raise RuntimeError("provider input source is no longer available")
    stale = source["deleted"] or source["deleting_at"] or source["captured_version_id"] != source["version_id"]
    if batch["retry_of"] and stale:
        return  # submit_batch owns the durable superseded transition.
    if source["deleted"] or source["deleting_at"]:
        raise RuntimeError("provider input source is no longer available")
    missing = {}
    for request in requests:
        if request["status"] == "complete":
            continue
        try:
            remote = client.files.get(name=request["input_file_id"])
        except Exception as error:
            if getattr(error, "code", None) not in {403, 404}:
                raise  # Unauthenticated, throttled and transport failures defer.
            # Gemini also returns 403 for missing files. This does not prove
            # deletion: it only means this input cannot be reused. Regeneration
            # reads our authorized immutable source; the replacement upload
            # must still succeed using the same provider credentials. Cleanup
            # must never interpret this permission response as erasure.
            missing[request["ordinal"]] = request
            continue
        if remote.state and remote.state.name == "FAILED":
            missing[request["ordinal"]] = request
        elif not remote.state or remote.state.name != "ACTIVE":
            raise RuntimeError("provider input is not ready")
    if not missing:
        return
    with tempfile.TemporaryDirectory(prefix="pufferfs-refresh-") as directory:
        if path is None:
            if s3 is None or not bucket:
                raise ValueError("durable source storage required to refresh provider inputs")
            path = str(Path(directory) / ("source" + Path(source["path"]).suffix.lower()))
            manifest = read_manifest(s3, bucket, source)
            materialize_source(s3, bucket, manifest, path)
        inputs = (media_inputs(path, clip_seconds=media_clip_seconds(source["revision"]), ordinals=set(missing))
                  if file_family(path) in {"audio", "video"} else visual_inputs(path, ordinals=set(missing)))
        with closing(inputs):
            for item in inputs:
                ordinal = item["ordinal"]
                request = missing.get(ordinal)
                if request is None:
                    continue
                if item["location"] != request["location"] or item["mime_type"] != request["mime_type"]:
                    raise ValueError("regenerated provider input changed its source location")
                uploaded = upload_prepared(client, item, request["request_key"], request["extraction_id"], connect=connect)
                with connect() as conn:
                    current = conn.execute("SELECT * FROM provider_batches WHERE id=%s FOR UPDATE", (batch_id,)).fetchone()
                    if (current["status"] != "preparing" or current["submission_started_at"] is not None
                            or current["provider_job_id"]):
                        raise RuntimeError("provider submission started during input refresh")
                    updated = conn.execute("""UPDATE provider_requests SET input_file_id=%s,input_uri=%s
                        WHERE request_key=%s AND batch_id=%s AND status='pending' AND input_file_id=%s
                        RETURNING request_key""", (uploaded.name, uploaded.uri, request["request_key"],
                                                   batch_id, request["input_file_id"])).fetchone()
                    if updated is None:
                        raise RuntimeError("provider input changed during refresh")
                    conn.execute("UPDATE provider_batches SET input_file_id=NULL,updated_at=NOW() WHERE id=%s", (batch_id,))
                del missing[ordinal]
                if not missing:
                    break
    if missing:
        raise ValueError("regenerated source has fewer pages or clips than its request mapping")
