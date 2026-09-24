"""Black-box scenarios. All application writes go through the CLI or HTTP API.

SQL and S3 reads inspect actual durable effects. Tests never invoke worker
processing directly or mutate database state.
"""

import base64
import gzip
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import subprocess
import struct
import socket
import sys
import tempfile
import time
import urllib.error
import urllib.request
import urllib.parse
import uuid

import boto3
import psycopg
from psycopg.rows import dict_row

API = os.environ["PUFFERFS_SERVER_URL"]
BUCKET = os.environ["AWS_BUCKET_NAME"]
STATE = Path("/state/run.json")
REPORT = Path("/artifacts/results.jsonl")
TIMEOUT = int(os.environ.get("PUFFERFS_E2E_TIMEOUT_SECONDS", "3600"))
s3 = boto3.client("s3")


def request(method, path, payload=None, *, key=None, statuses=(200,), server=None, cookie=None, with_status=False):
    data = None if payload is None else json.dumps(payload).encode()
    headers = {"Content-Type": "application/json"}
    if cookie is not None:
        headers["Cookie"] = "pf_session=" + cookie
    else:
        headers["Authorization"] = "Bearer " + (key or os.environ["PUFFERFS_ADMIN_KEY"])
    req = urllib.request.Request((server or API) + path, data=data, method=method, headers=headers)
    try:
        response = urllib.request.urlopen(req, timeout=180)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        body = response.read()
        assert response.status in statuses, f"{method} {path}: HTTP {response.status}"
        result = json.loads(body) if body else None
        return (response.status, result) if with_status else result


def sql(query, args=()):
    with psycopg.connect(os.environ["DATABASE_URL"], row_factory=dict_row,
                         options="-c default_transaction_read_only=on -c statement_timeout=15000") as conn:
        return conn.execute(query, args).fetchall()


def vector_bytes(value, dimensions):
    """Validate either provider wire encoding and return its exact float32 bytes."""
    if isinstance(value, str):
        packed = base64.b64decode(value, validate=True)
    else:
        assert isinstance(value, list) and len(value) == dimensions
        packed = struct.pack(f"<{dimensions}f", *value)
    assert len(packed) == dimensions * 4
    assert all(math.isfinite(number) for number, in struct.iter_unpack("<f", packed))
    return packed



def published_index_filter(root):
    legacy = sql("""SELECT e.id FROM file_catalog f JOIN file_extractions e ON e.id=f.indexed_extraction_id
        WHERE f.root_id=%s AND NOT f.deleted AND e.row_format=1""", (root,))
    segments = sql("""SELECT m.segment_id FROM file_catalog f
        JOIN extraction_segments m ON m.extraction_id=f.indexed_extraction_id
        WHERE f.root_id=%s AND NOT f.deleted""", (root,))
    choices = []
    if legacy:
        choices.append(["extraction_id", "In", [row['id'] for row in legacy]])
    if segments:
        choices.append(["segment_id", "In", [row['segment_id'] for row in segments]])
    return ["Or", choices] if choices else ["extraction_id", "Eq", ""]


def assert_index_vectors(state, root, *, dimensions):
    """Inspect provider state created only through CLI/API capture and workers."""
    namespaces = sql("SELECT namespace FROM root_index_namespaces WHERE root_id=%s AND retired_at IS NULL", (root,))
    extractions = sql("""SELECT e.id,e.chunk_count FROM file_catalog f
        JOIN file_extractions e ON e.id=f.indexed_extraction_id
        WHERE f.root_id=%s AND NOT f.deleted""", (root,))
    publication_filter = published_index_filter(root)
    seen = 0
    for entry in namespaces:
        namespace = entry["namespace"]
        status, metadata = request("GET", f"/v2/namespaces/{namespace}/metadata",
            key=os.environ["TURBOPUFFER_API_KEY"], server=os.environ["TURBOPUFFER_API_URL"],
            statuses=(200, 404), with_status=True)
        if status == 404:
            continue  # Empty allocated namespaces need not exist; the total below must still match.
        schema = metadata["schema"]
        if dimensions is None:
            assert not schema["content"].get("embed") and "vector" not in schema
        else:
            assert schema["content"]["embed"]["model"] == "qwen/qwen3-embedding-8b"
        after = None
        while True:
            filters = [publication_filter]
            if after is not None:
                filters.append(["id", "Gt", after])
            response = request("POST", f"/v2/namespaces/{namespace}/query",
                {"rank_by": ["id", "asc"], "limit": 128,
                 "filters": ["And", filters], "include_attributes": True},
                key=os.environ["TURBOPUFFER_API_KEY"], server=os.environ["TURBOPUFFER_API_URL"])
            rows = response["rows"]
            for row in rows:
                if dimensions is None:
                    assert row.get("vector") is None
                else:
                    vector_bytes(row["vector"], dimensions)
                assert hashlib.sha256(row["content"].encode()).hexdigest() == row["content_hash"]
            seen += len(rows)
            if len(rows) < 128:
                break
            after = rows[-1]["id"]
    assert seen == sum(row["chunk_count"] for row in extractions)
    assert not s3.list_objects_v2(Bucket=BUCKET, Prefix=f"embeddings/{state['org']}/").get("Contents")
    assert sql("SELECT to_regclass('embedding_packs') AS name")[0]["name"] is None
    return seen

def save(state):
    STATE.write_text(json.dumps(state))
    STATE.chmod(0o600)


def cli(state, *args):
    env = dict(os.environ, PUFFERFS_API_KEY=state["key"])
    result = subprocess.run(["pufferfs", *args], env=env, text=True, capture_output=True, timeout=180)
    assert result.returncode == 0, f"CLI {' '.join(args)} failed: {result.stderr[-2000:]}"
    return result.stdout


def catalog(state, root=None):
    root = root or state["root"]
    rows, cursor = {}, ""
    while True:
        page = request("GET", f"/roots/{root}/captured-files?processing=true&limit=127&cursor={cursor}", key=state["key"])
        rows.update((file["path"], file) for file in page["files"])
        cursor = page.get("next_cursor", "")
        if not cursor:
            return rows


