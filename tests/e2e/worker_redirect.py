"""HTTP polling boundary forwarding every job to the real worker process.

Only transport redirects are synthesized. Worker responses and all model,
object-store, database and search operations remain real. No request bodies,
credentials or worker response bodies are logged or exposed by inspection.
"""

import asyncio
from contextlib import asynccontextmanager
import json
import os
import uuid

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import RedirectResponse, Response
import httpx
import uvicorn

UPSTREAM = os.environ["E2E_WORKER_UPSTREAM"]
REDIRECTS = int(os.environ["E2E_WORKER_REDIRECTS"])
if not 11 <= REDIRECTS <= 24:
    raise ValueError("redirect fixture must exceed Go's default and fit the production bound")
calls = {}


@asynccontextmanager
async def lifetime(app):
    async with httpx.AsyncClient(timeout=3700, trust_env=False) as client:
        app.state.client = client
        yield
        for call in calls.values():
            if not call["task"].done():
                call["task"].cancel()
        await asyncio.gather(*(call["task"] for call in calls.values()), return_exceptions=True)


app = FastAPI(lifespan=lifetime)


@app.get("/healthz")
async def health():
    return {"role": "test-worker-redirect"}


@app.get("/status")
async def status(request: Request):
    if request.headers.get("X-E2E-Control") != "e2e-worker-redirect-only":
        raise HTTPException(401)
    return {"calls": [{"work_id": c["work_id"], "last_poll": c["last_poll"]} for c in calls.values()]}


@app.post("/")
async def submit(request: Request):
    body = await request.body()
    if len(body) > 65536:
        raise HTTPException(413)
    work = json.loads(body)["work_id"]
    call_id = uuid.uuid4().hex
    # The provider-side call survives the original HTTP caller, as work behind
    # a result-polling endpoint does. It still runs the normal authenticated role.
    task = asyncio.create_task(app.state.client.post(UPSTREAM, content=body,
                                                   headers={"Content-Type": "application/json"}))
    calls[call_id] = {"work_id": work, "task": task, "last_poll": -1}
    return RedirectResponse(f"/result/{call_id}/0", status_code=303)


@app.get("/result/{call_id}/{poll}")
async def result(call_id: str, poll: int):
    call = calls.get(call_id)
    if call is None or not 0 <= poll <= REDIRECTS:
        raise HTTPException(404)
    call["last_poll"] = max(call["last_poll"], poll)
    if poll < REDIRECTS:
        return RedirectResponse(f"/result/{call_id}/{poll + 1}", status_code=303)
    try:
        response = await asyncio.shield(call["task"])
    except httpx.HTTPError:
        raise HTTPException(502, "worker transport failed") from None
    return Response(response.content, status_code=response.status_code,
                    media_type=response.headers.get("content-type"))


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8080, access_log=False)
