"""Real cache retirement while CLI-triggered worker S3 requests are delayed."""

import json
from pathlib import Path
import urllib.request

import run


def relay(method, path, payload=None):
    data = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request("http://embedding-relay:8080" + path, method=method,
        data=data, headers={"Content-Type": "application/json", "X-E2E-Control": "e2e-embedding-only"})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def absent(key):
    try:
        run.s3.head_object(Bucket=run.BUCKET, Key=key)
    except run.s3.exceptions.ClientError as error:
        if error.response["ResponseMetadata"]["HTTPStatusCode"] == 404:
            return True
        raise
    return False


def verify():
    state = run.provision()
    directory = Path("/state/embedding-io")
    directory.mkdir()
    expected = {"first.txt": "Orchid observatory telescope calibration.\n"}
    (directory / "first.txt").write_text(expected["first.txt"])
    root = run.new_root(state, "Embedding IO retirement", directory, False)
    run.cli(state, "sync", str(directory), "--id", root)
    run.wait_indexed(state, root)
    initial, = run.embedding_locations(state["org"])
    for method, path, content in (
        ("GET", "second.txt", expected["first.txt"]),
        ("PUT", "third.txt", "Violet observatory telescope calibration with new sensor readings.\n"),
    ):
        fault = relay("POST", "/fault", {"method": method})["fault_id"]
        expected[path] = content
        (directory / path).write_text(content)
        try:
            run.cli(state, "sync", str(directory), "--id", root)
            def held():
                event = relay("GET", "/status")["event"]
                return event if event["id"] == fault and event["state"] == "request_held" else None
            event = run.eventually("embedding S3 request held outside the worker", held, 60)
            key = event["key"]
            if method == "GET":
                assert key == initial["object_key"]
            def retired():
                rows = run.sql("SELECT retired_at,deleted_at,content_hashes FROM embedding_packs WHERE object_key=%s", (key,))
                return rows and rows[0]["retired_at"] and rows[0]["deleted_at"] and not rows[0]["content_hashes"] and absent(key)
            run.eventually("scheduled retirement proceeds during delayed S3 IO", retired, 240)
            relay("POST", "/release")
            files = run.wait_indexed(state, root)
            traffic = relay("GET", "/status")["event"]
            assert traffic["upstream_status"] == (404 if method == "GET" else 200)
            assert run.sql("SELECT content_hashes FROM embedding_packs WHERE object_key=%s", (key,)) == [{"content_hashes": []}]
            if method == "PUT":
                run.eventually("late PUT cleaned without reviving the cache directory", lambda: absent(key), 120)
            work = run.sql("""SELECT w.status,w.attempt_count,w.mutation_ref FROM file_work w
                JOIN file_extractions e ON e.id=w.extraction_id JOIN file_versions v ON v.id=e.version_id
                JOIN file_catalog f ON f.id=v.file_id
                WHERE f.root_id=%s AND f.path=%s AND w.stage='index'""", (root, path))
            assert len(work) == 1 and work[0]["status"] == "complete" and work[0]["attempt_count"] == 1
            assert work[0]["mutation_ref"]
            for record in run.chunks(work[0]["mutation_ref"]):
                for row in record["write"]["upsert_rows"]:
                    run.vector_bytes(row["vector"], 768)
            for name, text in expected.items():
                run.assert_source_retained(files[name])
                result = run.request("POST", f"/roots/{root}/read",
                    {"path": name, "lines": {"start": 1, "end": 1}}, key=state["key"])
                assert [line["content"] for line in result["lines"]] == [text.rstrip("\n")]
            for mode in ("fts", "vector", "hybrid"):
                result = run.request("POST", "/query", {"root_id": root, "query": "telescope calibration",
                    "mode": mode, "top_k": 5}, key=state["key"])
                assert {hit["file_path"] for hit in result["results"]} == set(expected)
            print(json.dumps({"event": "embedding_io_verified", "method": method,
                              "network_requests_held": traffic["held_requests"]}), flush=True)
        finally:
            relay("POST", "/release")
