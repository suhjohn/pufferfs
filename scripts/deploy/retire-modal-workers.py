#!/usr/bin/env python3
"""Stop retired CPU apps and wait for their containers to exit; leave vision alone."""

import json
import os
import subprocess
import sys
import time


RETIRED_APPS = {
    "pufferfs-transform",
    "pufferfs-batch-collector",
    "pufferfs-index",
    "pufferfs-reconciliation",
}


def main():
    environment = os.environ.get("MODAL_ENVIRONMENT", "").strip()
    if not environment:
        raise RuntimeError("Set MODAL_ENVIRONMENT for the retired Modal apps")

    def modal(*args):
        return subprocess.check_output(
            [sys.executable, "-m", "modal", "app", *args, "--env", environment],
            text=True,
            timeout=60,
        )

    def retired_apps():
        apps = json.loads(modal("list", "--json"))
        if not isinstance(apps, list) or any(
            not isinstance(app, dict)
            or any(not isinstance(app.get(key), str) for key in ("app_id", "description", "state"))
            or not str(app.get("tasks", "")).isdigit()
            for app in apps
        ):
            raise RuntimeError("Unexpected Modal app-list JSON; refusing to claim workers are stopped")
        return [app for app in apps if app["description"] in RETIRED_APPS]

    for app in retired_apps():
        if app["state"] != "stopped":
            print(f"Stopping {app['description']} ({app['app_id']})", flush=True)
            modal("stop", app["app_id"], "--yes")

    deadline = time.monotonic() + 180
    while True:
        remaining = [
            app for app in retired_apps()
            if app["state"] != "stopped" or int(app["tasks"]) != 0
        ]
        if not remaining:
            print("Verified: no retired PufferFS Modal apps or containers are running.")
            return
        if time.monotonic() >= deadline:
            raise RuntimeError(f"Retired Modal workers have not stopped: {remaining}")
        print("Waiting for retired Modal apps and containers to stop...", flush=True)
        time.sleep(5)


if __name__ == "__main__":
    main()
