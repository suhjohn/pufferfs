"""Readiness gate for the actual API after production migrations finish."""
import time
import sys
import socket
import urllib.error
import urllib.request

endpoints = sys.argv[1:]
if not endpoints:
    addresses = {row[4][0] for row in socket.getaddrinfo("api", 8080, type=socket.SOCK_STREAM)}
    endpoints = [f"http://{address}:8080/healthz" for address in addresses]
    endpoints.append("http://faults:8474/proxies/source-upload")
for endpoint in endpoints:
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
