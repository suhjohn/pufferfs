"""Black-box scenarios. All application writes go through the CLI or HTTP API.

SQL and S3 reads inspect actual durable effects. The duplicate-delivery scenario
uses SQS's public API; it never invokes worker processing directly or mutates its DB state.
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
sqs = boto3.client("sqs")


def request(method, path, payload=None, *, key=None, statuses=(200,), server=None, cookie=None):
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
        return json.loads(body) if body else None


def sql(query, args=()):
    with psycopg.connect(os.environ["DATABASE_URL"], row_factory=dict_row,
                         options="-c default_transaction_read_only=on -c statement_timeout=15000") as conn:
        return conn.execute(query, args).fetchall()


def embedding_locations(org):
    """Read-only expansion of durable pack directories for vector assertions."""
    return sql("""SELECT DISTINCT ON (p.model_revision,h.hash) p.org_id,p.model_revision,
        h.hash AS content_hash,p.object_key,(h.slot-1)*p.dimensions::bigint*4 AS byte_offset,
        p.dimensions*4 AS byte_length,p.dimensions
        FROM embedding_packs p,unnest(p.content_hashes) WITH ORDINALITY AS h(hash,slot)
        WHERE p.org_id=%s AND p.retired_at IS NULL AND h.hash IS NOT NULL
        ORDER BY p.model_revision,h.hash,p.object_key""", (org,))


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
        line = b"Captured orchid observatory log.".ljust(63, b" ") + b"\n"
        with path.open("wb") as output:
            for _ in range(512):
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
                env=dict(os.environ, PUFFERFS_API_KEY=state["key"]), stdout=log, stderr=log, text=True)
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
        assert manifest["size"] == 32 << 20 and manifest["content_hash"] == expected
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
    assert all(not file["indexed_version_id"] for file in files.values()), "consumers must be stopped during capture"
    state["before"] = files
    save(state)
    # Catalog/state is real even though neither execution consumer is running.
    result = request("POST", "/query", {"query": "Orchid", "root_id": state["root"], "mode": "fts", "top_k": 10}, key=state["key"])
    assert not result.get("results"), "unindexed capture was visible"
    pending = sqs.get_queue_attributes(QueueUrl=os.environ["PUFFERFS_SQS_TRANSFORM_QUEUE_URL"],
                                      AttributeNames=["ApproximateNumberOfMessages"])
    assert int(pending["Attributes"]["ApproximateNumberOfMessages"]) > 0
    assert sql("SELECT count(*) n FROM source_multipart_uploads")[0]["n"] > 0, "large source did not use multipart"
    print(f"Captured {len(files)} synthetic files across 100 directories without any execution consumer.")


def work_rows(root, stage="transform"):
    return sql("""SELECT w.id,w.stage,w.status,w.enqueued_at,w.attempt_count,w.extraction_id,
        e.status extraction_status,e.chunks_ref,e.chunk_count,
        v.id version_id,f.id file_id,f.path,f.indexed_version_id,f.root_id,r.org_id FROM file_work w
        JOIN file_extractions e ON e.id=w.extraction_id JOIN file_versions v ON v.id=e.version_id
        JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
        WHERE f.root_id=%s AND w.stage=%s ORDER BY w.id""", (root, stage))


def handoff_outage():
    state = json.loads(STATE.read_text()) if STATE.exists() else provision()
    directory = Path("/state/handoff-" + uuid.uuid4().hex[:8])
    directory.mkdir()
    for i in range(12):  # More than one SQS SendMessageBatch, all real file captures.
        (directory / f"file-{i:02}.txt").write_text(f"Recoverable orchid telemetry {i}.\n")
    state["handoff_root"] = new_root(state, directory.name, directory, True)
    state["handoff_directory"] = str(directory)
    save(state)
    # The collector and execution consumers are stopped for this scenario.
    # No cleanup targets/indexed files exist before the recovery role starts.
    assert not sql("SELECT id FROM file_catalog WHERE indexed_version_id IS NOT NULL LIMIT 1")
    assert not sql("SELECT root_id FROM root_cleanup_targets LIMIT 1")
    fault("POST", "/proxies/queue-delivery", {"enabled": False})
    cli(state, "sync", str(directory), "--id", state["handoff_root"], "--no-vector")
    work = work_rows(state["handoff_root"])
    assert len(work) == 12 and all(w["status"] == "pending" and w["enqueued_at"] is None and
                                  w["attempt_count"] == 0 for w in work)
    files = catalog(state, state["handoff_root"])
    assert len(files) == 12 and all(not f["indexed_version_id"] for f in files.values())
    state["handoff_work_ids"] = [w["id"] for w in work]
    save(state)
    print("CLI accepted all 12 captured versions while SQS delivery was disconnected.")


def handoff_recovered():
    state = json.loads(STATE.read_text())
    expected = set(state["handoff_work_ids"])
    assert all(w["enqueued_at"] is None for w in work_rows(state["handoff_root"]))
    fault("POST", "/proxies/queue-delivery", {"enabled": True})
    # No CLI registration retry or API call sends these jobs. Only the actual
    # independently scheduled reconciliation container can repair the handoff.
    def repaired():
        work = work_rows(state["handoff_root"])
        return work if work and all(w["enqueued_at"] is not None for w in work) else None
    work = eventually("scheduled reconciliation to deliver the committed captures", repaired, 150)
    assert {w["id"] for w in work} == expected
    assert all(w["status"] == "pending" and w["attempt_count"] == 0 for w in work), "reconciliation executed work"
    inspect_sqs_deliveries(work, "transform")
    print("Scheduled reconciliation delivered all 12 original work IDs in small SQS messages; none executed.")


def sqs_group_id(row):
    identity = row["file_id"] if row["stage"] == "index" else row["id"]
    return hashlib.sha256("".join(value + "\0" for value in
        (row["org_id"], row["root_id"], identity, row["stage"])).encode()).hexdigest()


def inspect_sqs_deliveries(work, stage):
    expected = {w["id"] for w in work}
    received, receipts = {}, []
    url = os.environ[f"PUFFERFS_SQS_{stage.upper()}_QUEUE_URL"]
    try:
        for _ in range((len(expected) + 9) // 10 + 10):
            messages = sqs.receive_message(QueueUrl=url, MaxNumberOfMessages=10, WaitTimeSeconds=1,
                                          MessageSystemAttributeNames=["MessageGroupId"],
                                          VisibilityTimeout=60).get("Messages", [])
            for message in messages:
                receipts.append(message["ReceiptHandle"])
                body = json.loads(message["Body"])
                if body.get("work_id") in expected:
                    received[body["work_id"]] = message
            if set(received) == expected:
                break
        assert set(received) == expected, "ledger acknowledgement has no matching SQS delivery"
        for row in work:
            message = received[row["id"]]
            body = json.loads(message["Body"])
            assert message["Attributes"]["MessageGroupId"] == sqs_group_id(row)
            assert len(json.dumps(body).encode()) < 1024
            assert body == {**{k: row[k] for k in ("org_id", "root_id", "file_id", "version_id", "extraction_id")},
                            "job_id": row["id"], "work_id": row["id"], "stage": stage}, \
                f"{stage} delivery has unexpected fields or references: {sorted(body)}"
        sizes = [len(message["Body"].encode()) for message in received.values()]
        print(f"Inspected {len(received)} {stage} deliveries: 8 fields, {min(sizes)}–{max(sizes)} body bytes.")
    finally:
        # Inspection must not consume the work. Actual workers still receive it
        # later; the read increases SQS receive count but not DB attempt count.
        for receipt in receipts:
            sqs.change_message_visibility(QueueUrl=url, ReceiptHandle=receipt, VisibilityTimeout=0)


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
    inspect_sqs_deliveries(work_rows(state["native_root"]), "transform")
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
                 "embedding_cache", "embedding_locations", "content_proofs"):
        assert sql("SELECT to_regclass(%s) AS table_name", (name,))[0]["table_name"] is None
    current = subprocess.run(["pufferfs", "root", "current", "--json"], cwd=directory,
        env=dict(os.environ, PUFFERFS_API_KEY=state["key"]), capture_output=True, text=True, timeout=30)
    assert current.returncode == 0, current.stderr
    identity = json.loads(current.stdout)
    assert identity["id"] == state["native_root"]
    assert identity["name"] == request("GET", f"/roots/{state['native_root']}", key=state["key"])["name"]
    print(f"Captured {len(state['native_files'])} native-format fixtures with execution consumers stopped; retired endpoints absent, root identity persisted.")


def native_transformed():
    state = json.loads(STATE.read_text())
    root, directory = state["native_root"], Path(state["native_directory"])
    def ready():
        rows = work_rows(root)
        return rows if len(rows) == len(state["native_files"]) and all(
            r["status"] == "complete" and r["extraction_status"] == "complete" for r in rows) else None
    rows = eventually("native files to become durable chunks through the SQS consumer", ready, 180)
    assert all(r["attempt_count"] == 1 and not r["indexed_version_id"] for r in rows)
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
    def delivered():
        work = work_rows(root, "index")
        return work if len(work) == len(rows) and all(w["enqueued_at"] is not None for w in work) else None
    index_work = eventually("native extraction's durable index handoff", delivered, 60)
    assert all(w["status"] == "pending" and w["attempt_count"] == 0 for w in index_work)
    inspect_sqs_deliveries(index_work, "index")
    assert not sql("SELECT id FROM provider_batches LIMIT 1"), "native input submitted Gemini work"
    assert not sql("SELECT org_id FROM embedding_packs LIMIT 1"), "transformation generated vectors"
    assert not sql("SELECT id FROM file_work WHERE mutation_ref<>'' LIMIT 1"), "transformation created index mutations"
    # Also drain the preceding handoff-recovery root before stopping this role.
    wait_queue_empty("transform")
    print(f"Native transformation produced {total_chunks} verified chunks for {len(rows)} files and delivered index IDs; no indexing ran.")


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
            assert after["extents"][:len(before["extents"])] == before["extents"], "follow recopied the captured prefix"
            assert sum(e["length"] for e in after["extents"][len(before["extents"]):]) == len(suffix)
            source.write_bytes(replacement)
            rewritten = captured(replacement)
            assert assert_source_retained(rewritten)["extents"] != after["extents"], "rewrite retained stale source extents"
            source.write_bytes(b"")
            empty = captured(b"")
            assert not assert_source_retained(empty).get("extents"), "truncation retained source bytes"
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
    assert [v["id"] for v in history] == ids and len(history) == 5
    assert [v["previous_version_id"] for v in history] == [None] + ids[:-1]
    assert len({v["file_id"] for v in history}) == 1
    # File deletion and successful captures must not remove earlier originals.
    for file in state["follow_versions"][:-1]:
        assert_source_retained(file)
    def delivered():
        jobs = work_rows(root, "index")
        return jobs if len(jobs) == 5 and all(w["enqueued_at"] is not None for w in jobs) else None
    jobs = eventually("all five follow versions to reach index SQS", delivered, 60)
    assert all(w["status"] == "pending" and w["attempt_count"] == 0 for w in jobs)
    inspect_sqs_deliveries(jobs, "index")
    assert not sql("SELECT id FROM provider_batches LIMIT 1")
    wait_queue_empty("transform")
    print("One live agent captured create/append/rewrite/truncate/delete as five linked versions; originals retained and all five index jobs remain unexecuted.")


def redeliver_completed_work(work):
    for row in work:
        body = {k: row[k] for k in ("org_id", "root_id", "file_id", "version_id", "extraction_id", "stage")}
        body.update(job_id=row["id"], work_id=row["id"])
        for _ in range(3):
            sqs.send_message(QueueUrl=os.environ[f"PUFFERFS_SQS_{row['stage'].upper()}_QUEUE_URL"],
                MessageBody=json.dumps(body), MessageGroupId=sqs_group_id(row), MessageDeduplicationId=uuid.uuid4().hex)


def wait_queue_empty(stage):
    url = os.environ[f"PUFFERFS_SQS_{stage.upper()}_QUEUE_URL"]
    settings = sqs.get_queue_attributes(QueueUrl=url,
        AttributeNames=["VisibilityTimeout", "ReceiveMessageWaitTimeSeconds"])["Attributes"]
    # Restart can lose a receive response after SQS has made its receipt
    # invisible. Allow normal production visibility expiry and redelivery.
    timeout = int(settings["VisibilityTimeout"]) + int(settings["ReceiveMessageWaitTimeSeconds"]) + 60
    def empty():
        values = sqs.get_queue_attributes(QueueUrl=url,
            AttributeNames=["ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible"])["Attributes"]
        return not any(int(value) for value in values.values())
    eventually(f"{stage} SQS receipts to be acknowledged", empty, timeout)


def native_replay():
    state = json.loads(STATE.read_text())
    # The shell has stopped the HTTP worker and restarted only the Go consumer.
    # A fresh execution attempt would fail, not silently create another artifact.
    try:
        urllib.request.urlopen("http://transform:8080/healthz", timeout=3).close()
    except urllib.error.HTTPError as error:
        raise AssertionError("transform worker still responds during completed-work replay") from error
    except urllib.error.URLError:
        pass
    else:
        raise AssertionError("transform worker must be stopped during completed-work replay")
    before = work_rows(state["native_root"])
    assert len(before) == len(state["native_files"]) and all(r["status"] == "complete" for r in before)
    def artifacts():
        prefix = f"extractions/{state['org']}/{state['native_root']}/"
        return {item["Key"]: (item["ETag"], item["LastModified"]) for page in
                s3.get_paginator("list_objects_v2").paginate(Bucket=BUCKET, Prefix=prefix)
                for item in page.get("Contents", [])}
    saved_artifacts = artifacts()
    assert len(saved_artifacts) == len(before)
    redeliver_completed_work(before)
    wait_queue_empty("transform")
    assert work_rows(state["native_root"]) == before, "duplicate delivery executed completed work"
    assert artifacts() == saved_artifacts, "duplicate delivery rewrote durable chunks"
    index = work_rows(state["native_root"], "index")
    assert len(index) == len(before) and all(w["status"] == "pending" and w["attempt_count"] == 0 for w in index)
    url = sqs.get_queue_url(QueueName="file-transform-dlq.fifo")["QueueUrl"]
    assert not sqs.receive_message(QueueUrl=url, WaitTimeSeconds=1).get("Messages"), "duplicate delivery entered DLQ"
    print(f"Restarted consumer acknowledged {3 * len(before)} duplicate receipts with the transform worker stopped; no attempts or artifacts changed.")


def worker_authentication():
    # Ordinary HTTP requests to separately running production roles. Use a
    # valid query body so a missing query guard cannot fail for unrelated input.
    payload = {"texts": ["Untrusted request"], "work_id": "untrusted", "attempt_token": "untrusted"}
    credentials = [{}, *({"secret_key": value} for value in
        ("", "incorrect", None, 123, [], {}, "incorrect-\u00e9", "incorrect-\ud800"))]
    for endpoint in ("transform", "index-cpu", "index-vector", "query"):
        for credential in credentials:
            req = urllib.request.Request(f"http://{endpoint}:8080/", method="POST",
                data=json.dumps(payload | credential).encode(), headers={"Content-Type": "application/json"})
            try:
                with urllib.request.urlopen(req, timeout=180):
                    raise AssertionError(f"unauthenticated {endpoint} accepted a request")
            except urllib.error.HTTPError as error:
                assert error.code == 401, f"unauthenticated {endpoint} returned HTTP {error.code}"
    print("Transform, both index roles and query reject missing, invalid and malformed credentials.", flush=True)


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
    rows = sql("""SELECT f.path,e.id AS extraction_id,e.chunks_ref,e.chunk_count,w.mutation_ref,w.acknowledged_batches,w.mutation_batch_count
        FROM file_catalog f JOIN file_extractions e ON e.id=f.indexed_extraction_id
        JOIN file_work w ON w.extraction_id=e.id AND w.stage='index' WHERE f.root_id=%s""", (state["root"],))
    assert len(rows) == len(files)
    assert all(row["acknowledged_batches"] == row["mutation_batch_count"] for row in rows)
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
        assert row["mutation_ref"], f"publication has no durable mutation artifact: {path}"
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
    worker_authentication()
    # No rendered pages or converted clips in durable object storage.
    objects = [item for page in s3.get_paginator("list_objects_v2").paginate(Bucket=BUCKET) for item in page.get("Contents", [])]
    assert not any(item["Key"].lower().endswith((".png", ".jpg", ".wav", ".mp4", ".pdf")) for item in objects)
    assert not embedding_locations(state["org"]), "no-vector root embedded content"
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
    assert [row for row in embedding_locations(state["org"]) if row["dimensions"] == 768]
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
    assert new["extents"][:len(old["extents"])] == old["extents"], "append recopied previous captured bytes"
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
    before = sql("SELECT id,attempt_count,mutation_ref,acknowledged_batches FROM file_work ORDER BY id")
    work = sql("""SELECT w.id,w.stage,e.id extraction_id,v.id version_id,f.id file_id,f.root_id,r.org_id
        FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id JOIN file_versions v ON v.id=e.version_id
        JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
        WHERE f.root_id=%s AND f.path='rewrite.txt' AND w.status='complete'""", (state["root"],))
    redeliver_completed_work(work)
    for stage in ("transform", "index"):
        wait_queue_empty(stage)
    assert before == sql("SELECT id,attempt_count,mutation_ref,acknowledged_batches FROM file_work ORDER BY id")
    for stage in ("transform", "index"):
        url = sqs.get_queue_url(QueueName=f"file-{stage}-dlq.fifo")["QueueUrl"]
        assert not sqs.receive_message(QueueUrl=url, WaitTimeSeconds=1).get("Messages"), "unexpected DLQ message"


def malformed_capture():
    state = json.loads(STATE.read_text()) if STATE.exists() else provision()
    # Run last, with both consumers stopped and prior work already drained.
    # The intentional malformed receipt stays queued until isolated cleanup.
    wait_queue_empty("transform")
    # A stopped client's outstanding 20-second ReceiveMessage can still take
    # a newly sent receipt server-side. Let that long poll expire before this
    # scenario; lost receive responses are exercised separately by native-replay.
    print("Waiting for any stopped consumer's 20-second SQS long poll to expire.", flush=True)
    time.sleep(21)
    directory = Path("/state/malformed-" + uuid.uuid4().hex[:8])
    directory.mkdir()
    for name in ("first.txt", "second.txt"):
        (directory / name).write_text(f"Orchid delivery isolation {name}.\n")
    root = new_root(state, directory.name, directory, True)
    state["malformed_root"] = root
    state["malformed_directory"] = str(directory)
    save(state)
    cli(state, "sync", str(directory), "--id", root, "--no-vector")
    work = work_rows(root)
    assert len(work) == 2 and all(w["status"] == "pending" and w["enqueued_at"] is not None for w in work)
    # SQS accepts malformed application JSON. Use a real work's FIFO group so
    # the consumer can receive valid and malformed bodies in the same batch.
    sent = sqs.send_message(QueueUrl=os.environ["PUFFERFS_SQS_TRANSFORM_QUEUE_URL"],
        MessageBody='{"invalid_e2e_delivery":', MessageGroupId=sqs_group_id(work[0]),
        MessageDeduplicationId=uuid.uuid4().hex)
    state["malformed_message_id"] = sent["MessageId"]
    save(state)
    print("Captured two native files and queued one malformed body beside a valid job; consumers remain stopped.")


def malformed_transformed():
    state = json.loads(STATE.read_text())
    root = state["malformed_root"]
    def ready():
        rows = work_rows(root)
        return rows if len(rows) == 2 and all(r["status"] == "complete" for r in rows) else None
    # Much shorter than the production 300-second SQS visibility timeout:
    # healthy work must execute on this delivery, not after abandoned receipts expire.
    rows = eventually("valid jobs in a malformed receive batch to transform", ready, 90)
    for row in rows:
        assert row["attempt_count"] == 1 and not row["indexed_version_id"]
        records = list(chunks(row["chunks_ref"]))
        assert row["chunk_count"] == len(records) == 1
        expected = (Path(state["malformed_directory"]) / row["path"]).read_text()
        assert records[0]["content"] == expected
        assert records[0]["content_hash"] == hashlib.sha256(expected.encode()).hexdigest()
    for file in catalog(state, root).values():
        assert_source_retained(file)
    index = work_rows(root, "index")
    assert len(index) == 2 and all(w["status"] == "pending" and w["attempt_count"] == 0 for w in index)
    inspect_sqs_deliveries(index, "index")
    def only_malformed_inflight():
        attributes = sqs.get_queue_attributes(QueueUrl=os.environ["PUFFERFS_SQS_TRANSFORM_QUEUE_URL"],
            AttributeNames=["ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible",
                            "ApproximateNumberOfMessagesDelayed"])["Attributes"]
        return (int(attributes["ApproximateNumberOfMessages"]) == 0
                and int(attributes["ApproximateNumberOfMessagesNotVisible"]) == 1
                and int(attributes["ApproximateNumberOfMessagesDelayed"]) == 0)
    eventually("healthy receipts to be acknowledged while the malformed receipt remains invisible",
               only_malformed_inflight, 30)
    print("Both valid jobs transformed once and reached index SQS; only the malformed receipt remains unacknowledged.")


def malformed_published():
    state = json.loads(STATE.read_text())
    root = state["malformed_root"]
    files = wait_indexed(state, root)
    assert set(files) == {"first.txt", "second.txt"}
    for path in files:
        result = request("POST", f"/roots/{root}/read",
            {"path": path, "lines": {"start": 1, "end": 1}}, key=state["key"])
        assert result["lines"][0]["content"] == f"Orchid delivery isolation {path}."
    result = request("POST", "/query", {"root_id": root, "query": "Orchid delivery isolation",
        "mode": "fts", "top_k": 10}, key=state["key"])
    assert {r["file_path"] for r in result["results"]} == set(files)


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
                 "handoff-outage": handoff_outage, "handoff-recovered": handoff_recovered,
                 "native-capture": native_capture, "native-transformed": native_transformed,
                 "native-replay": native_replay, "native-published": native_published,
                 "follow-backlog": follow_backlog,
                 "malformed-capture": malformed_capture, "malformed-transformed": malformed_transformed,
                 "malformed-published": malformed_published,
                 "verify": verify, "authorization": authorization,
                 "outage": outage, "resumed": resumed, "cleanup": cleanup}
    if phase == "embedding-retention":
        from retention_security import check_embedding_retention
        functions[phase] = lambda: check_embedding_retention(provision())
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
