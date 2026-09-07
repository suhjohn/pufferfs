"""Actual CLI/SQS/Modal GPU publication; read-only assertions on cloud artifacts."""

import hashlib
import math
import os
from pathlib import Path
import struct
import urllib.request
import uuid

from botocore.exceptions import ClientError
import run


def verify():
    try:
        run.s3.list_buckets()
    except ClientError as error:
        assert error.response["Error"]["Code"] == "AccessDenied"
    else:
        raise AssertionError("Runtime AWS credentials can enumerate unrelated buckets")
    state = run.provision()
    directory = Path("/state/cloud-index")
    directory.mkdir()
    # Distinct generic records force multiple 64-vector GPU microbatches in one S3 pack.
    path = directory / "measurements.jsonl"
    import json

    def record(number, text):
        # Each complete record fits one 6,000-byte chunk, but two do not.
        # Tiny JSONL records legitimately coalesce and cannot exercise several
        # 64-vector batches. The append has the same boundary requirement.
        line = json.dumps({"record": number, "text": text +
            " Calibration tracks temperature, humidity, exposure and instrument alignment." * 50}) + "\n"
        assert 3000 < len(line.encode()) < 6000
        return line

    with path.open("w") as output:
        for number in range(130):
            output.write(record(number, "Orchid telescope measurement " + str(number)))
    state["root"] = run.new_root(state, "cloud GPU publication", directory, False)
    empty_root = run.new_root(state, "uncaptured vector root", directory / "uncaptured", False)
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", state["root"])
    files = run.wait_indexed(state)
    initial = run.assert_source_retained(files[path.name])
    extraction, = run.sql("""SELECT e.chunk_count,w.mutation_ref,w.acknowledged_batches,w.mutation_batch_count
        FROM file_catalog f JOIN file_extractions e ON e.id=f.indexed_extraction_id
        JOIN file_work w ON w.extraction_id=e.id AND w.stage='index' WHERE f.id=%s""", (files[path.name]["file_id"],))
    assert extraction["chunk_count"] == 130 and extraction["mutation_ref"]
    assert extraction["acknowledged_batches"] == extraction["mutation_batch_count"] > 0
    mutations = list(run.chunks(extraction["mutation_ref"]))
    assert len(mutations) == extraction["mutation_batch_count"]
    published = [row for mutation in mutations for row in mutation["write"]["upsert_rows"]]
    assert len(published) == 130
    vectors = run.embedding_locations(state["org"])
    assert len(vectors) == 130 and len({v["object_key"] for v in vectors}) == 1
    assert run.sql("SELECT to_regclass('embedding_locations') AS table_name")[0]["table_name"] is None
    stamps = {}
    packed_vectors = {}
    for key in {v["object_key"] for v in vectors}:
        with run.s3.get_object(Bucket=run.BUCKET, Key=key)["Body"] as source:
            data = source.read()
        stamps[key] = hashlib.sha256(data).hexdigest()
        for locator in vectors:
            if locator["object_key"] == key:
                offset = locator["byte_offset"]
                packed_vectors[locator["content_hash"]] = data[offset:offset + locator["byte_length"]]
        assert len(data) % (768 * 4) == 0
        for offset in range(0, len(data), 768 * 4):
            vector = struct.unpack_from("<768f", data, offset)
            assert all(math.isfinite(value) for value in vector)
            assert abs(sum(value * value for value in vector) - 1) < 0.02
    for row in published:
        assert run.vector_bytes(row["vector"], 768) == packed_vectors[row["content_hash"]]
    for mode in ("fts", "vector", "hybrid"):
        for selector, count in (({"root_id": state["root"]}, 1),
                                ({"root_ids": [empty_root, state["root"], empty_root]}, 2), ({"all_roots": True}, 2)):
            result = run.request("POST", "/query", dict(selector, query="telescope", mode=mode, top_k=5), key=state["key"])
            assert result["roots_searched"] == count and result["results"]
            assert all(hit["file_path"] == path.name and hit["root_id"] == state["root"] for hit in result["results"])
    with path.open("a") as output:
        output.write(record(130, "Violet rainfall update"))
    run.cli(state, "sync", str(directory), "--id", state["root"])
    updated = run.wait_indexed(state)
    manifest = run.assert_source_retained(updated[path.name])
    assert manifest["extents"][:len(initial["extents"])] == initial["extents"]
    current = run.embedding_locations(state["org"])
    assert len(current) == 131
    assert all(row in current for row in vectors), "append replaced cached vector locators"
    for key, digest in stamps.items():
        with run.s3.get_object(Bucket=run.BUCKET, Key=key)["Body"] as source:
            assert hashlib.sha256(source.read()).hexdigest() == digest
    read = run.request("POST", f"/roots/{state['root']}/read", {"path": path.name, "lines": {"start": 131, "end": 131}}, key=state["key"])
    assert "Violet rainfall update" in read["lines"][0]["content"]

    verify_vector_ranking(state, empty_root)

    # Multipart is exercised through the public API and signed upload URL.
    # Its unaccepted pack is removed by the ordinary root deletion workflow.
    payload = b"bounded synthetic source bytes\n" * 1024
    body = {"request_id": str(uuid.uuid4()), "size": len(payload)}
    endpoint = f"/roots/{state['root']}/sources/multipart"
    upload = run.request("POST", endpoint + "/init", body, key=state["key"])
    resumed = run.request("POST", endpoint + "/init", body, key=state["key"])
    assert resumed["upload_id"] == upload["upload_id"]  # real ListParts permission
    part = run.request("POST", endpoint + "/part", {"object_key": upload["object_key"], "part_number": 1}, key=state["key"])
    headers = {key: values[0] for key, values in part["headers"].items()}
    with urllib.request.urlopen(urllib.request.Request(part["url"], data=payload, method="PUT", headers=headers), timeout=30) as response:
        etag = response.headers["ETag"]
    run.request("POST", endpoint + "/complete", {"object_key": upload["object_key"], "parts": [{"part_number": 1, "etag": etag}]}, key=state["key"])
    with run.s3.get_object(Bucket=run.BUCKET, Key=upload["object_key"])["Body"] as source:
        assert source.read() == payload
    abandoned = run.request("POST", endpoint + "/init", {"request_id": str(uuid.uuid4()), "size": 128}, key=state["key"])
    uploads = run.s3.list_multipart_uploads(Bucket=run.BUCKET, Prefix=f"sources/{state['org']}/{state['root']}/").get("Uploads", [])
    assert any(item["UploadId"] == abandoned["upload_id"] for item in uploads)
    run.request("DELETE", f"/roots/{state['root']}", key=state["key"])
    # The API removes current objects; bounded multipart/late-write cleanup is
    # deliberately owned by the scheduled reconciler, not the request handler.
    def multipart_cleaned():
        prefix = f"sources/{state['org']}/{state['root']}/"
        if run.s3.list_multipart_uploads(Bucket=run.BUCKET, Prefix=prefix).get("Uploads"):
            return False
        target, = run.sql("SELECT last_checked_at FROM root_cleanup_targets WHERE root_id=%s AND kind='prefix' AND target=%s",
            (state["root"], prefix))
        return bool(target["last_checked_at"])

    run.eventually("scheduled root cleanup to abort the abandoned AWS upload", multipart_cleaned, timeout=180)
    assert not run.s3.list_objects_v2(Bucket=run.BUCKET, Prefix=f"sources/{state['org']}/{state['root']}/").get("Contents")
    print("Actual AWS multipart init/resume/upload/complete/abort/delete and SQS -> Modal GPU -> S3 vectors/mutations -> search/read passed; append reused 130 cached vectors.", flush=True)


