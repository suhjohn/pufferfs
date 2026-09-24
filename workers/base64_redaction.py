"""Bounded text redaction of explicitly encoded data-URL payloads.

Yield (replacement bytes, original byte count, was redacted). Whitespace ends
a URL; bare base64 and non-base64 URLs remain ordinary text. JSON's escaped
slash, ASCII Unicode escapes and URL percent escapes are accepted inside payloads.
"""

import base64
import re

MARKER = b"[base64 image]"
HEADER_LIMIT = 4096
START = re.compile(rb"data:", re.I)
HEADER = re.compile(
    rb"data:(?:[a-z0-9!#$&^_.+*-]+(?:/|\\/)[a-z0-9!#$&^_.+*-]+)?"
    rb"(?:;[a-z0-9!#$&^_.+*-]+=[^,;\s\"'<>]+)*;base64,", re.I)
PAYLOAD = re.compile(
    rb"(?:[A-Za-z0-9+/=]+|\\/|(?:\\u00|%)(?:2[bBfF]|3[0-9dD]|[46][1-9a-fA-F]|[57][0-9aA]))+")


class Redactor:
    """A bounded data-URL parser whose state can survive a source segment."""
    def __init__(self, state=None):
        self.pending, self.payload, self.removed, self.finished = b"", False, 0, False
        if state is not None:
            if set(state) != {"pending", "payload", "removed"}:
                raise ValueError("invalid redactor checkpoint")
            self.pending = base64.b64decode(state["pending"], validate=True)
            self.payload, self.removed = state["payload"], state["removed"]
            if (len(self.pending) > HEADER_LIMIT + 6 or type(self.payload) is not bool
                    or type(self.removed) is not int or self.removed < 0
                    or (self.removed and not self.payload)):
                raise ValueError("invalid redactor checkpoint bounds")

    def snapshot(self):
        if self.finished or len(self.pending) > HEADER_LIMIT + 6:
            raise ValueError("redactor checkpoint requires a fully consumed input block")
        return {"pending": base64.b64encode(self.pending).decode(),
                "payload": self.payload, "removed": self.removed}

    def feed(self, block):
        if self.finished:
            raise ValueError("redactor already finished")
        eof = block is None
        if block is not None:
            if len(block) > 65536:
                raise ValueError("redactor input exceeds 64 KiB")
            self.pending += block
        while self.pending or (eof and self.payload):
            if self.payload:
                match = PAYLOAD.match(self.pending)
                if match:
                    self.removed += match.end()
                    self.pending = self.pending[match.end():]
                    if not self.pending and not eof:
                        break
                    continue
                if not eof and (not self.pending or (self.pending.startswith(b"\\") and len(self.pending) < 6)
                                or (self.pending.startswith(b"%") and len(self.pending) < 3)):
                    break
                if self.removed:
                    yield MARKER, self.removed, True
                self.payload, self.removed = False, 0
                continue
            match = START.search(self.pending)
            if match is None:
                end = len(self.pending) if eof else max(0, len(self.pending) - 4)
                if end:
                    yield self.pending[:end], end, False
                    self.pending = self.pending[end:]
                break
            if match.start():
                yield self.pending[:match.start()], match.start(), False
                self.pending = self.pending[match.start():]
            comma = self.pending.find(b",", 0, HEADER_LIMIT)
            if comma < 0 and len(self.pending) < HEADER_LIMIT and not eof:
                break
            end = comma + 1
            if comma >= 0 and HEADER.fullmatch(self.pending[:end]):
                yield self.pending[:end], end, False
                self.pending = self.pending[end:]
                self.payload = True
            else:
                yield self.pending[:5], 5, False
                self.pending = self.pending[5:]
        if eof:
            self.finished = True


def redacted_spans(blocks):
    redactor = Redactor()
    for block in blocks:
        for offset in range(0, len(block), 65536):
            yield from redactor.feed(block[offset:offset + 65536])
    yield from redactor.feed(None)
