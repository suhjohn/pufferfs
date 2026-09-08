"""Delay real immutable embedding-cache S3 traffic outside production processes."""

import asyncio
from contextlib import asynccontextmanager
from urllib.parse import unquote
import uuid

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import Response
import httpx
import uvicorn

fault = None
HOP = {"connection", "keep-alive", "transfer-encoding", "host", "content-length"}


@asynccontextmanager
async def lifetime(app):
    async with httpx.AsyncClient(timeout=180, trust_env=False) as client:
        app.state.client = client
        yield


app = FastAPI(lifespan=lifetime)


def control(request):
    if request.headers.get("X-E2E-Control") != "e2e-embedding-only":
        raise HTTPException(401)


@app.get("/healthz")
async def health():
    return {"role": "test-embedding-relay"}


@app.get("/status")
async def status(request: Request):
    control(request)
    return {"event": {k: v for k, v in fault.items() if k != "gate"} if fault else None}


@app.post("/fault")
async def arm(request: Request):
    global fault
    control(request)
    config = await request.json()
    if config.get("method") not in {"GET", "PUT"}:
        raise HTTPException(400)
    if fault and not fault["gate"].is_set():
        raise HTTPException(409)
    fault = {"id": uuid.uuid4().hex, "method": config["method"],
             "state": "armed", "gate": asyncio.Event()}
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
    body = await request.body()
    selected = None
    if (key.startswith("embeddings/") and fault and not fault["gate"].is_set()
            and request.method == fault["method"] and key == fault.get("key", key)):
        selected = fault
        selected.update(state="request_held", key=key,
                        held_requests=selected.get("held_requests", 0) + 1)
        await asyncio.wait_for(selected["gate"].wait(), 600)
    url = "http://aws:4566" + request.url.path
    if request.url.query:
        url += "?" + request.url.query
    headers = {k: v for k, v in request.headers.items() if k.lower() not in HOP}
    response = await app.state.client.request(request.method, url, headers=headers, content=body)
    if selected:
        selected.update(state="released", upstream_status=response.status_code)
    headers = {k: v for k, v in response.headers.items() if k.lower() not in HOP | {"content-encoding"}}
    if request.method == "HEAD" and "content-length" in response.headers:
        headers["content-length"] = response.headers["content-length"]
    return Response(response.content, status_code=response.status_code, headers=headers)


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8080, access_log=False)
