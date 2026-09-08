"""Test-only network boundary: real provider requests, optionally delayed in flight.

No application imports, fake provider successes or database access. Bodies and
credentials are forwarded unchanged and never logged. Control records contain
only namespace/publication identities, byte counts, wire hashes and transport progress.
"""

import asyncio
from collections import deque
from contextlib import asynccontextmanager
import hashlib
import gzip
import io
import json
import os
import re
import uuid
from urllib.parse import urlsplit

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import Response
import httpx
import uvicorn

UPSTREAM = os.environ["E2E_TURBOPUFFER_UPSTREAM"].rstrip("/")
parsed = urlsplit(UPSTREAM)
if (parsed.scheme != "https" or not parsed.hostname.endswith(".turbopuffer.com")
        or parsed.port not in {None, 443} or parsed.path or parsed.query or parsed.fragment
        or parsed.username or parsed.password):
    raise ValueError("relay requires a fixed real Turbopuffer HTTPS origin")

events = deque(maxlen=64)
fault = None
HOP_HEADERS = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
               "te", "trailer", "transfer-encoding", "upgrade", "host", "content-length"}


@asynccontextmanager
async def lifetime(app):
    async with httpx.AsyncClient(timeout=120, trust_env=False) as client:
        app.state.client = client
        yield


app = FastAPI(lifespan=lifetime)


def control(request):
    if request.headers.get("X-E2E-Control") != "e2e-index-fault-only":
        raise HTTPException(401)


@app.get("/healthz")
async def health():
    return {"role": "test-index-relay"}


@app.get("/status")
async def status(request: Request):
    control(request)
    return {"events": list(events)}


@app.post("/fault")
async def arm(request: Request):
    global fault
    control(request)
    body = await request.body()
    if len(body) > 65536:
        raise HTTPException(413)
    config = json.loads(body)
    names = config.get("namespaces")
    if (config.get("operation", "write") not in {"write", "query"}
            or config.get("mode") not in {"hold_request", "hold_response"}
            or type(config.get("skip", 0)) is not int or not 0 <= config.get("skip", 0) <= 100
            or type(config.get("count", 1)) is not int or not 1 <= config.get("count", 1) <= 64
            or not isinstance(names, list) or not 1 <= len(names) <= 256
            or any(not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", name) for name in names)):
        raise HTTPException(400)
    if fault is not None and not fault["gate"].is_set():
        raise HTTPException(409, "release the previous fault first")
    fault = {"id": uuid.uuid4().hex, "namespaces": set(names), "mode": config["mode"],
             "operation": config.get("operation", "write"), "skip": config.get("skip", 0),
             "remaining": config.get("count", 1), "gate": asyncio.Event()}
    return {"fault_id": fault["id"]}


@app.post("/release")
async def release(request: Request):
    control(request)
    if fault is not None:
        fault["gate"].set()
    return {"released": True}


@app.post("/v2/namespaces/{namespace}/query")
@app.post("/v2/namespaces/{namespace}")
async def write(namespace: str, request: Request):
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", namespace):
        raise HTTPException(400)
    body = bytearray()
    async for block in request.stream():
        body.extend(block)
        if len(body) > 16 << 20:
            raise HTTPException(413)
    operation = "query" if request.url.path.endswith("/query") else "write"
    selected = None
    if fault is not None and fault["remaining"] and namespace in fault["namespaces"] and operation == fault["operation"]:
        if fault["skip"]:
            fault["skip"] -= 1
        else:
            selected = fault
            selected["remaining"] -= 1
    encoding = request.headers.get("content-encoding", "identity").lower()
    if encoding == "gzip":
        with gzip.GzipFile(fileobj=io.BytesIO(body)) as compressed:
            payload = compressed.read((16 << 20) + 1)
        if len(payload) > 16 << 20:
            raise HTTPException(413)
    elif encoding == "identity":
        payload = body
    else:
        raise HTTPException(415, "unsupported index request compression")
    event = {"id": uuid.uuid4().hex, "namespace": namespace,
             "operation": operation,
             "wire_sha256": hashlib.sha256(body).hexdigest(),
             "payload_sha256": hashlib.sha256(payload).hexdigest(), "encoding": encoding,
             "gzip_mtime": int.from_bytes(body[4:8], "little") if encoding == "gzip" else None,
             "state": "received",
             "fault_id": selected["id"] if selected else None}
    if operation == "query":
        def publications(value):
            if not isinstance(value, list):
                return []
            if len(value) == 3 and value[:2] == ["extraction_id", "Eq"]:
                return [value[2]]
            return [item for child in value for item in publications(child)]
        event["publication_ids"] = publications(json.loads(payload).get("filters"))
    events.append(event)
    del payload
    try:
        if selected and selected["mode"] == "hold_request":
            event["state"] = "request_held"
            await asyncio.wait_for(selected["gate"].wait(), timeout=1200)
        # The full request has reached this relay. Releasing it still forwards
        # those bytes if its caller has since crashed, modeling an in-flight
        # write that cancellation cannot retract from an intermediate service.
        headers = {key: value for key, value in request.headers.items() if key.lower() not in HOP_HEADERS}
        response = await app.state.client.post(UPSTREAM + request.url.path,
                                               content=bytes(body), headers=headers)
        event["upstream_status"] = response.status_code
        if operation == "query" and response.status_code == 200:
            rows = response.json().get("rows", [])
            event["response_rows"] = len(rows)
            event["response_content_bytes"] = sum(len(row.get("content", "").encode()) for row in rows)
        if selected and selected["mode"] == "hold_response":
            event["state"] = "response_held"
            await asyncio.wait_for(selected["gate"].wait(), timeout=1200)
        event["state"] = "response_released"
        # httpx decodes response compression; retain provider retry and request
        # headers, but let the downstream server compute framing for these bytes.
        headers = {key: value for key, value in response.headers.items()
                   if key.lower() not in HOP_HEADERS | {"content-encoding"}}
        return Response(response.content, status_code=response.status_code, headers=headers)
    except Exception as error:
        event["state"] = "transport_failed"
        event["error_type"] = type(error).__name__
        raise HTTPException(502, "index relay transport failed") from None


@app.api_route("/v2/namespaces/{namespace}", methods=["GET", "DELETE"])
async def namespace_request(namespace: str, request: Request):
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", namespace):
        raise HTTPException(400)
    headers = {key: value for key, value in request.headers.items() if key.lower() not in HOP_HEADERS}
    response = await app.state.client.request(request.method, UPSTREAM + request.url.path, headers=headers)
    headers = {key: value for key, value in response.headers.items()
               if key.lower() not in HOP_HEADERS | {"content-encoding"}}
    return Response(response.content, status_code=response.status_code, headers=headers)


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8080, access_log=False)
