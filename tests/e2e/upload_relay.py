"""Observe and hold real S3 transfers at the network boundary; never fake success."""

import asyncio
from collections import deque
from contextlib import asynccontextmanager
import hashlib
import re
import time
import xml.etree.ElementTree as ET
from urllib.parse import unquote

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import Response
import httpx
import uvicorn

events = deque(maxlen=16384)
active = 0
peak = 0
hold = None
failed_prefixes = []
HOP = {"connection", "keep-alive", "transfer-encoding", "host", "content-length"}


@asynccontextmanager
async def lifetime(app):
    async with httpx.AsyncClient(timeout=180, trust_env=False) as client:
        app.state.client = client
        yield


app = FastAPI(lifespan=lifetime)


def control(request):
    if request.headers.get("X-E2E-Control") != "e2e-upload-fault-only":
        raise HTTPException(401)


@app.get("/healthz")
async def health():
    return {"role": "test-upload-relay"}


@app.get("/status")
async def status(request: Request):
    control(request)
    return {"active": active, "peak": peak, "events": list(events)}


@app.post("/hold")
async def arm(request: Request):
    global hold, peak
    control(request)
    config = await request.json()
    if (not re.fullmatch(r"sources/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+/", config.get("prefix", ""))
            or not isinstance(config.get("parts"), list)
            or any(type(n) is not int or not 1 <= n <= 8 for n in config["parts"])):
        raise HTTPException(400)
    if hold and not hold["gate"].is_set():
        raise HTTPException(409)
    hold = {**config, "gate": asyncio.Event()}
    peak = active
    return {"armed": True}


@app.post("/release")
async def release(request: Request):
    control(request)
    if hold:
        hold["gate"].set()
    return {"released": True}


@app.post("/fail-listings")
async def fail_listings(request: Request):
    global failed_prefixes
    control(request)
    config = await request.json()
    prefixes = config.get("prefixes")
    if (not isinstance(prefixes, list) or len(prefixes) > 10
            or any(not re.fullmatch(r"sources/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+/", p) for p in prefixes)):
        raise HTTPException(400)
    failed_prefixes = prefixes
    return {"armed": len(prefixes)}


@app.api_route("/{path:path}", methods=["GET", "HEAD", "PUT", "POST", "DELETE"])
async def forward(path: str, request: Request):
    global active, peak
    body = bytearray()
    async for block in request.stream():
        body.extend(block)
        if len(body) > 32 << 20:
            raise HTTPException(413)
    key = unquote(path).partition("/")[2]
    part = request.query_params.get("partNumber")
    transfer = request.method == "PUT" and "/manifests/" not in key
    event = None
    if request.method == "GET" and key and not request.query_params:
        event = {"operation": "get", "key": key, "range": request.headers.get("range"), "at": time.time()}
        events.append(event)
    prefix = request.query_params.get("prefix")
    if request.method == "GET" and prefix in failed_prefixes:
        events.append({"operation": "list", "prefix": prefix, "status": 503, "at": time.time()})
        return Response("<Error><Code>ServiceUnavailable</Code><Message>Network fault</Message></Error>",
                        status_code=503, media_type="application/xml")
    if request.method == "POST" and "delete" in request.query_params:
        keys = [node.text for node in ET.fromstring(body).iter() if node.tag.rsplit("}", 1)[-1] == "Key"]
        event = {"operation": "delete", "keys": keys, "at": time.time()}
        events.append(event)
    if request.method == "DELETE" and "uploadId" in request.query_params:
        event = {"operation": "abort", "key": key, "at": time.time()}
        events.append(event)
    if transfer:
        active += 1
        peak = max(peak, active)
        event = {"key": key, "part": int(part) if part else None,
                 "bytes": len(body), "sha256": hashlib.sha256(body).hexdigest(), "state": "received"}
        events.append(event)
    try:
        if transfer and hold and key.startswith(hold["prefix"]) and (not hold["parts"] or event["part"] in hold["parts"]):
            event["state"] = "held"
            await asyncio.wait_for(hold["gate"].wait(), timeout=300)
        url = "http://aws:4566" + request.url.path
        if request.url.query:
            url += "?" + request.url.query
        headers = {k: v for k, v in request.headers.items() if k.lower() not in HOP}
        response = await app.state.client.request(request.method, url, headers=headers, content=bytes(body))
        if event is not None:
            event.update(state="forwarded", status=response.status_code)
            if event.get("operation") == "get":
                event["bytes"] = len(response.content)
        headers = {k: v for k, v in response.headers.items() if k.lower() not in HOP | {"content-encoding"}}
        if request.method == "HEAD" and "content-length" in response.headers:
            headers["content-length"] = response.headers["content-length"]
        return Response(response.content, status_code=response.status_code, headers=headers)
    finally:
        if transfer:
            active -= 1


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8080, access_log=False)
