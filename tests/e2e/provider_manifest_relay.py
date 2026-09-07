"""Hold real S3 manifest requests/responses at an external network boundary."""

import asyncio
from collections import deque
from contextlib import asynccontextmanager
import json
import re
from urllib.parse import unquote
import uuid

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import Response
import httpx
import uvicorn

fault = None
counts = {kind: {"puts": 0, "gets": 0} for kind in ("input", "result", "cleanup")}
events = deque(maxlen=64)
HOP = {"connection", "keep-alive", "transfer-encoding", "host", "content-length"}


@asynccontextmanager
async def lifetime(app):
    async with httpx.AsyncClient(timeout=180, trust_env=False) as client:
        app.state.client = client
        yield


app = FastAPI(lifespan=lifetime)


def control(request):
    if request.headers.get("X-E2E-Control") != "e2e-provider-manifest-only":
        raise HTTPException(401)


@app.get("/healthz")
async def health():
    return {"role": "test-provider-manifest-relay"}


@app.get("/status")
async def status(request: Request):
    control(request)
    return {"counts": counts, "events": list(events)}


@app.post("/fault")
async def arm(request: Request):
    global fault
    control(request)
    config = await request.json()
    if config.get("mode") not in {"hold_request", "hold_response"} or config.get("kind") not in counts:
        raise HTTPException(400)
    if fault and not fault["gate"].is_set():
        raise HTTPException(409)
    fault = dict(config, id=uuid.uuid4().hex, claimed=False, gate=asyncio.Event())
    return {"fault_id": fault["id"]}


@app.post("/release")
async def release(request: Request):
    control(request)
    if fault:
        fault["gate"].set()
    return {"released": True}


@app.api_route("/{path:path}", methods=["GET", "HEAD", "PUT", "POST", "DELETE"])
async def forward(path: str, request: Request):
    key = unquote(path).partition("/")[2]
    match = re.fullmatch(r"maintenance/provider/[0-9a-f]{64}/(input|result|cleanup)/[0-9a-f]{64}\.json", key)
    body = await request.body()
    selected, event = None, None
    if match and request.method == "PUT" and fault and not fault["claimed"] and match[1] == fault["kind"]:
        fault["claimed"] = True
        selected = fault
        event = {"fault_id": fault["id"], "key": key, "manifest": json.loads(body), "state": "received"}
        events.append(event)
        if selected["mode"] == "hold_request":
            event["state"] = "request_held"
            await asyncio.wait_for(selected["gate"].wait(), 1200)
    url = "http://aws:4566" + request.url.path
    if request.url.query:
        url += "?" + request.url.query
    headers = {k: v for k, v in request.headers.items() if k.lower() not in HOP}
    response = await app.state.client.request(request.method, url, headers=headers, content=body)
    if match and response.is_success:
        if request.method == "PUT":
            counts[match[1]]["puts"] += 1
        if request.method == "GET":
            counts[match[1]]["gets"] += 1
    if event:
        event["upstream_status"] = response.status_code
        if selected["mode"] == "hold_response":
            event["state"] = "response_held"
            await asyncio.wait_for(selected["gate"].wait(), 1200)
        event["state"] = "released"
    headers = {k: v for k, v in response.headers.items() if k.lower() not in HOP | {"content-encoding"}}
    if request.method == "HEAD" and "content-length" in response.headers:
        headers["content-length"] = response.headers["content-length"]
    return Response(response.content, status_code=response.status_code, headers=headers)


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8080, access_log=False)
