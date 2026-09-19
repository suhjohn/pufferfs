"""CLI-to-publication benchmark with synthetic, checked contents.

No application imports or direct work invocation. Production worker processes
and the real native embedding provider handle every capture.
"""

import json
import os
from pathlib import Path
import time

import run


def verify():
    state = run.provision()
    vector_disabled = os.environ.get("PUFFERFS_E2E_THROUGHPUT_NO_VECTOR", "false") == "true"
    directory = Path("/state/throughput")
    directory.mkdir()
    expected = {}
    repeats = int(os.environ.get("PUFFERFS_E2E_THROUGHPUT_REPEATS", "1"))
    assert 1 <= repeats <= 16
    fixtures = (8, 64, 128, 768) * repeats
    if os.environ.get("PUFFERFS_E2E_THROUGHPUT_RECORDS"):
        records = int(os.environ["PUFFERFS_E2E_THROUGHPUT_RECORDS"])
        assert 1 <= records <= 768
        fixtures = (records,) * len(fixtures)
    for ordinal, records in enumerate(fixtures):
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
    state["root"] = run.new_root(state, "Worker throughput", directory, vector_disabled)
    run.save(state)
    for label, flags in (("initial", ()), ("reindex", ("--force",))):
        started = time.monotonic()
        capture_started = time.monotonic()
        run.cli(state, "sync", str(directory), "--id", state["root"], *flags, *(["--no-vector"] if vector_disabled else []))
        capture_seconds = time.monotonic() - capture_started
        files = run.wait_indexed(state)
        elapsed = time.monotonic() - started
        rows = run.sql("""SELECT f.path,e.chunk_count,w.id AS work_id,w.stage,w.attempt_count,
            w.status AS work_status,e.status AS extraction_status,e.chunks_ref
            FROM file_catalog f JOIN file_extractions e ON e.id=f.indexed_extraction_id
            JOIN file_work w ON w.extraction_id=e.id WHERE f.root_id=%s ORDER BY f.path,w.stage""", (state["root"],))
        assert len(rows) == len(expected)
        for row in rows:
            assert row["chunk_count"] == len(expected[row["path"]])
            assert row["attempt_count"] >= 1
            assert row["extraction_status"] == row["work_status"] == "complete"
            assert row["stage"] == "index" and row["chunks_ref"]
            chunks = list(run.chunks(row["chunks_ref"]))
            assert len(chunks) == row["chunk_count"]
            assert all(chunk["content"] in "".join(expected[row["path"]]) for chunk in chunks)
        assert run.assert_index_vectors(state, state["root"], dimensions=None if vector_disabled else 4096) == sum(map(len, expected.values()))
        for path, lines in expected.items():
            run.assert_source_retained(files[path])
            read = run.request("POST", f"/roots/{state['root']}/read",
                {"path": path, "lines": {"start": 1, "end": len(lines)}}, key=state["key"])
            assert [line["content"] for line in read["lines"]] == [line.rstrip("\n") for line in lines]
        for mode in (("fts",) if vector_disabled else ("fts", "vector", "hybrid")):
            result = run.request("POST", "/query", {"root_id": state["root"], "query": "telescope calibration",
                "mode": mode, "top_k": 5}, key=state["key"])
            assert result["results"]
            for hit in result["results"]:
                assert hit["content"] in "".join(expected[hit["file_path"]])
        result = {"event": "worker_throughput", "run_id": state["nonce"], "phase": label,
                  "vector_disabled": vector_disabled, "capture_seconds": round(capture_seconds, 3),
                  "concurrency": int(os.environ.get("PUFFERFS_E2E_THROUGHPUT_CONCURRENCY", "4")),
                  "embedding_batch_documents": int(os.environ.get("PUFFERFS_EMBEDDING_BATCH_DOCUMENTS", "64")),
                  "source_bytes": sum(len(line.encode()) for lines in expected.values() for line in lines),
                  "files": len(expected), "chunks": sum(map(len, expected.values())),
                  "capture_to_publication_seconds": round(elapsed, 3), "work": rows}
        with Path("/artifacts/worker-throughput.jsonl").open("a") as output:
            output.write(json.dumps(result) + "\n")
        print(json.dumps(result), flush=True)
        from index_recovery import relay
        names = {row["namespace"] for row in run.sql("SELECT namespace FROM root_index_namespaces WHERE root_id=%s AND retired_at IS NULL",(state["root"],))}
        observed = [e for e in relay("GET","/status")["events"] if e["namespace"] in names]
        with Path("/artifacts/worker-throughput-network.jsonl").open("a") as output:
            output.write(json.dumps({"run_id":state["nonce"],"phase":label,"events":observed})+"\n")
