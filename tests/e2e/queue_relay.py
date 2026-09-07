"""Real SQS forwarding with connection loss after a bounded number of sends.

No production imports, database access or fabricated SQS responses. Control
state contains only work identities; request headers and bodies are not logged.
"""

from collections import deque
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import socket
import threading


lock = threading.Lock()
remaining = None
events = deque(maxlen=256)
HOP_HEADERS = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
               "te", "trailer", "transfer-encoding", "upgrade", "host", "content-length"}


class Relay(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, body, headers=None):
        self.send_response(status)
        for key, value in (headers or {"Content-Type": "application/json"}).items():
            if key.lower() not in HOP_HEADERS:
                self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path not in {"/healthz", "/events"}:
            self.reply(404, b"{}")
            return
        with lock:
            body = json.dumps(list(events) if self.path == "/events" else {"ready": True}).encode()
        self.reply(200, body)

    def disconnect(self):
        self.close_connection = True
        try:
            self.connection.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        self.connection.close()

    def do_POST(self):
        global remaining
        length = int(self.headers.get("Content-Length", "0"))
        if not 0 <= length <= 1 << 20:
            self.reply(413, b"{}")
            return
        body = self.rfile.read(length)
        if self.path == "/fault":
            config = json.loads(body)
            allowed = config["allow_send_requests"]
            if allowed is not None and (type(allowed) is not int or not 0 <= allowed <= 128):
                self.reply(400, b"{}")
                return
            with lock:
                remaining = allowed
            self.reply(200, b"{}")
            return
        entries = None
        if self.headers.get("X-Amz-Target", "").endswith(".SendMessageBatch"):
            entries = {entry["Id"]: json.loads(entry["MessageBody"])["work_id"]
                       for entry in json.loads(body)["Entries"]}
            with lock:
                drop = remaining == 0
                if remaining is not None and remaining > 0:
                    remaining -= 1
                if drop:
                    events.append({"state": "disconnected", "work_ids": list(entries.values())})
            if drop:
                self.disconnect()
                return
        upstream = http.client.HTTPConnection("aws", 4566, timeout=30)
        try:
            headers = {key: value for key, value in self.headers.items() if key.lower() not in HOP_HEADERS}
            upstream.request("POST", self.path, body, headers)
            response = upstream.getresponse()
            data = response.read()
            if entries is not None:
                result = json.loads(data)
                with lock:
                    events.append({"state": "forwarded", "status": response.status,
                        "work_ids": [entries[item["Id"]] for item in result.get("Successful", [])]})
            self.reply(response.status, data, dict(response.getheaders()))
        except (OSError, http.client.HTTPException):
            self.disconnect()
        finally:
            upstream.close()


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Relay).serve_forever()