def eventually(description, predicate, timeout=TIMEOUT):
    started = time.monotonic()
    next_report = started + 30
    while time.monotonic() - started < timeout:
        result = predicate()
        if result:
            return result
        if time.monotonic() > next_report:
            print(f"Waiting: {description} ({int(time.monotonic()-started)}s)", flush=True)
            next_report += 30
        time.sleep(2)
    raise AssertionError(f"Timed out waiting for {description}")


def wait_indexed(state, root=None):
    def ready():
        files = catalog(state, root)
        return files if files and all(f["version_id"] == f["indexed_version_id"] and
            f["processing"]["status"] == "complete" for f in files.values()) else None
    return eventually("captured versions to become searchable", ready)


def object_json(key):
    options = {"Bucket": BUCKET, "Key": key}
    packed = ".jsonl#" in key
    if packed:
        options["Key"], locator = key.rsplit("#", 1)
        offset, length, digest = locator.split(":")
        offset, length = int(offset), int(length)
        assert offset >= 0 and length > 0
        options["Range"] = f"bytes={offset}-{offset+length-1}"
    with s3.get_object(**options)["Body"] as body:
        raw = body.read()
    if packed:
        assert len(raw) == length and hashlib.sha256(raw).hexdigest() == digest
    return json.loads(raw)


def chunks(key):
    with s3.get_object(Bucket=BUCKET, Key=key)["Body"] as body, gzip.GzipFile(fileobj=body) as stream:
        first = next(stream, None)
        if first is None:
            return
        record = json.loads(first)
        if record.get('kind') == 'segment_manifest':
            assert record['format'] == 2
            position = 0
            owner = '/'.join(key.split('/')[:3])+'/'
            for line in stream:
                segment = json.loads(line)
                assert segment['ordinal_start'] == position and 1 <= segment['chunk_count'] <= 64
                assert segment['chunks_ref'].startswith(owner) and '/segments/' in segment['chunks_ref']
                seen = 0
                for chunk in chunks(segment['chunks_ref']):
                    assert chunk['chunk_index'] == position
                    seen += 1
                    position += 1
                    yield chunk
                assert seen == segment['chunk_count']
            assert position == record['chunk_count']
        else:
            yield record
            for line in stream:
                yield json.loads(line)


def assert_source_retained(file):
    manifest = object_json(file["source_manifest_ref"])
    digest = hashlib.sha256()
    for extent in manifest.get("extents") or []:
        start, size = extent["offset"], extent["length"]
        with s3.get_object(Bucket=BUCKET, Key=extent["object_key"], Range=f"bytes={start}-{start+size-1}")["Body"] as body:
            while block := body.read(65536):
                digest.update(block)
    assert "sha256:" + digest.hexdigest() == file["content_hash"]
    assert manifest["content_hash"] == file["content_hash"] and manifest["size"] == file["size"]
    return manifest


