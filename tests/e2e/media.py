"""Real media capture -> Gemini Batch -> chunks -> Turbopuffer diagnostics."""

import hashlib
import json
from pathlib import Path
import time

from fixtures import create_extended_media
import run


def verify():
    state = run.provision()
    directory = Path("/state/media")
    expectations = create_extended_media(directory)
    state["root"] = run.new_root(state, "e2e-media", directory, True)
    run.save(state)
    run.cli(state, "sync", str(directory), "--id", state["root"], "--no-vector")
    files = run.wait_indexed(state)
    assert set(files) == set(expectations)
    observations = []
    for path, file in files.items():
        run.assert_source_retained(file)
        row, = run.sql("""SELECT e.id,e.chunks_ref,e.chunk_count FROM file_extractions e
            JOIN file_catalog f ON f.indexed_extraction_id=e.id WHERE f.id=%s""", (file["file_id"],))
        records = list(run.chunks(row["chunks_ref"]))
        requests = run.sql("""SELECT p.ordinal,p.request_key,p.location,p.status,p.error,b.provider_job_id
            FROM provider_requests p JOIN provider_batches b ON b.id=p.batch_id
            WHERE p.extraction_id=%s ORDER BY p.ordinal""", (row["id"],))
        observation = {"path": path, "source_hash": file["content_hash"],
                       "extraction_id": row["id"], "requests": requests,
                       "chunk_count": len(records), "chunks": records}
        observations.append(observation)
        # All inputs are synthetic recordings. Preserve every transcript
        # and its exact request/location mapping, including passing siblings,
        # before any content assertion or external cleanup.
        print(json.dumps(observation), flush=True)
        assert len(records) == row["chunk_count"]
    for observation in observations:
        records, path = observation["chunks"], observation["path"]
        content = " ".join(c["content"] for c in records).lower()
        expected = expectations[path]
        assert all(word in content for word in expected["terms"]), f"missing extracted content: {path}"
        assert [c["chunk_index"] for c in records] == list(range(len(records)))
        requests = observation["requests"]
        assert len(requests) == expected["clip_count"]
        assert [item["ordinal"] for item in requests] == list(range(len(requests)))
        assert all(item["status"] == "complete" for item in requests)
        by_scope = {item["request_key"]: item for item in requests}
        assert len(by_scope) == len(requests), "split recordings reused a speaker scope"
        for chunk in records:
            assert hashlib.sha256(chunk["content"].encode()).hexdigest() == chunk["content_hash"]
            location = chunk["location"]
            assert location["speaker"] and location["speaker_scope"]
            request = by_scope[location["speaker_scope"]]
            clip = request["location"]
            assert clip["start_seconds"] <= location["start_seconds"] <= location["end_seconds"] <= clip["end_seconds"]
        for request, clip in zip(requests, expected.get("clips", [])):
            assert request["location"] == {key: clip[key] for key in ("start_seconds", "end_seconds")}
            scoped = [c for c in records if c["location"]["speaker_scope"] == request["request_key"]]
            if clip.get("silent"):
                assert not scoped, f"invented speech in silent clip: {path}, clip {request['ordinal']}"
            text = " ".join(c["content"] for c in scoped).lower()
            assert all(word in text for word in clip["terms"]), "speech missing from its expected clip"
        for utterance in expected.get("utterances", []):
            matches = [c for c in records if all(word in c["content"].lower() for word in utterance["terms"])
                       and abs(c["location"]["start_seconds"] - utterance["start_seconds"]) <= utterance["tolerance_seconds"]]
            assert matches, f"speech missing at expected time: {path}: {utterance}"
        result = run.request("POST", "/query", {"root_id": state["root"], "query": expected["query"],
            "glob": path, "mode": "fts", "top_k": 5}, key=state["key"])
        assert result["results"] and all(hit["file_path"] == path for hit in result["results"])
    objects = [item for page in run.s3.get_paginator("list_objects_v2").paginate(Bucket=run.BUCKET)
               for item in page.get("Contents", [])]
    assert not any(item["Key"].lower().endswith((".png", ".jpg", ".wav", ".mp4")) for item in objects)
    print(f"{len(expectations)} media fixtures passed real Batch transcription, timestamp/speaker-scope, durable chunk and public search checks.", flush=True)
    run.provider_cleanup()


if __name__ == "__main__":
    started, status = time.monotonic(), "failed"
    try:
        verify()
        status = "passed"
    finally:
        state = json.loads(run.STATE.read_text()) if run.STATE.exists() else {}
        with run.REPORT.open("a") as output:
            output.write(json.dumps({"run_id": state.get("nonce"), "phase": "media",
                                    "status": status, "seconds": round(time.monotonic() - started, 2)}) + "\n")
        print(f"media: {status}", flush=True)
