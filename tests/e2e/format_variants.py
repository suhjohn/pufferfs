"""CLI -> real format decoders/Gemini -> durable chunks -> public search/read."""

import hashlib
from pathlib import Path

from format_fixtures import create
import run


def verify():
    state = run.provision()
    directory = Path("/state/formats")
    expectations = create(directory)
    state["format_expectations"] = expectations
    state["root"] = run.new_root(state, "e2e-format-variants", directory, True)
    run.save(state)
    print(f"Generated {len(expectations)} real format fixtures.", flush=True)
    run.cli(state, "sync", str(directory), "--id", state["root"], "--no-vector")
    assert set(run.catalog(state)) == set(expectations), "CLI omitted format variants"
    remaining = set(expectations)

    def verify_ready():
        files = run.catalog(state)
        assert set(files) == set(expectations), "captured format catalog changed"
        for path in sorted(remaining):
            file = files[path]
            if file["version_id"] != file["indexed_version_id"] or file["processing"]["status"] != "complete":
                continue
            verify_file(state, directory, path, file, expectations[path])
            remaining.remove(path)
        return not remaining

    # Validate each independently published file immediately. A slow provider
    # job must not hide content/address failures in already completed siblings.
    run.eventually("all format variants to pass content/search checks", verify_ready)
    objects = [item["Key"] for page in run.s3.get_paginator("list_objects_v2").paginate(Bucket=run.BUCKET)
               for item in page.get("Contents", [])]
    assert not any(key.lower().endswith((".png", ".jpg", ".pdf", ".wav")) for key in objects), "temporary rendered media persisted"
    run.provider_cleanup()
    print(f"All {len(expectations)} format variants passed.", flush=True)


def verify_file(state, directory, path, file, expected):
    run.assert_source_retained(file)
    with (directory / path).open("rb") as source:
        assert "sha256:" + hashlib.file_digest(source, "sha256").hexdigest() == file["content_hash"]
    row, = run.sql("""SELECT e.id,e.chunks_ref,e.chunk_count,w.mutation_ref,w.acknowledged_batches,w.mutation_batch_count
        FROM file_extractions e JOIN file_catalog f ON f.indexed_extraction_id=e.id
        JOIN file_work w ON w.extraction_id=e.id AND w.stage='index' WHERE f.id=%s""", (file["file_id"],))
    records = list(run.chunks(row["chunks_ref"]))
    assert len(records) == row["chunk_count"] and records, path
    assert [chunk["chunk_index"] for chunk in records] == list(range(len(records))), path
    for chunk in records:
        assert len(chunk["content"].encode()) <= 6000, path
        assert hashlib.sha256(chunk["content"].encode()).hexdigest() == chunk["content_hash"], path
    content = "\n".join(chunk["content"] for chunk in records).lower()
    assert all(term in content for term in expected["terms"]), f"missing content: {path}: {content[:1500]}"
    assert row["mutation_ref"] and row["acknowledged_batches"] == row["mutation_batch_count"], path
    if "sheets" in expected:
        sheets = {}
        for chunk in records:
            location = chunk["location"]
            assert 1 <= location["row_start"] <= location["row_end"], path
            sheet = location["sheet"]
            sheets[sheet] = sheets.get(sheet, "") + chunk["content"].split("\nCells:\n", 1)[1]
        assert sheets == expected["sheets"], f"cell content/addresses changed: {path}: {sheets}"
    if "anchor" in expected:
        assert sorted({chunk["location"][expected["anchor"]] for chunk in records}) == expected["anchors"], path
    if expected.get("anchor") == "page_number":
        response = run.request("POST", f"/roots/{state['root']}/read", {"path": path,
            "pages": {"start": 1, "end": len(expected["anchors"])}}, key=state["key"])
        assert [page["page_number"] for page in response["pages"]] == expected["anchors"], path
        text = "\n".join(page["content"] for page in response["pages"]).lower()
        assert all(term in text for term in expected["terms"]), f"page read lost content: {path}"
    response = run.request("POST", "/query", {"root_id": state["root"], "query": expected["terms"][0],
        "glob": path, "mode": "fts", "top_k": 10}, key=state["key"])
    assert response["results"] and all(hit["file_path"] == path for hit in response["results"]), path
    print(f"{path}: source hash, {len(records)} chunks, locations, durable publication and public search passed.", flush=True)
