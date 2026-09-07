"""Single-slot CLI-to-publication benchmark with synthetic, checked contents.

No application imports or direct work invocation. Run with the isolated cloud
runner so the encoder is the real deployed GPU role and providers are real.
"""

import json
from pathlib import Path
import time

import run


def verify():
    state = run.provision()
    directory = Path("/state/throughput")
    directory.mkdir()
    expected = {}
    fixtures = ((8, 1), (64, 1), (128, 1), (768, 2))
    for ordinal, (records, _) in enumerate(fixtures):
        lines = []
        for number in range(records):
            # Mix natural language, numeric logs and UTF-8 in one corpus. Each
            # record occupies one source chunk under the documented byte limit.
            text = ("Calibration tracks temperature, humidity and telescope alignment. "
                    if ordinal % 2 == 0 else "測定 instrument αβ coordinates=123.456,-78.901 flags=[17,23,41] ")
            line = json.dumps({"file": ordinal, "record": number, "text": text * (55 + number % 7)}, ensure_ascii=False) + "\n"
            assert 3000 < len(line.encode()) < 6000
            lines.append(line)
        path = directory / f"measurements-{ordinal}.jsonl"
        path.write_text("".join(lines))
        expected[path.name] = lines
    state["root"] = run.new_root(state, "Single worker throughput", directory, False)
    run.save(state)
    cached = None
    for label, flags in (("cold-cache", ()), ("warm-cache", ("--force",))):
        started = time.monotonic()
        run.cli(state, "sync", str(directory), "--id", state["root"], *flags)
        files = run.wait_indexed(state)
        elapsed = time.monotonic() - started
        rows = run.sql("""SELECT f.path,e.chunk_count,w.id AS work_id,w.stage,w.attempt_count,
            w.status AS work_status,e.status AS extraction_status,w.mutation_ref,
            w.mutation_batch_count,w.acknowledged_batches
            FROM file_catalog f JOIN file_extractions e ON e.id=f.indexed_extraction_id
            JOIN file_work w ON w.extraction_id=e.id WHERE f.root_id=%s ORDER BY f.path,w.stage""", (state["root"],))
        assert len(rows) == 2 * len(expected)
        for row in rows:
            assert row["chunk_count"] == len(expected[row["path"]])
            assert row["attempt_count"] == 1
            assert row["extraction_status"] == row["work_status"] == "complete"
            if row["stage"] == "index":
                assert row["mutation_ref"] and row["acknowledged_batches"] == row["mutation_batch_count"]
        locations = run.embedding_locations(state["org"])
        assert len(locations) == sum(map(len, expected.values()))
        assert len({row["object_key"] for row in locations}) == sum(packs for _, packs in fixtures)
        assert run.sql("SELECT to_regclass('embedding_locations') AS table_name")[0]["table_name"] is None
        if cached is not None:
            assert locations == cached, "force reindex did not preserve cached vector locations"
        cached = locations
        packs = {}
        for key in {location["object_key"] for location in locations}:
            with run.s3.get_object(Bucket=run.BUCKET, Key=key)["Body"] as body:
                packs[key] = body.read()
        vectors = {location["content_hash"]: packs[location["object_key"]][
            location["byte_offset"]:location["byte_offset"] + location["byte_length"]]
            for location in locations}
        mutation_bytes = mutation_records = vector_json_bytes = 0
        for row in rows:
            if row["stage"] != "index":
                continue
            records = list(run.chunks(row["mutation_ref"]))
            assert len(records) == row["mutation_batch_count"]
            mutation_records += len(records)
            for record in records:
                mutation_bytes += len(json.dumps(record, ensure_ascii=False, separators=(",", ":")).encode())
                for published in record["write"]["upsert_rows"]:
                    vector = published["vector"]
                    assert run.vector_bytes(vector, 768) == vectors[published["content_hash"]]
                    vector_json_bytes += len(json.dumps(vector, separators=(",", ":")).encode())
        for path, lines in expected.items():
            run.assert_source_retained(files[path])
            read = run.request("POST", f"/roots/{state['root']}/read",
                {"path": path, "lines": {"start": 1, "end": len(lines)}}, key=state["key"])
            assert [line["content"] for line in read["lines"]] == [line.rstrip("\n") for line in lines]
        for mode in ("fts", "vector", "hybrid"):
            result = run.request("POST", "/query", {"root_id": state["root"], "query": "telescope calibration",
                "mode": mode, "top_k": 5}, key=state["key"])
            assert result["results"]
            for hit in result["results"]:
                assert hit["content"] in "".join(expected[hit["file_path"]])
        result = {"event": "worker_throughput", "run_id": state["nonce"], "phase": label,
                  "files": len(expected), "chunks": sum(map(len, expected.values())),
                  "mutation_records": mutation_records, "mutation_json_bytes": mutation_bytes,
                  "vector_json_bytes": vector_json_bytes,
                  "capture_to_publication_seconds": round(elapsed, 3), "work": rows}
        with Path("/artifacts/worker-throughput.jsonl").open("a") as output:
            output.write(json.dumps(result) + "\n")
        print(json.dumps(result), flush=True)
