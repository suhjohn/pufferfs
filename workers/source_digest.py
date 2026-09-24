"""Bounded pipe IO to the standard-library resumable SHA-256 implementation."""
import base64
import json
import struct
import subprocess


class SourceDigest:
    def __init__(self, state=None):
        self.process = subprocess.Popen(["pufferfs-source-digest"], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        try:
            if state is not None:
                decoded = base64.b64decode(state, validate=True)
                if len(decoded) > 1024:
                    raise ValueError("source digest state exceeds bounds")
                self._send(b"R", decoded)
                self.snapshot()  # Validate restoration before reading source.
        except Exception:
            self.close()
            raise

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

    def _send(self, command, payload=b""):
        if len(payload) > 1048576:
            raise ValueError("digest update exceeds one MiB")
        self.process.stdin.write(command + struct.pack(">I", len(payload)))
        self.process.stdin.write(payload)

    def update(self, data):
        self._send(b"D", data)

    def snapshot(self):
        self._send(b"S")
        self.process.stdin.flush()
        line = self.process.stdout.readline(4097)
        if not line.endswith(b"\n") or len(line) > 4096:
            raise RuntimeError("source digest process failed")
        result = json.loads(line)
        if set(result) != {"state", "hash"} or not result["hash"].startswith("sha256:") or len(result["hash"]) != 71:
            raise ValueError("invalid source digest response")
        return result

    def close(self):
        try:
            self.process.stdin.close()
        except (BrokenPipeError, OSError):
            pass
        try:
            self.process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=3)
        self.process.stdout.close()
