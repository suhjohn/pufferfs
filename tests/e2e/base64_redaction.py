"""Real CLI/API/worker/provider E2E for data-URL payload redaction."""

import base64
import hashlib
import json
import os
from pathlib import Path
import time

import run

MARKER = "[base64 image]"


def fixtures():
    payload = base64.b64encode(bytes(range(256)) * 32768).decode()
    header = "data:image/png;base64,"
    cases = {}

    def case(name, prefix, encoded, suffix, uri=header):
        cases[name] = (prefix + uri + encoded + suffix, prefix + uri + MARKER + suffix)

    case("embedded.txt", "constellation landmark before\r\n", payload,
         "!\r\nconstellation landmark after\r\n")
    case("records.jsonl", '{"text":"constellation landmark","image":"',
         payload[:8192].replace("/", "\\/"), '"}\n{"text":"after image"}\n',
         "data:image\\/jpeg;base64,")
    case("marker-boundary.log", "x" * (5995 - len(header)), payload[:12000], "!after\n")
    case("header-boundary.md", ("boundary " * 7282)[:65533], payload[:16000], ")\nconstellation landmark\n")
    case("escape-boundary.jsonl", '{"image":"', "A" * (65534 - len('{"image":"') - len(header)) + "\\u002f" + "\\u0041" + "\\u003d",
         '","text":"constellation landmark"}\n')
    case("at-end.txt", "constellation landmark ", "SGVsbG8=", "", "data:application/octet-stream;base64,")
    case("parameters.txt", "constellation landmark ", "YWJjZA==", "!", "DATA:text/plain;charset=utf-8;BASE64,")
    case("percent-escapes.txt", "constellation landmark ", "%2F%2B%3D%41", "!")
    unchanged = "constellation landmark 測定 αβ\nSGVsbG8=\ndata:text/plain,hello\ndata:image/png;base64,\nends with data:"
    cases["ordinary.txt"] = (unchanged, unchanged)
    malformed = "data:text/plain" + ";charset=utf-8" * 128 + ";base63,SGVsbG8=\n"
    cases["malformed-header.txt"] = (malformed, malformed)
    # The same rule also applies to short spreadsheet cells, not only long rows.
    cases["cells.csv"] = ('value\n"data:image/png;base64,SGVsbG8="\n', {
        "contains": ['A2="data:image/png;base64,[base64 image]"'], "absent": ["SGVsbG8="]})
    return cases


