"""Per-invocation timings and counts, without source text or credentials.

Timings are inclusive (a parent phase includes its child phases); do not sum
them as disjoint wall time. Context is local to the executing request thread.
"""

from contextlib import contextmanager
from contextvars import ContextVar
from functools import wraps
import json
import os
import socket
import time


current = ContextVar("worker_metrics", default=None)


@contextmanager
def timed(name):
    metrics = current.get()
    if metrics is None:
        yield
        return
    started = time.perf_counter()
    try:
        yield
    finally:
        metrics["seconds"][name] = metrics["seconds"].get(name, 0) + time.perf_counter() - started
        count(name + "_calls")


def count(name, amount=1):
    metrics = current.get()
    if metrics is not None:
        metrics["counts"][name] = metrics["counts"].get(name, 0) + amount


def profile_work(stage):
    def decorate(function):
        @wraps(function)
        def measured(work, *args, **kwargs):
            metrics = {"event": "file_work_metrics", "stage": stage, "work_id": work,
                       "seconds": {}, "counts": {}, "status": "error",
                       "started_at": time.time(), "container": socket.gethostname(),
                       "region": os.getenv("MODAL_REGION", "local")}
            token = current.set(metrics)
            started = time.perf_counter()
            try:
                result = function(work, *args, **kwargs)
                metrics["status"] = result["status"]
                return result
            finally:
                metrics["total_seconds"] = round(time.perf_counter() - started, 6)
                metrics["seconds"] = {k: round(v, 6) for k, v in metrics["seconds"].items()}
                current.reset(token)
                print(json.dumps(metrics, separators=(",", ":")), flush=True)
        return measured
    return decorate
