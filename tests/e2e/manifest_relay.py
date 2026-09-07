"""S3 network fault boundary; forwards real storage responses, never changes objects."""
from collections import deque
from contextlib import asynccontextmanager
import re
from urllib.parse import unquote
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import Response
import httpx
import uvicorn

fault = None
counts = {"puts": 0, "gets": 0}
events = deque(maxlen=64)
HOP = {"connection", "keep-alive", "transfer-encoding", "host", "content-length"}

@asynccontextmanager
async def lifetime(app):
    async with httpx.AsyncClient(timeout=180, trust_env=False) as client:
        app.state.client = client
        yield

app = FastAPI(lifespan=lifetime)

def control(request):
    if request.headers.get("X-E2E-Control") != "e2e-manifest-fault-only":
        raise HTTPException(401)

@app.get("/healthz")
async def health():
    return {"role": "test-manifest-relay"}

@app.get("/status")
async def status(request: Request):
    control(request)
    return {**counts, "events": list(events)}

@app.post("/fault")
async def arm(request: Request):
    global fault
    control(request)
    config = await request.json()
    if config.get("mode") not in {"flip", "truncate"} or not re.fullmatch(
            r"sources/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+/manifests/", config.get("prefix", "")):
        raise HTTPException(400)
    fault = config
    return {"armed": True}

@app.delete("/fault")
async def clear(request: Request):
    global fault
    control(request)
    fault = None
    return {"cleared": True}

@app.api_route("/{path:path}", methods=["GET", "HEAD", "PUT", "POST", "DELETE"])
async def forward(path: str, request: Request):
    url = "http://aws:4566" + request.url.path
    if request.url.query:
        url += "?" + request.url.query
    headers = {k:v for k,v in request.headers.items() if k.lower() not in HOP}
    response = await app.state.client.request(request.method, url, headers=headers, content=await request.body())
    body = response.content
    headers = {k:v for k,v in response.headers.items() if k.lower() not in HOP | {"content-encoding"}}
    if request.method == "HEAD" and "content-length" in response.headers:
        headers["content-length"] = response.headers["content-length"]
    key = unquote(path).partition("/")[2]
    if "/manifests/" in key and response.is_success:
        if request.method == "PUT":
            counts["puts"] += 1
        if request.method == "GET":
            counts["gets"] += 1
            if fault and key.startswith(fault["prefix"]):
                mode = fault["mode"]
                if body:
                    body = bytes([body[0] ^ 1]) + body[1:] if mode == "flip" else body[:-1]
                headers = {k:v for k,v in headers.items() if not k.lower().startswith("x-amz-checksum-") and k.lower() != "content-md5"}
                events.append({"key": key, "mode": mode, "range": request.headers.get("range")})
    return Response(body, status_code=response.status_code, headers=headers)

if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8080, access_log=False)
