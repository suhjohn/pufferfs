"""Bounded text redaction of explicitly encoded data-URL payloads.

Yield (replacement bytes, original byte count, was redacted). Whitespace ends
a URL; bare base64 and non-base64 URLs remain ordinary text. JSON's escaped
slash, ASCII Unicode escapes and URL percent escapes are accepted inside payloads.
"""

import re

MARKER = b"[base64 image]"
HEADER_LIMIT = 4096
START = re.compile(rb"data:", re.I)
HEADER = re.compile(
    rb"data:(?:[a-z0-9!#$&^_.+*-]+(?:/|\\/)[a-z0-9!#$&^_.+*-]+)?"
    rb"(?:;[a-z0-9!#$&^_.+*-]+=[^,;\s\"'<>]+)*;base64,", re.I)
PAYLOAD = re.compile(
    rb"(?:[A-Za-z0-9+/=]+|\\/|(?:\\u00|%)(?:2[bBfF]|3[0-9dD]|[46][1-9a-fA-F]|[57][0-9aA]))+")


def redacted_spans(blocks):
    def bounded_blocks():
        for block in blocks:
            for offset in range(0, len(block), 65536):
                yield block[offset:offset + 65536]
        yield None

    pending = b""
    payload = False
    removed = 0
    for block in bounded_blocks():
        eof = block is None
        if block is not None:
            pending += block
        while pending or (eof and payload):
            if payload:
                match = PAYLOAD.match(pending)
                if match:
                    removed += match.end()
                    pending = pending[match.end():]
                    if not pending and not eof:
                        break
                    continue
                # An escape can straddle an input read.
                if not eof and (not pending or (pending.startswith(b"\\") and len(pending) < 6)
                                or (pending.startswith(b"%") and len(pending) < 3)):
                    break
                if removed:
                    yield MARKER, removed, True
                payload, removed = False, 0
                continue

            match = START.search(pending)
            if match is None:
                end = len(pending) if eof else max(0, len(pending) - 4)
                if end:
                    yield pending[:end], end, False
                    pending = pending[end:]
                break
            if match.start():
                yield pending[:match.start()], match.start(), False
                pending = pending[match.start():]
            comma = pending.find(b",", 0, HEADER_LIMIT)
            if comma < 0 and len(pending) < HEADER_LIMIT and not eof:
                break
            end = comma + 1
            if comma >= 0 and HEADER.fullmatch(pending[:end]):
                yield pending[:end], end, False
                pending = pending[end:]
                payload = True
            else:
                yield pending[:5], 5, False
                pending = pending[5:]
