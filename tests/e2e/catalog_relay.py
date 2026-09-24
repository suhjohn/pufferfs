"""Hold real API catalog responses to interrupt a client's durable cache update."""
import asyncio
from collections import deque
from contextlib import asynccontextmanager
import hashlib
import re

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import Response
import httpx
import uvicorn

events = deque(maxlen=4096)
fault = None
HOP = {"connection", "keep-alive", "transfer-encoding", "host", "content-length", "content-encoding"}


@asynccontextmanager
async def lifetime(app):
    async with httpx.AsyncClient(timeout=180, trust_env=False) as client:
        app.state.client = client
        yield


app = FastAPI(lifespan=lifetime)


def control(request):
    if request.headers.get("X-E2E-Control") != "e2e-catalog-fault-only":
        raise HTTPException(401)


@app.get("/healthz")
async def health():
    return {"role": "catalog-response-relay"}


@app.get("/status")
async def status(request: Request):
    control(request)
    return {"events": list(events)}


@app.post("/hold")
async def hold(request: Request):
    global fault
    control(request)
    config = await request.json()
    if not re.fullmatch(r"[A-Za-z0-9_-]+", config.get("root", "")) or type(config.get("skip")) is not int or not 0 <= config["skip"] <= 10:
        raise HTTPException(400)
    if fault and not fault["gate"].is_set():
        raise HTTPException(409)
    fault = {**config, "gate": asyncio.Event(), "selected": False}
    return {"armed": True}


@app.post("/release")
async def release(request: Request):
    control(request)
    if fault:
        fault["gate"].set()
    return {"released": True}


@app.api_route("/{path:path}", methods=["GET", "POST", "PUT", "DELETE"])
async def forward(path: str, request: Request):
    body = await request.body()
    headers = {k: v for k, v in request.headers.items() if k.lower() not in HOP}
    response = await app.state.client.request(request.method, "http://api:8080/" + path,
        params=request.query_params, content=body, headers=headers)
    if request.method == "GET" and path.endswith("/catalog-changes"):
        event = {"path": path, "cursor_sha256": hashlib.sha256(request.query_params.get("cursor", "").encode()).hexdigest(),
                 "status": response.status_code, "state": "forwarded"}
        if response.status_code == 200:
            event["files"] = len(response.json()["files"])
        events.append(event)
        if fault and path == f"roots/{fault['root']}/catalog-changes" and not fault["selected"]:
            if fault["skip"]:
                fault["skip"] -= 1
            else:
                fault["selected"] = True
                event["state"] = "held"
                await asyncio.wait_for(fault["gate"].wait(), timeout=300)
                event["state"] = "released"
    headers = {k: v for k, v in response.headers.items() if k.lower() not in HOP}
    return Response(response.content, status_code=response.status_code, headers=headers)


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8080, access_log=False)