def verify_files(state):
    directory = Path(state["redaction_directory"])
    expected = state["redaction_expected"]
    files = run.wait_indexed(state)
    assert {name for name, file in files.items() if not file["deleted"]} == set(expected)
    for name in state.get("redaction_deleted", []):
        assert files[name]["deleted"]
        run.request("POST", f"/roots/{state['root']}/read", {"path": name,
            "lines": {"start": 1,"end": 1}}, key=state["key"], statuses=(404,))
    total = 0
    indexed = {}
    for name, content in expected.items():
        raw = (directory / name).read_bytes()
        assert files[name]["content_hash"] == "sha256:" + hashlib.sha256(raw).hexdigest()
        run.assert_source_retained(files[name])
        rows = run.sql("""SELECT e.chunks_ref,e.chunk_count FROM file_catalog f
            JOIN file_extractions e ON e.id=f.indexed_extraction_id
            WHERE f.root_id=%s AND f.path=%s""", (state["root"], name))
        chunks = list(run.chunks(rows[0]["chunks_ref"]))
        total += len(chunks)
        assert len(chunks) == rows[0]["chunk_count"]
        text = "".join(chunk["content"] for chunk in chunks)
        indexed.update((chunk["content_hash"], chunk["content"]) for chunk in chunks)
        if isinstance(content, dict):
            assert all(value in text for value in content["contains"])
            assert all(value not in text for value in content["absent"])
            continue
        assert text == content, name
        assert chunks[0]["location"]["byte_start"] == 0
        assert chunks[-1]["location"]["byte_end"] == len(raw)
        for chunk in chunks:
            location = chunk["location"]
            start, end = location["byte_start"], location["byte_end"]
            assert 0 <= start < end <= len(raw)
            assert location["line_start"] == raw[:start].count(b"\n") + 1
            assert location["line_end"] == max(location["line_start"], raw[:end].count(b"\n") + int(raw[end - 1:end] != b"\n"))
        lines = content.split("\n")
        if lines[-1] == "":
            lines.pop()
        result = run.request("POST", f"/roots/{state['root']}/read", {
            "path": name, "lines": {"start": 1, "end": len(lines)}}, key=state["key"])
        assert [line["content"] for line in result["lines"]] == lines
    assert run.assert_index_vectors(state, state["root"], dimensions=4096) == total
    namespace = run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=%s AND retired_at IS NULL", (state["root"],))[0]["namespace"]
    extractions = [row["id"] for row in run.sql("""SELECT e.id FROM file_catalog f
        JOIN file_extractions e ON e.id=f.indexed_extraction_id WHERE f.root_id=%s AND NOT f.deleted""", (state["root"],))]
    result = run.request("POST", f"/v2/namespaces/{namespace}/query", {
        "rank_by": ["id", "asc"], "limit": 1000, "filters": ["extraction_id", "In", extractions],
        "include_attributes": ["content", "content_hash"]}, key=os.environ["TURBOPUFFER_API_KEY"],
        server=os.environ["TURBOPUFFER_API_URL"])
    assert len(result["rows"]) == total < 1000
    assert all(indexed[row["content_hash"]] == row["content"] for row in result["rows"])
    for mode in ("fts", "vector", "hybrid"):
        result = run.request("POST", "/query", {"root_id": state["root"], "query": "constellation landmark",
            "mode": mode, "top_k": 100}, key=state["key"])
        assert result["results"]
    # A scoped viewer cannot read the owner's user-scoped root.
    run.request("POST", f"/roots/{state['root']}/read", {"path": "embedded.txt", "lines": {"start": 1,"end": 2}},
                key=state["outsider_key"], statuses=(403,404))


def capture():
    state = run.provision()
    directory = Path("/state/base64-redaction")
    directory.mkdir()
    expected = {}
    for name, (raw, text) in fixtures().items():
        (directory / name).write_bytes(raw.encode())
        expected[name] = text
    state.update(redaction_directory=str(directory), redaction_expected=expected)
    state["root"] = run.new_root(state, "Base64 redaction", directory, False)
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", state["root"])
    verify_files(state)


def updated():
    state = json.loads(run.STATE.read_text())
    directory = Path(state["redaction_directory"])
    suffix = "\nupdated constellation landmark data:image/webp;base64,YWJjZA==!\n"
    with (directory / "embedded.txt").open("ab") as output:
        output.write(suffix.encode())
    state["redaction_expected"]["embedded.txt"] += suffix.replace("YWJjZA==", MARKER)
    (directory / "at-end.txt").unlink()
    del state["redaction_expected"]["at-end.txt"]
    state["redaction_deleted"] = ["at-end.txt"]
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", state["root"])
    verify_files(state)
    run.cli(state, "sync", str(directory), "--id", state["root"], "--force")
    verify_files(state)


if __name__ == "__main__":
    import sys
    phase, started, status = sys.argv[1], time.monotonic(), "failed"
    try:
        {"capture": capture, "updated": updated,
         "restarted": lambda: verify_files(json.loads(run.STATE.read_text()))}[phase]()
        status = "passed"
    finally:
        state = json.loads(run.STATE.read_text()) if run.STATE.exists() else {}
        result = {"run_id": state.get("nonce"), "phase": "base64-redaction-" + phase,
                  "status": status, "seconds": round(time.monotonic() - started, 2)}
        with run.REPORT.open("a") as output:
            output.write(json.dumps(result) + "\n")
        print(json.dumps(result), flush=True)
