"""Real Gemini traffic with a bounded, externally controlled submission delay.

No application imports, database access, fake provider results or body edits.
Only batch identities, hashes and transport progress are exposed by control IO.
"""

import asyncio
from collections import deque
from contextlib import asynccontextmanager
import hashlib
import json
import uuid

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import Response
import httpx
import uvicorn

UPSTREAM = "https://generativelanguage.googleapis.com"
HOP_HEADERS = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
               "te", "trailer", "transfer-encoding", "upgrade", "host", "content-length"}
events = deque(maxlen=128)
fault = None


@asynccontextmanager
async def lifetime(app):
    async with httpx.AsyncClient(timeout=120, trust_env=False) as client:
        app.state.client = client
        yield


app = FastAPI(lifespan=lifetime)


def control(request):
    if request.headers.get("X-E2E-Control") != "e2e-provider-fault-only":
        raise HTTPException(401)


@app.get("/healthz")
async def health():
    return {"role": "test-provider-relay"}


@app.get("/status")
async def status(request: Request):
    control(request)
    return {"events": list(events)}


@app.post("/fault")
async def arm(request: Request):
    global fault
    control(request)
    body = await request.body()
    if len(body) > 1024:
        raise HTTPException(413)
    config = json.loads(body)
    if config.get("mode") not in {"hold_request", "hold_response"}:
        raise HTTPException(400)
    if fault is not None and not fault["gate"].is_set():
        raise HTTPException(409, "release the previous fault first")
    fault = {"id": uuid.uuid4().hex, "mode": config["mode"],
             "claimed": False, "gate": asyncio.Event()}
    return {"fault_id": fault["id"]}


@app.post("/release")
async def release(request: Request):
    control(request)
    if fault is not None:
        fault["gate"].set()
    return {"released": True}


@app.api_route("/{path:path}", methods=["GET", "POST", "DELETE"])
async def forward(path: str, request: Request):
    # The SDK's normal upload-resume URLs point directly to Google. Only its
    # control requests and result downloads need to pass this fixed-origin
    # relay. Never permit callers to select another upstream origin.
    if not path.startswith(("v1beta/", "upload/v1beta/", "download/v1beta/")):
        raise HTTPException(404)
    body = bytearray()
    async for block in request.stream():
        body.extend(block)
        if len(body) > 16 << 20:
            raise HTTPException(413)
    selected, event = None, None
    if request.method == "POST" and path.endswith(":batchGenerateContent"):
        batch = json.loads(body)["batch"]
        if fault is not None and not fault["claimed"]:
            selected = fault
            selected["claimed"] = True
        event = {"id": uuid.uuid4().hex, "batch_id": batch.get("display_name", batch.get("displayName")),
                 "wire_sha256": hashlib.sha256(body).hexdigest(), "state": "received",
                 "fault_id": selected["id"] if selected else None}
        events.append(event)
    try:
        if selected and selected["mode"] == "hold_request":
            event["state"] = "request_held"
            await asyncio.wait_for(selected["gate"].wait(), timeout=1200)
        headers = {key: value for key, value in request.headers.items() if key.lower() not in HOP_HEADERS}
        url = UPSTREAM + "/" + path
        if request.url.query:
            url += "?" + request.url.query
        response = await app.state.client.request(request.method, url, content=bytes(body), headers=headers)
        if event is not None:
            event["upstream_status"] = response.status_code
            if response.is_success:
                event["provider_job_id"] = response.json().get("name")
            if selected and selected["mode"] == "hold_response":
                event["state"] = "response_held"
                await asyncio.wait_for(selected["gate"].wait(), timeout=1200)
            event["state"] = "response_released"
        headers = {key: value for key, value in response.headers.items()
                   if key.lower() not in HOP_HEADERS | {"content-encoding"}}
        return Response(response.content, status_code=response.status_code, headers=headers)
    except Exception as error:
        if event is not None:
            event["state"] = "transport_failed"
            event["error_type"] = type(error).__name__
        raise HTTPException(502, "provider relay transport failed") from None


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8080, access_log=False)
