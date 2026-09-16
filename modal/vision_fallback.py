"""Recover failed image requests using an authenticated Modal vision endpoint."""

import base64
from collections import deque
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
import json
import os
from pathlib import Path
import tempfile
import time
from urllib.parse import urlsplit

import httpx

from extraction import text_chunks
from file_runtime import database
from gemini_contract import VISUAL_PROMPT
from provider_refresh import MAX_MEDIA_BYTES, prepared_inputs
from source_io import materialize_source, read_manifest

MODEL = "deepseek-ai/DeepSeek-V4.1-Flash"


def transcribe(client, url, token, model, data):
    payload = {"model": model, "max_tokens": 16384, "temperature": 0, "reasoning_effort": "none",
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": VISUAL_PROMPT},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64," + base64.b64encode(data).decode("ascii")}},
        ]}]}
    with client.stream("POST", url, headers={"Authorization": "Bearer " + token}, json=payload) as response:
        response.raise_for_status()
        raw = bytearray()
        for block in response.iter_bytes():
            raw.extend(block)
            if len(raw) > 1024 * 1024:
                raise ValueError("vision response exceeds 1 MiB")
    choice = json.loads(raw)["choices"][0]
    content = choice["message"]["content"]
    if choice["finish_reason"] != "stop" or not isinstance(content, str):
        raise ValueError("vision response missing, blocked or truncated")
    return content


def fallback_results(batch, requests, s3, bucket, *, connect=database):
    base = os.getenv("PUFFERFS_VISION_BASE_URL", "").strip()
    missing = {r["ordinal"]: r for r in requests if r["status"] == "failed" and r["mime_type"] == "image/png"}
    if not base or not missing:
        return
    endpoint = urlsplit(base)
    if endpoint.scheme != "https" or not endpoint.hostname or endpoint.username or endpoint.password or endpoint.query or endpoint.fragment:
        raise ValueError("vision endpoint requires an HTTPS base URL")
    token = os.environ["MODAL_PROXY_TOKEN"]
    model = os.getenv("PUFFERFS_VISION_MODEL", MODEL).strip() or MODEL
    with connect() as conn:
        source = conn.execute("""SELECT v.source_manifest_ref,v.content_hash,v.size_bytes,
            f.root_id,r.org_id,f.path,e.revision FROM file_extractions e
            JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
            JOIN roots r ON r.id=f.root_id WHERE e.id=%s AND NOT f.deleted AND r.deleting_at IS NULL
              AND f.captured_version_id=e.version_id AND e.status NOT IN ('failed','superseded')""",
            (batch["extraction_id"],)).fetchone()
    if source is None:
        raise RuntimeError("vision fallback source is no longer current")
    with tempfile.TemporaryDirectory(prefix="pufferfs-vision-") as directory:
        path = str(Path(directory) / ("source" + Path(source["path"]).suffix.lower()))
        materialize_source(s3, bucket, read_manifest(s3, bucket, source), path)
        # Four live calls bound memory and concurrency. Stop starting calls
        # after five minutes, leaving room for source rendering and publication
        # within the collector's 900-second timeout. Remaining failures use the
        # ordinary durable retry budget; successful siblings are never redone.
        deadline, pending = time.monotonic() + 300, deque()
        with httpx.Client(timeout=60, follow_redirects=False) as client, ThreadPoolExecutor(max_workers=4) as pool, \
                closing(prepared_inputs(path, source["revision"], set(missing))) as inputs:
            def completed():
                request, future = pending.popleft()
                try:
                    text = future.result()
                    chunks = list(text_chunks([text.encode()]))
                except (httpx.HTTPError, ValueError, KeyError, TypeError, IndexError) as error:
                    request["error"] = "vision fallback: " + type(error).__name__
                    return None
                for part, chunk in enumerate(chunks):
                    chunk["location"] = dict(request["location"], part=part)
                request.update(result_provider="modal", result_model=model)
                return request, chunks

            for item in inputs:
                if len(pending) == 4:
                    result = completed()
                    if result is not None:
                        yield result
                if time.monotonic() >= deadline:
                    break
                request = missing[item["ordinal"]]
                if item["location"] != request["location"] or item["mime_type"] != request["mime_type"]:
                    raise ValueError("vision fallback input identity changed")
                with open(item["path"], "rb") as image:
                    data = image.read(MAX_MEDIA_BYTES + 1)
                if not data or len(data) > MAX_MEDIA_BYTES:
                    raise ValueError("vision input is empty or exceeds 20 MiB")
                pending.append((request, pool.submit(transcribe, client, base.rstrip("/") + "/chat/completions", token, model, data)))
            while pending:
                result = completed()
                if result is not None:
                    yield result
