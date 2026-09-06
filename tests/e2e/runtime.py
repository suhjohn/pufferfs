"""Compose process adapter, not a second implementation of worker behavior.

Modal's local invocation runs the actual decorated entrypoint, including class
initializers. No Modal deployment, remote function, stub or monkeypatch is used.
"""

import importlib
import os
from pathlib import Path
import signal
import sys
import threading
import time

sys.path.insert(0, "/app/modal")
os.chdir("/app/modal")


def main():
    role = sys.argv[1]
    target = {
        "transform": ("transform_app", "transform_file"),
        "index-cpu": ("index_cpu_app", "index"),
        "index-vector": ("index_gpu_app", "Indexer"),
        "query": ("query_app", "QueryEmbedder"),
        "collector": ("collector_app", "collect"),
        "reconciler": ("reconciliation_app", "reconcile"),
    }[role]
    entry = getattr(importlib.import_module(target[0]), target[1])
    if role in {"collector", "reconciler"}:
        stopped = threading.Event()
        signal.signal(signal.SIGTERM, lambda *_: stopped.set())
        signal.signal(signal.SIGINT, lambda *_: stopped.set())
        while not stopped.is_set():
            started = time.monotonic()
            try:
                entry.local()
            except Exception as error:
                # An invocation failure does not cancel Modal's next schedule.
                # Do not refresh the health heartbeat for a failed invocation.
                print(f"{role} scheduled invocation failed: {type(error).__name__}", flush=True)
            else:
                Path("/tmp/role-heartbeat").touch()
            stopped.wait(max(0, 60 - (time.monotonic() - started)))
        return

    if role == "index-vector":
        instance = entry()
        entry = instance.index
    elif role == "query":
        instance = entry()
        entry = instance.embed_query_endpoint

    from fastapi import FastAPI
    import uvicorn

    app = FastAPI()
    # These Modal roles do not opt into @modal.concurrent: each container
    # processes one input at a time. FastAPI's default thread pool must not
    # introduce shared-model concurrency absent from the deployed topology.
    invocation = threading.Lock()

    @app.get("/healthz")
    def health():
        return {"role": role}

    @app.post("/")
    def execute(item: dict):
        with invocation:
            return entry.local(item)

    uvicorn.run(app, host="0.0.0.0", port=8080, access_log=False)


if __name__ == "__main__":
    main()
