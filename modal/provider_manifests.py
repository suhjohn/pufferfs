"""Bounded, immutable provider metadata. Postgres stores only these pointers."""

import hashlib
import json
import re
from datetime import datetime, timedelta, timezone

from file_runtime import stable_id
from gemini_contract import batch_request

MAX_BATCH_REQUESTS = 64
MAX_MANIFEST_BYTES = 4 * 1024 * 1024


def manifest_prefix(batch):
    if not re.fullmatch(r"[0-9a-f]{64}", batch["id"]):
        raise ValueError("invalid provider batch identity")
    # Outside root-erasure prefixes: cleanup identities outlive catalog rows.
    return f"maintenance/provider/{batch['id']}/"


def write_manifest(s3, bucket, batch, kind, value):
    if kind not in {"input", "result", "cleanup"}:
        raise ValueError("invalid provider manifest kind")
    document = dict(value, format=1, kind=kind, batch_id=batch["id"])
    raw = json.dumps(document, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()
    if len(raw) > MAX_MANIFEST_BYTES:
        raise ValueError("provider manifest exceeds 4 MiB")
    key = f"{manifest_prefix(batch)}{kind}/{hashlib.sha256(raw).hexdigest()}.json"
    s3.put_object(Bucket=bucket, Key=key, Body=raw, ContentType="application/json")
    return key


def read_manifest(s3, bucket, batch, ref, kind):
    match = re.fullmatch(re.escape(manifest_prefix(batch) + kind + "/") + r"([0-9a-f]{64})\.json", ref)
    if match is None:
        raise ValueError("provider manifest is outside its batch")
    response = s3.get_object(Bucket=bucket, Key=ref)
    with response["Body"] as body:
        raw = body.read(MAX_MANIFEST_BYTES + 1)
    if len(raw) > MAX_MANIFEST_BYTES or hashlib.sha256(raw).hexdigest() != match[1]:
        raise ValueError("provider manifest size or checksum mismatch")
    value = json.loads(raw)
    if (value.get("format") != 1 or value.get("kind") != kind or value.get("batch_id") != batch["id"]):
        raise ValueError("provider manifest identity mismatch")
    if kind in {"input", "result"}:
        validate_requests(batch, value["requests"])
        if type(value.get("attempt")) is not int or not 1 <= value["attempt"] <= 3:
            raise ValueError("invalid provider manifest attempt")
    if kind == "input":
        if not 1 <= len(value["uploads"]) <= MAX_BATCH_REQUESTS + 1:
            raise ValueError("invalid provider upload manifest size")
        for upload in value["uploads"]:
            if not re.fullmatch(r"files/[A-Za-z0-9_-]+", upload["file_id"]):
                raise ValueError("invalid provider upload identity")
            expiry = datetime.fromisoformat(upload["expires_at"])
            if expiry.utcoffset() is None:
                raise ValueError("provider upload expiry requires timezone")
    return value


def validate_requests(batch, requests):
    if len(requests) != batch["request_count"] or not 1 <= len(requests) <= MAX_BATCH_REQUESTS:
        raise ValueError("provider manifest range length mismatch")
    for i, request in enumerate(requests):
        ordinal = batch["ordinal_start"] + i
        if (type(request["ordinal"]) is not int or request["ordinal"] != ordinal
                or request["request_key"] != stable_id(batch["extraction_id"], str(ordinal))
                or request["status"] not in {"pending", "complete", "failed"}):
            raise ValueError("provider manifest request identity mismatch")
        batch_request(request["request_key"], request["mime_type"], request["input_uri"], request["location"])
        if request["status"] == "complete":
            prefix = f"extractions/{batch['org_id']}/{batch['root_id']}/{batch['extraction_id']}/provider/{batch['id']}/"
            if not re.fullmatch(re.escape(prefix) + r"[0-9a-f]{64}\.jsonl\.gz", request["result_ref"]):
                raise ValueError("provider result is outside its source batch")


def upload_record(uploaded):
    if not uploaded.name or not re.fullmatch(r"files/[A-Za-z0-9_-]+", uploaded.name):
        raise ValueError("provider upload returned no file identity")
    expiry = uploaded.expiration_time or datetime.now(timezone.utc) + timedelta(hours=48)
    if expiry.utcoffset() is None:
        raise ValueError("provider upload expiry requires timezone")
    return {"file_id": uploaded.name, "expires_at": expiry.isoformat()}
