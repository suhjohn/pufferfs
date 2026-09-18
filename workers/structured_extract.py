"""Ordinary email/contact/calendar fields -> bounded text chunks, locally."""

from email import policy
from email.parser import BytesParser
from html.parser import HTMLParser
from pathlib import Path
import quopri
import re

from extraction import text_chunks

FIELDS = {
    ".vcf": {"FN": "Name", "N": "Name", "ORG": "Organization", "TITLE": "Title", "EMAIL": "Email",
             "TEL": "Phone", "ADR": "Address", "URL": "URL", "NOTE": "Note", "CATEGORIES": "Categories"},
    ".ics": {"SUMMARY": "Title", "DTSTART": "Start", "DTEND": "End", "DUE": "Due", "LOCATION": "Location",
             "DESCRIPTION": "Description", "ORGANIZER": "Organizer", "ATTENDEE": "Attendee", "STATUS": "Status", "URL": "URL"},
}


class HTMLText(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.parts = []
        self.hidden = 0

    def handle_starttag(self, tag, attrs):
        if tag in {"script", "style"}:
            self.hidden += 1
        elif tag in {"p", "br", "div", "li", "tr"}:
            self.parts.append("\n")

    def handle_endtag(self, tag):
        if tag in {"script", "style"}:
            self.hidden = max(0, self.hidden - 1)
        elif tag in {"p", "div", "li", "tr"}:
            self.parts.append("\n")

    def handle_data(self, data):
        if not self.hidden:
            self.parts.append(data)


def html_text(value):
    parser = HTMLText()
    parser.feed(value)
    parser.close()
    return "".join(parser.parts)


def email_text(path):
    # MIME parsers retain the message tree; bound this explicit allocation.
    if Path(path).stat().st_size > 32 * 1024 * 1024:
        raise ValueError("structured email exceeds 32 MiB parser limit")
    if Path(path).suffix.lower() == ".msg":
        import extract_msg
        message = extract_msg.Message(path)
        try:
            headers = [("Subject", message.subject), ("From", message.sender), ("To", message.to),
                       ("Cc", message.cc), ("Date", message.date)]
            body = message.body
            if not body:
                body = message.htmlBody or b""
                body = html_text(body.decode("utf-8", errors="replace") if isinstance(body, bytes) else body)
        finally:
            message.close()
    else:
        with open(path, "rb") as source:
            message = BytesParser(policy=policy.default).parse(source)
        headers = [(name, message.get(name)) for name in ("Subject", "From", "To", "Cc", "Date")]
        part = message.get_body(preferencelist=("plain", "html"))
        body = part.get_content() if part else ""
        if part and part.get_content_type() == "text/html":
            body = html_text(body)
    return "\n".join(f"{name}: {value}" for name, value in headers if value) + "\n\n" + (body or "")


def unfolded_lines(source):
    pending = ""
    while raw := source.readline(1024 * 1024 + 1):
        if len(raw.encode()) > 1024 * 1024:
            raise ValueError("structured field exceeds 1 MiB")
        raw = raw.rstrip("\r\n")
        if raw.startswith((" ", "\t")):
            pending += raw[1:]
            if len(pending.encode()) > 1024 * 1024:
                raise ValueError("unfolded field exceeds 1 MiB")
        else:
            if pending:
                yield pending
            pending = raw
    if pending:
        yield pending


def structured_chunks(path):
    suffix = Path(path).suffix.lower()
    if suffix in {".eml", ".msg"}:
        for chunk in text_chunks([email_text(path).encode()]):
            chunk["location"] = {"record_number": 0, "part": chunk["chunk_index"]}
            yield chunk
        return
    fields = FIELDS[suffix]
    ordinal, record = 0, -1
    with open(path, encoding="utf-8-sig", errors="strict", newline=None) as source:
        for line in unfolded_lines(source):
            if line.upper() in {"BEGIN:VCARD", "BEGIN:VEVENT", "BEGIN:VTODO"}:
                record += 1
            if ":" not in line:
                continue
            raw_key, value = line.split(":", 1)
            key = raw_key.split(";", 1)[0].split(".")[-1].upper()
            if key not in fields:
                continue
            if "ENCODING=QUOTED-PRINTABLE" in raw_key.upper():
                value = quopri.decodestring(value).decode("utf-8", errors="replace")
            value = re.sub(r"\\([nN,;\\])", lambda m: "\n" if m[1] in "nN" else m[1], value)
            content = f"{fields[key]}: {value}\n"
            for part, chunk in enumerate(text_chunks([content.encode()])):
                chunk["chunk_index"] = ordinal
                chunk["location"] = {"record_number": max(record, 0), "field": key, "part": part}
                ordinal += 1
                yield chunk
