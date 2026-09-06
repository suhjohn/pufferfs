"""Readiness gate for the actual API after production migrations finish."""
import time
import sys
import urllib.error
import urllib.request

for endpoint in sys.argv[1:] or ("http://api:8080/healthz", "http://faults:8474/proxies/source-upload",
                               "http://faults:8474/proxies/queue-delivery"):
    for attempt in range(60):
        try:
            with urllib.request.urlopen(endpoint, timeout=3) as response:
                if response.status == 200:
                    break
        except (urllib.error.URLError, TimeoutError):
            pass
        time.sleep(1)
    else:
        raise SystemExit(f"Compose dependency did not become ready: {endpoint}")
