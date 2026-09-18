"""Stream inline media into one bounded JSONL upload per batch attempt."""

from contextlib import closing, contextmanager
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import tempfile

from extraction import file_family
from file_runtime import database, stable_id
from gemini_contract import batch_request
from media_prepare import media_clip_seconds, media_inputs
from provider_manifests import read_manifest as read_provider_manifest, upload_record, validate_requests, write_manifest
from source_io import materialize_source, read_manifest
from visual_prepare import visual_inputs

# A rendered RGB PNG is at most 2400 x 2400; a mono 60s WAV is under 2 MiB.
# Include base64 expansion in the JSONL bound, below Gemini's 2 GB file limit.
MAX_MEDIA_BYTES = 20 * 1024 * 1024
MAX_BATCH_BYTES = 1900 * 1024 * 1024


def prepared_inputs(path, revision, ordinals):
    return (media_inputs(path, clip_seconds=media_clip_seconds(revision), ordinals=ordinals)
            if file_family(path) in {"audio", "video"} else visual_inputs(path, ordinals=ordinals))


@contextmanager
def prepare_inputs(inputs, extraction_id, attempt):
    requests = []
    with tempfile.TemporaryDirectory(prefix="pufferfs-envelope-") as directory:
        path = Path(directory) / "requests.jsonl"
        with path.open("wb") as output:
            for item in inputs:
                if len(requests) >= 64:
                    raise ValueError("provider batch exceeds 64 inputs")
                key = stable_id(extraction_id, str(item["ordinal"]))
                with open(item["path"], "rb") as source:
                    data = source.read(MAX_MEDIA_BYTES + 1)
                if len(data) > MAX_MEDIA_BYTES:
                    raise ValueError("prepared media exceeds 20 MiB")
                row = batch_request(key, item["mime_type"], data, item["location"])
                raw = (json.dumps(row, separators=(",", ":")) + "\n").encode()
                if output.tell() + len(raw) > MAX_BATCH_BYTES:
                    raise ValueError("provider JSONL exceeds 1900 MiB")
                output.write(raw)
                request = {name: item[name] for name in ("ordinal", "location", "mime_type")}
                request.update(request_key=key, status="pending", result_ref="", error="", attempt_count=attempt)
                requests.append(request)
                del data, row, raw
        yield requests, path


def persist_inputs(batch, requests, path, client, s3, bucket, *, previous=""):
    validate_requests(batch, requests)
    uploaded = client.files.upload(file=str(path), config={"mime_type": "jsonl", "display_name": batch["id"]})
    value = {"attempt": batch["attempt_count"], "requests": requests, "uploads": [upload_record(uploaded)],
             "input_file_id": uploaded.name, "previous": previous}
    return write_manifest(s3, bucket, batch, "input", value)


def refresh_batch_inputs(batch, client, s3, bucket, *, path=None, connect=database):
    if batch["status"] != "preparing" or batch["submission_started_at"] is not None:
        return
    current = read_provider_manifest(s3, bucket, batch, batch["input_ref"], "input")
    # No per-input GET on ordinary resume. A missing remote file is a terminal
    # per-request failure and will be regenerated with the next batch attempt.
    if (current["attempt"] == batch["attempt_count"] and all(
            datetime.fromisoformat(item["expires_at"]) > datetime.now(timezone.utc) + timedelta(minutes=5)
            for item in current["uploads"])):
        return
    previous = (read_provider_manifest(s3, bucket, batch, batch["output_ref"], "result")
                if batch["output_ref"] else current)
    requests = previous["requests"]
    missing = {item["ordinal"]: item for item in requests if item["status"] != "complete"}
    if not missing:
        raise ValueError("provider retry has no incomplete requests")
    with connect() as conn:
        source = conn.execute("""SELECT v.source_manifest_ref,v.content_hash,v.size_bytes,
            f.root_id,r.org_id,f.path,e.revision FROM file_extractions e
            JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
            JOIN roots r ON r.id=f.root_id WHERE e.id=%s AND NOT f.deleted AND r.deleting_at IS NULL
              AND f.captured_version_id=e.version_id""", (batch["extraction_id"],)).fetchone()
    if source is None:
        raise RuntimeError("provider retry source is no longer current")
    with tempfile.TemporaryDirectory(prefix="pufferfs-refresh-") as directory:
        if path is None:
            path = str(Path(directory) / ("source" + Path(source["path"]).suffix.lower()))
            materialize_source(s3, bucket, read_manifest(s3, bucket, source), path)
        with closing(prepared_inputs(path, source["revision"], set(missing))) as inputs, \
                prepare_inputs(inputs, batch["extraction_id"], batch["attempt_count"]) as (replacements, envelope):
            if {item["ordinal"] for item in replacements} != set(missing):
                raise ValueError("provider retry source range changed")
            for item in replacements:
                before = missing[item["ordinal"]]
                if item["location"] != before["location"] or item["mime_type"] != before["mime_type"]:
                    raise ValueError("provider retry input identity changed")
            by_ordinal = {item["ordinal"]: item for item in replacements}
            requests = [by_ordinal.get(item["ordinal"], item) for item in requests]
            ref = persist_inputs(batch, requests, envelope, client, s3, bucket, previous=batch["input_ref"])
    with connect() as conn:
        updated = conn.execute("""UPDATE provider_batches SET input_ref=%s,updated_at=NOW()
            WHERE id=%s AND input_ref=%s AND lease_token=%s AND lease_until>NOW()
              AND attempt_count=%s AND status='preparing' AND submission_started_at IS NULL RETURNING *""",
            (ref, batch["id"], batch["input_ref"], batch["lease_token"], batch["attempt_count"])).fetchone()
        if updated is None:
            raise RuntimeError("provider input publication lost ownership")
    batch.update(updated)