def verify_vector_ranking(state, empty_root):
    """Compare public search with actual provider distances, across roots/shards."""
    import json

    directory = Path("/state/vector-ranking")
    directory.mkdir()
    texts = ["Telescopes observe distant stars and galaxies.",
             "A gardener plants carrots in fertile soil.",
             "An optical observatory measures light from a nebula.",
             "A chef kneads bread dough and preheats the oven.",
             "Astronomers align telescope mirrors for deep sky imaging.",
             "A mechanic replaces worn bicycle brakes.",
             "A spectrograph records the wavelengths of starlight.",
             "A musician tunes a violin before the concert."]
    for i, text in enumerate(texts):
        (directory / f"record-{i}.txt").write_text(text + "\n")
    root = run.new_root(state, "vector ranking", directory, False)
    run.cli(state, "sync", str(directory), "--id", root)
    run.wait_indexed(state, root)
    query = "telescope"
    request = urllib.request.Request(os.environ["MODAL_QUERY_EMBED_ENDPOINT"],
        data=json.dumps({"secret_key": os.environ["MODAL_SECRET_KEY"], "texts": [query]}).encode(),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=180) as response:
        vector, = json.load(response)["embeddings"]
    expected = {}
    for root_id in (state["root"], root):
        namespaces = run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=%s AND retired_at IS NULL", (root_id,))
        publications = [row["indexed_extraction_id"] for row in run.sql(
            "SELECT indexed_extraction_id FROM file_catalog WHERE root_id=%s AND NOT deleted", (root_id,))]
        counts = []
        distances = {}
        for namespace in namespaces:
            # Read-only assertions against the real provider; every document was
            # created through CLI capture and the ordinary publication pipeline.
            rows = run.request("POST", f"/v2/namespaces/{namespace['namespace']}/query",
                {"rank_by": ["vector", "ANN", vector], "limit": 200,
                 "filters": ["extraction_id", "In", publications],
                 "include_attributes": ["file_path", "chunk_index"]},
                key=os.environ["TURBOPUFFER_API_KEY"], server=os.environ["TURBOPUFFER_API_URL"], statuses=(200, 404))
            rows = rows.get("rows", [])
            counts.append(len(rows))
            distances.update({(root_id, row["file_path"], row["chunk_index"]): row["$dist"] for row in rows})
        if root_id == root:
            assert sum(counts) == len(texts)
            assert sum(count > 0 for count in counts) == len(namespaces), "fixture did not populate every configured shard"
        expected[root_id] = distances
    selections = [[state["root"]], [root], [empty_root, state["root"]],
                  [root, state["root"]], [state["root"], root, empty_root]]
    cases = [(dict(root_id=selected[0]) if len(selected) == 1 else dict(root_ids=selected), selected)
             for selected in selections]
    cases.append(({"all_roots": True}, selections[-1]))
    for selector, selected in cases:
        reference = {key: value for root_id in selected for key, value in expected.get(root_id, {}).items()}
        for top_k in (5, 200):
            result = run.request("POST", "/query", dict(selector, query=query, mode="vector", top_k=top_k), key=state["key"])
            assert result["roots_searched"] == len(selected)
            hits = result["results"]
            assert len(hits) == min(top_k, len(reference))
            scores = [hit["score"] for hit in hits]
            assert scores == sorted(scores), "vector distances must rank nearest first across roots and shards"
            assert all(abs(hit["score"] - reference[(hit["root_id"], hit["file_path"], hit["chunk_index"])]) < 1e-4 for hit in hits), "vector search replaced provider distances with fusion scores"
            assert all(abs(score - wanted) < 1e-4 for score, wanted in zip(scores, sorted(reference.values())[:top_k])), "global top-k omitted a nearer candidate"
    print("Vector ranking matched real provider distances and global top-k across populated/empty roots and every configured shard.", flush=True)
