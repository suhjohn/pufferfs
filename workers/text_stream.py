"""Checkpointable text chunking with exact source and redaction boundaries."""
import base64
from collections import deque

from base64_redaction import Redactor
from extraction import chunk_record, utf8_boundary


class TextStream:
    def __init__(self, *, target_bytes=6000, byte_start=0, line_start=1, state=None):
        if target_bytes < 4 or byte_start < 0 or line_start < 1:
            raise ValueError("invalid text chunk boundaries")
        self.target = target_bytes
        self.pending = bytearray()
        self.byte_start, self.line_start, self.ordinal = byte_start, line_start, 0
        self.source_cursor, self.output_cursor, self.shift = byte_start, byte_start, 0
        self.replacements = deque()
        self.redactor = Redactor()
        if state is not None:
            if state.get("format") != 1 or state.get("target") != target_bytes:
                raise ValueError("unsupported text checkpoint")
            self.pending = bytearray(base64.b64decode(state["pending"], validate=True))
            for name in ("byte_start", "line_start", "ordinal", "source_cursor", "output_cursor"):
                value = state[name]
                if type(value) is not int or value < (1 if name == "line_start" else 0):
                    raise ValueError("invalid text checkpoint position")
                setattr(self, name, value)
            self.shift = state["shift"]
            self.replacements = deque(tuple(row) for row in state["replacements"])
            self.redactor = Redactor(state["redactor"])
            if (len(self.pending) > target_bytes or type(self.shift) is not int
                    or self.output_cursor != self.byte_start + len(self.pending)
                    or len(self.replacements) > target_bytes + 1
                    or any(len(row) != 4 or any(type(n) is not int or n < 0 for n in row)
                           or row[0] >= row[1] or row[2] >= row[3] for row in self.replacements)):
                raise ValueError("invalid text checkpoint bounds")

    def snapshot(self):
        return {"format": 1, "target": self.target,
            "pending": base64.b64encode(self.pending).decode(),
            "byte_start": self.byte_start, "line_start": self.line_start, "ordinal": self.ordinal,
            "source_cursor": self.source_cursor, "output_cursor": self.output_cursor,
            "shift": self.shift, "replacements": list(self.replacements),
            "redactor": self.redactor.snapshot()}

    def _source_boundary(self, position, *, end):
        delta = self.shift
        for left, right, source_left, source_right in self.replacements:
            if position <= left:
                return position + delta
            if position < right:
                return source_right if end else source_left
            delta = source_right - right
        return position + delta

    def _emit(self, end):
        if end <= 0:
            raise ValueError("invalid UTF-8 chunk boundary")
        piece = bytes(self.pending[:end])
        content = piece.decode("utf-8", errors="strict")
        if "\x00" in content:
            raise ValueError("binary input is not a text file")
        left, right = self.byte_start, self.byte_start + end
        location = {"byte_start": self._source_boundary(left, end=False),
            "byte_end": self._source_boundary(right, end=True), "line_start": self.line_start,
            "line_end": max(self.line_start, self.line_start + piece.count(b"\n") - int(piece.endswith(b"\n")))}
        if any(a < right and b > left for a, b, _, _ in self.replacements):
            location["redacted"] = True
        while self.replacements and self.replacements[0][1] <= right:
            _, output_end, _, source_end = self.replacements.popleft()
            self.shift = source_end - output_end
        chunk = chunk_record(content, location, self.ordinal)
        self.line_start += piece.count(b"\n")
        self.byte_start = right
        self.ordinal += 1
        del self.pending[:end]
        return chunk

    def feed(self, block):
        for data, consumed, changed in self.redactor.feed(block):
            if changed:
                self.replacements.append((self.output_cursor, self.output_cursor + len(data),
                    self.source_cursor, self.source_cursor + consumed))
            self.source_cursor += consumed
            self.output_cursor += len(data)
            for offset in range(0, len(data), self.target):
                self.pending.extend(data[offset:offset + self.target])
                while len(self.pending) > self.target:
                    end = self.pending.rfind(b"\n", 0, self.target) + 1
                    if not end:
                        end = utf8_boundary(self.pending, self.target)
                    yield self._emit(end)
        if block is None and self.pending:
            yield self._emit(len(self.pending))