def fault(method, path, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request("http://faults:8474" + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as response:
        body = response.read()
        return json.loads(body) if body else None


def multipart_recovery():
    state = json.loads(STATE.read_text()) if STATE.exists() else provision()
    for mode in ("active", "expired", "completed"):
        directory = Path(f"/state/multipart-{mode}-" + uuid.uuid4().hex[:8])
        directory.mkdir()
        path = directory / "recovery.txt"
        # An exact production-sized 32 MiB pack with two 16 MiB parts.
        source_bytes = 32 << 20
        line = b"Captured orchid observatory log.".ljust(63, b" ") + b"\n"
        with path.open("wb") as output:
            for _ in range(source_bytes // (len(line) * 1024)):
                output.write(line * 1024)
        with path.open("rb") as source:
            expected = "sha256:" + hashlib.file_digest(source, "sha256").hexdigest()
        root = new_root(state, directory.name, directory, True)
        state.setdefault("recovery_roots", []).append(root)
        save(state)
        fault("POST", "/proxies/source-upload/toxics", {"name": "slow-upload", "type": "bandwidth",
              "stream": "upstream", "attributes": {"rate": 2048}})
        # Real TCP throttling makes the interruption repeatable without timing
        # hooks in production code. Poll the durable journal, not Go internals.
        with tempfile.TemporaryFile(mode="w+") as log:
            process = subprocess.Popen(["pufferfs", "sync", str(directory), "--id", root, "--no-vector"],
                env=dict(os.environ, PUFFERFS_API_KEY=state["key"], PUFFERFS_UPLOAD_CONCURRENCY="1"), stdout=log, stderr=log, text=True)
            observed = None
            try:
                deadline = time.monotonic() + 90
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        log.seek(0)
                        raise AssertionError("CLI exited before multipart interruption: " + log.read()[-2000:])
                    for journal_path in Path("/root/.tpfs/roots", root).glob("file-capture-*/pending/*/journal.json"):
                        journal = json.loads(journal_path.read_text())
                        for pack in journal["packs"]:
                            upload = pack.get("multipart") or {}
                            if len(upload.get("parts", [])) == 1 and not pack["complete"]:
                                observed = journal, pack
                                break
                        if observed:
                            break
                    if observed:
                        break
                    time.sleep(0.02)
                assert observed, "first part acknowledgement never reached the durable journal"
            finally:
                if process.poll() is None:
                    process.kill()
                process.wait(timeout=10)
                fault("DELETE", "/proxies/source-upload/toxics/slow-upload")
        journal, pack = observed
        upload = pack["multipart"]
        if mode == "expired":
            # Abort through S3, the same externally observable condition as an
            # incomplete-upload lifecycle expiry. No database state is changed.
            s3.abort_multipart_upload(Bucket=BUCKET, Key=pack["object_key"], UploadId=upload["upload_id"])
        elif mode == "active":
            parts = s3.list_parts(Bucket=BUCKET, Key=pack["object_key"], UploadId=upload["upload_id"])["Parts"]
            assert parts[0]["ETag"] == upload["parts"][0]["etag"]
            # A network outage must not be interpreted as an expired session.
            fault("POST", "/proxies/source-upload", {"enabled": False})
            try:
                failed = subprocess.run(["pufferfs", "sync", str(directory), "--id", root, "--no-vector"],
                    env=dict(os.environ, PUFFERFS_API_KEY=state["key"]), text=True, capture_output=True, timeout=45)
                assert failed.returncode != 0 and "HTTP 503" in failed.stderr
                assert json.loads(journal_path.read_text())["packs"][0] == pack, "outage discarded durable upload identity"
            finally:
                fault("POST", "/proxies/source-upload", {"enabled": True})
        else:
            # Finish through public upload APIs, then drop the caller connection
            # after S3 accepted completion but before the API can acknowledge it.
            part = request("POST", f"/roots/{root}/sources/multipart/part",
                           {"object_key": pack["object_key"], "part_number": 2}, key=state["key"])
            with (journal_path.parent / pack["name"]).open("rb") as source:
                source.seek(16 << 20)
                data = source.read(16 << 20)
            headers = {key: values[0] for key, values in part["headers"].items()}
            with urllib.request.urlopen(urllib.request.Request(part["url"], data=data, method="PUT", headers=headers), timeout=30) as response:
                etag = response.headers["ETag"]
            parts = upload["parts"] + [{"part_number": 2, "etag": etag}]
            fault("POST", "/proxies/source-upload/toxics", {"name": "delay-completion", "type": "latency",
                  "stream": "downstream", "attributes": {"latency": 5000, "jitter": 0}})
            connection = http.client.HTTPConnection(urllib.parse.urlsplit(API).netloc, timeout=15)
            try:
                connection.request("POST", f"/roots/{root}/sources/multipart/complete",
                    body=json.dumps({"object_key": pack["object_key"], "parts": parts}),
                    headers={"Authorization": "Bearer " + state["key"], "Content-Type": "application/json"})
                def object_completed():
                    return bool(s3.list_objects_v2(Bucket=BUCKET, Prefix=pack["object_key"]).get("Contents"))
                eventually("S3 completion before the delayed API acknowledgement", object_completed, 4)
                connection.sock.shutdown(socket.SHUT_RDWR)
                connection.close()
                # Let the delayed response window end; a canceled HTTP request
                # must not have durably acknowledged completion in Postgres.
                time.sleep(6)
                stored = sql("""SELECT o.completed_at,m.completion_parts FROM source_objects o
                    JOIN source_multipart_uploads m USING(object_key) WHERE object_key=%s""", (pack["object_key"],))[0]
                assert stored["completed_at"] is None and stored["completion_parts"] == parts
            finally:
                connection.close()
                fault("DELETE", "/proxies/source-upload/toxics/delay-completion")
        path.write_text("Live file rewritten while the capture agent was stopped.\n")
        cli(state, "sync", str(directory), "--id", root, "--no-vector")
        versions = sql("""SELECT v.* FROM file_versions v JOIN file_catalog f ON f.id=v.file_id
            WHERE f.root_id=%s AND f.path='recovery.txt' ORDER BY v.sequence""", (root,))
        assert len(versions) == 2, "resume must register captured bytes before the new live version"
        original = versions[0]
        assert original["capture_id"] == journal["request"]["capture_id"] and original["content_hash"] == expected
        manifest = object_json(original["source_manifest_ref"])
        assert manifest["size"] == source_bytes and manifest["content_hash"] == expected
        digest = hashlib.sha256()
        for extent in manifest["extents"]:
            start, length = extent["offset"], extent["length"]
            with s3.get_object(Bucket=BUCKET, Key=extent["object_key"], Range=f"bytes={start}-{start+length-1}")["Body"] as body:
                while block := body.read(65536):
                    digest.update(block)
        assert "sha256:" + digest.hexdigest() == expected, "resume reread the mutated path instead of captured bytes"
        completed = next(p for p in Path("/root/.tpfs/roots", root).glob("file-capture-*/completed/*/journal.json")
                         if json.loads(p.read_text())["request"]["capture_id"] == original["capture_id"])
        recovered = json.loads(completed.read_text())["packs"][0]
        if mode == "active":
            assert len(recovered["multipart"]["parts"]) == source_bytes // (16 << 20)
        if mode == "expired":
            assert recovered["object_key"] != pack["object_key"]
            assert recovered["multipart"]["request_id"] != upload["request_id"]
        else:
            assert recovered["object_key"] == pack["object_key"]
            assert recovered["multipart"]["upload_id"] == upload["upload_id"]
            assert recovered["multipart"]["parts"][0] == upload["parts"][0]
        print(f"Multipart {mode}: retained capture bytes and journal identity verified.")


def provision():
    nonce = uuid.uuid4().hex
    org = request("POST", "/admin/orgs", {"name": "Synthetic Compose E2E", "slug": "e2e-" + nonce})
    state = {"org": org["id"], "nonce": nonce, "roots": [], "users": []}
    save(state)  # Preserve cleanup identity before creating any external index.
    for role in ("owner", "viewer"):
        user = request("POST", "/admin/users", {"email": f"{role}-{nonce}@example.invalid", "name": role})
        state["users"].append(user["id"])
        save(state)
        request("PUT", f"/admin/orgs/{org['id']}/members/{user['id']}", {"role": role})
        key = request("POST", f"/admin/orgs/{org['id']}/users/{user['id']}/api-keys",
                      {"name": "e2e", "scopes": ["query", "sync", "root:delete"]}, statuses=(201,))["key"]
        state["key" if role == "owner" else "outsider_key"] = key
        save(state)
    return state


def new_root(state, name, path, no_vector):
    root = request("POST", "/roots", {"name": name, "source_path": str(path), "scope": "user",
                   "vector_disabled": no_vector}, key=state["key"], statuses=(201,))
    state["roots"].append(root["id"])
    save(state)
    return root["id"]


def capture():
    from fixtures import create
    state = json.loads(STATE.read_text()) if STATE.exists() else provision()
    directory = Path("/state/workspace")
    state["files"], state["fixture_expectations"] = create(directory)
    state["root"] = new_root(state, "e2e-files", directory, True)
    save(state)
    cli(state, "sync", str(directory), "--id", state["root"], "--no-vector")
    files = catalog(state)
    assert set(files) == set(state["files"]), "CLI captured wrong paths or missed catalog pagination"
    assert all(not file["indexed_version_id"] for file in files.values()), "workers must be stopped during capture"
    state["before"] = files
    save(state)
    # Catalog/state is real even though execution workers are stopped.
    result = request("POST", "/query", {"query": "Orchid", "root_id": state["root"], "mode": "fts", "top_k": 10}, key=state["key"])
    assert not result.get("results"), "unindexed capture was visible"
    pending = work_rows(state["root"])
    assert len(pending) == len(files) and all(row["status"] == "pending" for row in pending)
    assert sql("SELECT count(*) n FROM source_multipart_uploads")[0]["n"] > 0, "large source did not use multipart"
    print(f"Captured {len(files)} synthetic files across 100 directories with execution workers stopped.")


def work_rows(root, stage="transform"):
    return sql("""SELECT w.id,w.stage,w.status,w.next_attempt_at,w.attempt_count,w.extraction_id,
        e.status extraction_status,e.chunks_ref,e.chunk_count,
        v.id version_id,f.id file_id,f.path,f.indexed_version_id,f.root_id,r.org_id FROM file_work w
        JOIN file_extractions e ON e.id=w.extraction_id JOIN file_versions v ON v.id=e.version_id
        JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
        WHERE f.root_id=%s AND w.stage=%s ORDER BY w.id""", (root, stage))


def native_capture():
    from fixtures import create_native
    state = json.loads(STATE.read_text()) if STATE.exists() else provision()
    directory = Path("/state/native-" + uuid.uuid4().hex[:8])
    state["native_files"], state["native_sheets"] = create_native(directory)
    state["native_directory"] = str(directory)
    state["native_root"] = new_root(state, "e2e-native-" + uuid.uuid4().hex[:8], directory, True)
    save(state)
    cli(state, "sync", str(directory), "--id", state["native_root"], "--no-vector")
    assert set(catalog(state, state["native_root"])) == set(state["native_files"])
    assert not sql("SELECT id FROM provider_batches LIMIT 1")
    assert all(row["status"] == "pending" for row in work_rows(state["native_root"]))
    # No generation pipeline can accept uploads or create background root jobs.
    for endpoint in ("upload", "upload-bundle", "upload/multipart/init", "sync", "sync/init"):
        req = urllib.request.Request(API + f"/roots/{state['native_root']}/{endpoint}",
            data=b"{}", method="POST", headers={"Authorization": "Bearer " + state["key"],
                                              "Content-Type": "application/json"})
        try:
            urllib.request.urlopen(req, timeout=10).close()
            raise AssertionError(f"retired ingestion endpoint still accepts requests: {endpoint}")
        except urllib.error.HTTPError as error:
            assert error.code == 404
            error.close()
    for name in ("sync_jobs", "sync_generations", "sync_job_shards", "root_states",
                 "embedding_cache", "embedding_locations", "embedding_packs", "content_proofs"):
        assert sql("SELECT to_regclass(%s) AS table_name", (name,))[0]["table_name"] is None
    current = subprocess.run(["pufferfs", "root", "current", "--json"], cwd=directory,
        env=dict(os.environ, PUFFERFS_API_KEY=state["key"]), capture_output=True, text=True, timeout=30)
    assert current.returncode == 0, current.stderr
    identity = json.loads(current.stdout)
    assert identity["id"] == state["native_root"]
    assert identity["name"] == request("GET", f"/roots/{state['native_root']}", key=state["key"])["name"]
    print(f"Captured {len(state['native_files'])} native-format fixtures with execution workers stopped; retired endpoints absent, root identity persisted.")


def native_transformed():
    state = json.loads(STATE.read_text())
    root, directory = state["native_root"], Path(state["native_directory"])
    def ready():
        rows = work_rows(root, "index")
        return rows if len(rows) == len(state["native_files"]) and all(
            r["status"] == "pending" and r["extraction_status"] == "complete" for r in rows) else None
    rows = eventually("native files to become durable chunks through the ingestion worker", ready, 180)
    assert all(r["attempt_count"] == 0 and not r["indexed_version_id"] for r in rows)
    signatures = {}
    total_chunks = 0
    for row in rows:
        path = row["path"]
        records = list(chunks(row["chunks_ref"]))
        assert len(records) == row["chunk_count"], path
        assert [c["chunk_index"] for c in records] == list(range(len(records))), path
        assert all(len(c["content"].encode()) <= 6000 and
                   hashlib.sha256(c["content"].encode()).hexdigest() == c["content_hash"] for c in records), path
        total_chunks += len(records)
        signatures[path] = records
        if path.endswith((".txt", ".jsonl")):
            position, line = 0, 1
            with (directory / path).open("rb") as source:
                for record in records:
                    data = record["content"].encode()
                    assert data == source.read(len(data)), f"source bytes changed: {path}"
                    end_line = max(line, line + data.count(b"\n") - int(data.endswith(b"\n")))
                    assert record["location"] == {"byte_start": position, "byte_end": position + len(data),
                                                  "line_start": line, "line_end": end_line}, path
                    position += len(data)
                    line += data.count(b"\n")
                assert source.read(1) == b"", f"missing source bytes: {path}"
        elif path in state["native_sheets"]:
            sheets = {}
            for record in records:
                location = record["location"]
                assert 1 <= location["row_start"] <= location["row_end"], path
                assert record["content"].startswith("First populated row (context):\n"), path
                sheet = location["sheet"]
                sheets[sheet] = sheets.get(sheet, "") + record["content"].split("\nCells:\n", 1)[1]
            assert sheets == state["native_sheets"][path], f"cell content/addresses changed: {path}"
        else:
            assert "Orchid" in "".join(c["content"] for c in records), path
            assert all(c["location"]["record_number"] == 0 for c in records), path
    assert signatures["records.jsonl"] == signatures["sessions/rollout.jsonl"], "filename changed generic JSONL behavior"
    assert not sql("SELECT id FROM provider_batches LIMIT 1"), "native input submitted Gemini work"
    assert not s3.list_objects_v2(Bucket=BUCKET, Prefix="mutations/").get("Contents")
    print(f"Native transformation produced {total_chunks} verified chunks for {len(rows)} files; publication is pending.")


def follow_backlog():
    state = json.loads(STATE.read_text()) if STATE.exists() else provision()
    directory = Path("/state/follow-" + uuid.uuid4().hex[:8])
    directory.mkdir()
    root = new_root(state, directory.name, directory, True)
    state["follow_root"], state["follow_versions"] = root, []
    save(state)
    source = directory / "activity.jsonl"
    initial = b''.join((json.dumps({"record": i, "text": "Orchid live telemetry"}) + "\n").encode()
                       for i in range(2000))
    suffix = b'{"record":2000,"text":"Orchid appended while indexing is pending"}\n'
    replacement = b'{"record":"replacement","text":"Cobalt rewrite"}\n'
    with tempfile.TemporaryFile() as output:
        process = subprocess.Popen(["pufferfs", "sync", str(directory), "--id", root, "--no-vector", "--follow"],
            env=dict(os.environ, PUFFERFS_API_KEY=state["key"]), stdin=subprocess.DEVNULL,
            stdout=output, stderr=subprocess.STDOUT)
        def alive():
            assert process.poll() is None, "sync --follow exited while indexing was pending"
        def watching():
            alive()
            return b"Following " in os.pread(output.fileno(), 65536, 0)
        def captured(data, deleted=False):
            previous = state["follow_versions"][-1]["version_id"] if state["follow_versions"] else None
            expected_hash = "" if deleted else "sha256:" + hashlib.sha256(data).hexdigest()
            def ready():
                alive()
                file = catalog(state, root).get("activity.jsonl")
                if (file and file["version_id"] != previous and file["deleted"] == deleted
                        and file["content_hash"] == expected_hash and file["size"] == len(data)
                        and file["processing"]["stage"] == "index" and file["processing"]["status"] == "pending"):
                    assert not file["indexed_version_id"]
                    return file
                return None
            file = eventually("live follow to capture and transform the next version without indexing", ready, 90)
            state["follow_versions"].append(file)
            save(state)
            status = json.loads(cli(state, "sync", "status", "--root", root, "--json"))
            assert status["total"] == 1 and status["status"] == "processing" and status["states"] == {"pending": 1}
            return file
        try:
            # Use normal debounce and wait for real watch readiness, not a sleep
            # that might race the initial scan/watcher installation.
            eventually("filesystem watch to start", watching, 60)
            source.write_bytes(initial)
            first = captured(initial)
            with source.open("ab") as target:
                target.write(suffix)
            appended = captured(initial + suffix)
            before, after = assert_source_retained(first), assert_source_retained(appended)
            assert after["extents"][:len(before["extents"])] == before["extents"]
            assert sum(extent["length"] for extent in after["extents"][len(before["extents"]):]) == len(suffix)
            source.write_bytes(replacement)
            rewritten = captured(replacement)
            assert assert_source_retained(rewritten)["extents"] != after["extents"], "rewrite retained stale source extents"
            source.write_bytes(b"")
            empty = captured(b"", deleted=True)
            assert not empty["source_manifest_ref"]
            source.write_bytes(replacement)
            captured(replacement)
            source.unlink()
            deleted = captured(b"", deleted=True)
            assert not deleted["source_manifest_ref"]
            alive()
        finally:
            if process.poll() is None:
                process.terminate()
            try:
                exit_code = process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
                raise AssertionError("sync --follow did not stop after SIGTERM")
        assert exit_code == 0, "sync --follow did not stop cleanly"

    history = sql("""SELECT v.id,v.previous_version_id,v.file_id FROM file_versions v
        JOIN file_catalog f ON f.id=v.file_id WHERE f.root_id=%s ORDER BY v.sequence""", (root,))
    ids = [file["version_id"] for file in state["follow_versions"]]
    assert [v["id"] for v in history] == ids and len(history) == 6
    assert [v["previous_version_id"] for v in history] == [None] + ids[:-1]
    assert len({v["file_id"] for v in history}) == 1
    # File deletion and successful captures must not remove earlier originals.
    for file in state["follow_versions"]:
        if not file["deleted"]:
            assert_source_retained(file)
    jobs = sql("""SELECT w.* FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
        JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id WHERE f.root_id=%s""", (root,))
    assert len(jobs) == 6 and len({row["extraction_id"] for row in jobs}) == 6
    print("One live agent captured create/append/rewrite/truncate/restore/delete as six linked versions; originals retained.")


def wait_work_idle(stage):
    def idle():
        return not sql("SELECT id FROM file_work WHERE stage=%s AND status IN ('pending','running') LIMIT 1", (stage,))
    eventually(f"{stage} work to finish", idle)

def native_published():
    state = json.loads(STATE.read_text())
    wait_indexed(state, state["native_root"])
    result = request("POST", f"/roots/{state['native_root']}/read",
                     {"path": "unicode.txt", "lines": {"start": 1, "end": 3}}, key=state["key"])
    expected = (Path(state["native_directory"]) / "unicode.txt").read_bytes().decode().split("\n")
    assert [line["content"] for line in result["lines"]] == expected, "read did not reassemble split UTF-8 lines"
    result = request("POST", "/query", {"root_id": state["native_root"], "query": "Orchid",
                     "mode": "fts", "top_k": 5}, key=state["key"])
    assert result["results"], "native-format root was not searchable"

    print("Native files are published, searchable and readable.", flush=True)


def verify():
    state = json.loads(STATE.read_text())
    files = wait_indexed(state)
    for root in state.get("recovery_roots", []):
        wait_indexed(state, root)
        result = request("POST", f"/roots/{root}/read", {"path": "recovery.txt", "lines": {"start": 1, "end": 1}}, key=state["key"])
        assert "Live file rewritten" in result["lines"][0]["content"]
    if state.get("handoff_root"):
        wait_indexed(state, state["handoff_root"])
        result = request("POST", f"/roots/{state['handoff_root']}/read",
                         {"path": "file-00.txt", "lines": {"start": 1, "end": 1}}, key=state["key"])
        assert "Recoverable orchid telemetry 0" in result["lines"][0]["content"]
    if state.get("native_root"):
        native_published()
    if state.get("follow_root"):
        followed = wait_indexed(state, state["follow_root"])
        assert followed["activity.jsonl"]["deleted"]
        request("POST", f"/roots/{state['follow_root']}/read",
                {"path": "activity.jsonl", "lines": {"start": 1, "end": 1}}, key=state["key"], statuses=(404,))
    cli(state, "sync", "/state/workspace", "--id", state["root"], "--no-vector")
    assert catalog(state) == files, "unchanged sync produced work or changed publication"
    rows = sql("""SELECT f.path,e.id AS extraction_id,e.chunks_ref,e.chunk_count,w.status AS work_status
        FROM file_catalog f JOIN file_extractions e ON e.id=f.indexed_extraction_id
        JOIN file_work w ON w.extraction_id=e.id AND w.stage='index' WHERE f.root_id=%s""", (state["root"],))
    assert len(rows) == len(files)
    assert all(row["work_status"] == "complete" for row in rows)
    batches = sql("SELECT * FROM provider_batches WHERE root_id=%s", (state["root"],))
    assert batches, "provider-backed fixtures did not produce batch results"
    packed_results = {}
    for batch in batches:
        assert batch["status"] == "complete"
        for result in provider_records(batch):
            assert result["status"] == "complete" and not result["error"] and result["result_ref"]
            packed_results.setdefault(result["result_ref"], set()).add(result["request_key"])
    for ref, expected_keys in packed_results.items():
        assert {chunk["request_key"] for chunk in chunks(ref)} == expected_keys
    for row in rows:
        path = row["path"]
        expected = state["fixture_expectations"].get(path, {})
        missing = {word.lower() for word in expected.get("terms", [])}
        count, excerpt, locations = 0, "", []
        # Stream every extraction, regardless of name, directory or size. Only
        # bounded diagnostic text is retained; fixture expectations are data.
        for chunk in chunks(row["chunks_ref"]):
            assert chunk["chunk_index"] == count
            assert hashlib.sha256(chunk["content"].encode()).hexdigest() == chunk["content_hash"]
            count += 1
            content = chunk["content"].lower()
            missing = {word for word in missing if word not in content}
            excerpt += chunk["content"][:max(0, 2000 - len(excerpt))]
            if len(locations) < 10:
                locations.append(chunk["location"])
            if expected.get("diarized"):
                location = chunk["location"]
                assert location.get("speaker") and location.get("speaker_scope")
                assert 0 <= location["start_seconds"] <= location["end_seconds"]
        assert count == row["chunk_count"]
        if expected.get("diarized"):
            assert count > 0
        if missing:
            jobs = sql("""SELECT provider_job_id FROM provider_batches WHERE extraction_id=%s""", (row["extraction_id"],))
            print(json.dumps({"failed_fixture": path, "missing_terms": sorted(missing),
                "content_excerpt": excerpt, "chunk_count": count, "locations": locations,
                "provider_jobs": [job["provider_job_id"] for job in jobs]}), flush=True)
            raise AssertionError(f"missing extracted content: {path}")
        assert row["chunks_ref"], f"publication has no canonical text artifact: {path}"
    # Verify captured bytes from their actual S3 ranges, independently of the worker.
    for file in files.values():
        assert_source_retained(file)
    response = json.loads(cli(state, "read", "sample.pdf", "--root", state["root"], "--pages", "1:2", "--json"))
    assert len(response["pages"]) == 2
    response = json.loads(cli(state, "query", "Orchid observatory", "--root", state["root"], "--mode", "fts", "--json"))
    assert response["results"]
    for endpoint, method, payload in [(f"/roots/{state['root']}/captured-files", "GET", None),
        (f"/roots/{state['root']}/read", "POST", {"path": "rewrite.txt", "lines": {"start": 1, "end": 1}})]:
        request(method, endpoint, payload, key=state["outsider_key"], statuses=(404,))
    # No rendered pages or converted clips in durable object storage.
    objects = [item for page in s3.get_paginator("list_objects_v2").paginate(Bucket=BUCKET) for item in page.get("Contents", [])]
    assert not any(item["Key"].lower().endswith((".png", ".jpg", ".wav", ".mp4", ".pdf")) for item in objects)
    assert_index_vectors(state, state["root"], dimensions=None)
    vector_dir = Path("/state/vector")
    vector_dir.mkdir()
    (vector_dir / "astronomy.txt").write_text("An observatory uses a telescope to study stars and galaxies.\n")
    (vector_dir / "gardening.txt").write_text("Garden soil needs compost and watering for vegetables.\n")
    state["vector_root"] = new_root(state, "e2e-vector", vector_dir, False)
    save(state)
    cli(state, "sync", str(vector_dir), "--id", state["vector_root"])
    wait_indexed(state, state["vector_root"])
    for mode in ("vector", "hybrid"):
        result = request("POST", "/query", {"root_id": state["vector_root"], "query": "astronomy telescope stars",
                         "mode": mode, "top_k": 1}, key=state["key"])
        assert result["results"][0]["file_path"] == "astronomy.txt"
    assert_index_vectors(state, state["vector_root"], dimensions=4096)
    state["before"] = files
    save(state)
    authorization()
    provider_cleanup()


def provider_manifest(ref):
    # Independent read-only assertion, not an application helper invocation.
    import re
    match = re.fullmatch(r"maintenance/provider/[0-9a-f]{64}/(input|result|cleanup)/([0-9a-f]{64})\.json", ref)
    assert match, "noncanonical provider manifest reference"
    with s3.get_object(Bucket=BUCKET, Key=ref)["Body"] as body:
        raw = body.read(4 * 1024 * 1024 + 1)
    assert len(raw) <= 4 * 1024 * 1024
    assert hashlib.sha256(raw).hexdigest() == match[2]
    value = json.loads(raw)
    assert value["format"] == 1 and value["kind"] == match[1]
    return value


def provider_records(batch):
    manifest = provider_manifest(batch["output_ref"] or batch["input_ref"])
    if manifest["attempt"] != batch["attempt_count"]:
        manifest = provider_manifest(batch["input_ref"])
    records = manifest["requests"]
    assert len(records) == batch["request_count"] <= 64
    assert [r["ordinal"] for r in records] == list(range(batch["ordinal_start"], batch["ordinal_start"] + len(records)))
    return [dict(item, batch_id=batch["id"], extraction_id=batch["extraction_id"]) for item in records]


def provider_uploads(batch):
    uploads, ref = {}, batch["input_ref"]
    seen = set()
    while ref:
        assert ref not in seen
        seen.add(ref)
        manifest = provider_manifest(ref)
        assert 1 <= len(manifest["uploads"]) <= 65
        for item in manifest["uploads"]:
            assert item["file_id"] not in uploads, "retry reused a temporary upload"
            uploads[item["file_id"]] = dict(item, deleted_at=None, expired_at=None, error="")
        ref = manifest["previous"]
    return uploads


def provider_cleanup(*, externally_deleted=None):
    from datetime import datetime, timezone
    batches = sql("SELECT * FROM provider_batches WHERE status IN ('complete','failed')")
    assert batches, "no completed provider batches"
    owned = {}
    for batch in batches:
        owned.update(provider_uploads(batch))
    assert owned
    externally_deleted = externally_deleted or {}
    assert set(externally_deleted) <= set(owned)

    def settled():
        outcomes = {}
        for batch in sql("SELECT * FROM provider_batches WHERE status IN ('complete','failed')"):
            ref, seen = batch["cleanup_ref"], set()
            while ref:
                assert ref not in seen
                seen.add(ref)
                checkpoint = provider_manifest(ref)
                assert len(checkpoint["files"]) <= 65
                for file in checkpoint["files"]:
                    outcomes.setdefault(file["file_id"], file)
                ref = checkpoint["previous"]
        for file_id, upload in owned.items():
            if file_id not in outcomes:
                return None
            file = outcomes[file_id]
            assert file["expires_at"] == upload["expires_at"]
            expiry_due = datetime.fromisoformat(file["expires_at"]) <= datetime.now(timezone.utc)
            if file_id in externally_deleted:
                assert datetime.fromisoformat(file["expires_at"]) == datetime.fromisoformat(externally_deleted[file_id])
            if file["deleted_at"]:
                continue
            if file["expired_at"]:
                assert expiry_due
                continue
            if file_id in externally_deleted and file["error"] == "ClientError status=403":
                assert not expiry_due
                continue
            return None
        return [outcomes[file_id] for file_id in owned]

    files = eventually("S3 batch cleanup checkpoints", settled, timeout=600)
    from google import genai
    with genai.Client(api_key=os.environ["GEMINI_API_KEY"], http_options={"retry_options": {"attempts": 1}}) as client:
        for file_id in owned:
            try:
                client.files.get(name=file_id)
            except Exception as error:
                assert getattr(error, "code", None) in {403, 404}
            else:
                raise AssertionError("provider still serves a settled upload")
    for table in ("provider_requests", "provider_files", "provider_batch_files"):
        assert sql("SELECT to_regclass(%s) AS relation", (table,))[0]["relation"] is None
    print(json.dumps({"provider_upload_cleanup": {
        "acknowledged_deletions": sum(bool(file["deleted_at"]) for file in files),
        "provider_deadlines_passed": sum(bool(file["expired_at"]) and not file["deleted_at"] for file in files),
        "external_deletions_pending_expiry": sum(not file["deleted_at"] and not file["expired_at"] for file in files)}}), flush=True)


def authorization():
    state = json.loads(STATE.read_text())
    root, owner = state["native_root"], state["users"][0]
    before = wait_indexed(state, root)
    work_before = work_rows(root) + work_rows(root, "index")
    path = "sessions/rollout.jsonl"
    file = before[path]
    # Provision through the real admin API. Cleanup of this synthetic user/org
    # also revokes this key; never print it or persist it in test artifacts.
    admin_key = request("POST", f"/admin/orgs/{state['org']}/users/{owner}/api-keys",
        {"name": "e2e-acl", "scopes": ["acl:read", "acl:write"]}, statuses=(201,))["key"]
    read_path = f"/roots/{root}/read"
    read_body = {"path": path, "lines": {"start": 1, "end": 1}}
    query = {"root_id": root, "query": "Orchid", "glob": path, "mode": "fts", "top_k": 5}
    for label, target in (("bare user", owner), ("user prefix", "user:" + owner), ("role", "role:owner"), ("wildcard", "*")):
        print(f"Checking {label} ACL against an already published file.", flush=True)
        assert request("POST", "/query", query, key=state["key"])["results"]
        request("POST", read_path, read_body, key=state["key"])
        acl = request("POST", f"/roots/{root}/acls",
            {"path_prefix": "/sessions/", "grant_to": target, "permission": "none"},
            key=admin_key, statuses=(201,))
        try:
            request("POST", read_path, read_body, key=state["key"], statuses=(404,))
            assert not request("POST", "/query", query, key=state["key"])["results"], "ACL leaked indexed content"
            assert set(catalog(state, root)) == set(before) - {path}, "ACL leaked catalog entries"
            request("POST", read_path, {"path": "unicode.txt", "lines": {"start": 1, "end": 1}}, key=state["key"])
            request("POST", f"/roots/{root}/captured-proofs", {"files": [
                {"path": path, "version_id": file["version_id"], "content_hash": file["content_hash"]}
            ]}, key=state["key"], statuses=(403,))
        finally:
            request("DELETE", f"/roots/{root}/acls/{acl['id']}", key=admin_key)
        request("POST", read_path, read_body, key=state["key"])
        assert request("POST", "/query", query, key=state["key"])["results"], "ACL removal did not restore access"
        assert catalog(state, root) == before, "ACL change mutated file publication"
    assert work_rows(root) + work_rows(root, "index") == work_before, "ACL changes unexpectedly reprocessed files"
    print("Raw-user, user-prefixed, role and wildcard denies hide published reads, FTS and catalog; removal restores access without reindexing.")


def outage():
    state = json.loads(STATE.read_text())
    directory = Path("/state/workspace")
    removed = {"remove.txt": "saffron", "move.txt": "cedar"}
    for path, word in removed.items():
        result = request("POST", "/query", {"root_id": state["root"], "query": word,
            "glob": path, "mode": "fts", "top_k": 5}, key=state["key"])
        assert result["results"], "deletion visibility fixture was not searchable before capture"
    with (directory / "append.jsonl").open("a") as output:
        output.write('{"record":2000,"text":"Appended violet telemetry"}\n')
    (directory / "rewrite.txt").write_text("Replacement amber description.\n")
    (directory / "remove.txt").unlink()
    (directory / "move.txt").rename(directory / "moved.txt")
    cli(state, "sync", str(directory), "--id", state["root"], "--no-vector")
    files = catalog(state)
    assert files["rewrite.txt"]["version_id"] != state["before"]["rewrite.txt"]["version_id"]
    assert files["rewrite.txt"]["indexed_version_id"] == state["before"]["rewrite.txt"]["indexed_version_id"]
    result = request("POST", f"/roots/{state['root']}/read", {"path": "rewrite.txt", "lines": {"start": 1, "end": 1}}, key=state["key"])
    assert "Original cobalt" in result["lines"][0]["content"]
    result = request("POST", "/query", {"root_id": state["root"], "query": "cobalt",
        "glob": "rewrite.txt", "mode": "fts", "top_k": 5}, key=state["key"])
    assert result["results"] and all("Original cobalt" in row["content"] for row in result["results"])
    for path, word in removed.items():
        request("POST", f"/roots/{state['root']}/read", {"path": path, "lines": {"start": 1, "end": 1}},
            key=state["key"], statuses=(404,))
        result = request("POST", "/query", {"root_id": state["root"], "query": word,
            "glob": path, "mode": "fts", "top_k": 5}, key=state["key"])
        assert not result["results"], "capture-time deletion leaked a previous indexed version"
    old = object_json(state["before"]["append.jsonl"]["source_manifest_ref"])
    new = object_json(files["append.jsonl"]["source_manifest_ref"])
    assert new["extents"][:len(old["extents"])] == old["extents"], "append must reuse its verified prefix"
    state["after"] = files
    save(state)


def resumed():
    state = json.loads(STATE.read_text())
    files = wait_indexed(state)
    assert files["remove.txt"]["deleted"] and files["move.txt"]["deleted"]
    assert not files["moved.txt"]["deleted"]
    for path in ("remove.txt", "move.txt"):
        request("POST", f"/roots/{state['root']}/read", {"path": path, "lines": {"start": 1, "end": 1}}, key=state["key"], statuses=(404,))
    response = request("POST", f"/roots/{state['root']}/read", {"path": "rewrite.txt", "lines": {"start": 1, "end": 1}}, key=state["key"])
    assert "Replacement amber" in response["lines"][0]["content"]
    for word, expected in (("cobalt", False), ("amber", True)):
        result = request("POST", "/query", {"root_id": state["root"], "query": word,
            "glob": "rewrite.txt", "mode": "fts", "top_k": 5}, key=state["key"])
        assert bool(result["results"]) == expected, "search returned the wrong published revision"
    for path, file in state["before"].items():
        if path not in {"append.jsonl", "rewrite.txt", "move.txt", "remove.txt"}:
            assert files[path]["version_id"] == file["version_id"], f"unchanged file recaptured: {path}"
    before = sql("SELECT id,attempt_count,status FROM file_work ORDER BY id")
    cli(state, "sync", "/state/workspace", "--id", state["root"], "--no-vector")
    assert before == sql("SELECT id,attempt_count,status FROM file_work ORDER BY id")


def cleanup():
    if not STATE.exists():
        return
    state = json.loads(STATE.read_text())
    # Cancel unfinished jobs owned by this isolated run, never enumerate/delete
    # a provider account's unrelated jobs, files or namespaces.
    batches = sql("SELECT provider_job_id FROM provider_batches WHERE provider_job_id IS NOT NULL")
    from google import genai
    with genai.Client(api_key=os.environ["GEMINI_API_KEY"], http_options={"retry_options": {"attempts": 1}}) as client:
        for batch in batches:
            try:
                remote = client.batches.get(name=batch["provider_job_id"])
            except Exception as error:
                if getattr(error, "code", None) == 404:
                    continue  # Already-removed run-owned jobs are not live work.
                raise
            if remote.state.name not in {"JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED"}:
                client.batches.cancel(name=batch["provider_job_id"])
    if state.get("orphan_uploads"):
        with genai.Client(api_key=os.environ["GEMINI_API_KEY"], http_options={"retry_options": {"attempts": 1}}) as client:
            for upload in state["orphan_uploads"]:
                try:
                    client.files.delete(name=upload["file_id"])
                except Exception as error:
                    if getattr(error, "code", None) not in {403, 404}:
                        raise
    for root in state.get("roots", []):
        request("DELETE", f"/roots/{root}", key=state["key"], statuses=(200, 404))
        prefix = f"sources/{state['org']}/{root}/"
        assert not s3.list_objects_v2(Bucket=BUCKET, Prefix=prefix).get("Contents"), "root deletion left source objects"
    for org in state.get("other_orgs", []):
        request("DELETE", f"/admin/orgs/{org}", statuses=(200, 204, 404))
    request("DELETE", f"/admin/orgs/{state['org']}", statuses=(200, 204, 404))
    for user in state.get("users", []):
        request("DELETE", f"/admin/users/{user}", statuses=(200, 204, 404))


if __name__ == "__main__":
    for name in ("TURBOPUFFER_API_KEY", "GEMINI_API_KEY"):
        if not os.environ.get(name):
            raise SystemExit(f"{name} is required: no provider stubs or skipped tests")
    phase = sys.argv[1]
    functions = {"capture": capture, "multipart-recovery": multipart_recovery,
                 "native-capture": native_capture, "native-transformed": native_transformed,
                 "native-published": native_published, "follow-backlog": follow_backlog,
                 "verify": verify, "authorization": authorization,
                 "outage": outage, "resumed": resumed, "cleanup": cleanup}
    if phase == "retention-security":
        from retention_security import verify as verify_retention_security
        functions[phase] = verify_retention_security
    if phase == "capture-acl-race":
        from retention_security import check_capture_acl_race
        functions[phase] = lambda: check_capture_acl_race(json.loads(STATE.read_text()) if STATE.exists() else provision())
    if phase == "capture-permission-races":
        from capture_permissions import check_capture_revocations
        functions[phase] = lambda: check_capture_revocations(json.loads(STATE.read_text()) if STATE.exists() else provision())
    if phase == "format-variants":
        from format_variants import verify as verify_formats
        functions[phase] = verify_formats
    if phase == "cloud-index":
        from cloud_index_scenario import verify as verify_cloud_index
        functions[phase] = verify_cloud_index
    if phase == "worker-throughput":
        from worker_throughput import verify as verify_worker_throughput
        functions[phase] = verify_worker_throughput
    started = time.monotonic()
    status = "failed"
    try:
        functions[phase]()
        status = "passed"
    finally:
        REPORT.parent.mkdir(parents=True, exist_ok=True)
        with REPORT.open("a") as output:
            run_id = json.loads(STATE.read_text()).get("nonce") if STATE.exists() else None
            output.write(json.dumps({"run_id": run_id, "phase": phase, "status": status,
                                     "seconds": round(time.monotonic()-started, 2)}) + "\n")
        print(f"{phase}: {status}", flush=True)
