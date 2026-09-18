"""Per-file transformation entrypoint, independent of the API server."""

from __future__ import annotations

import os
import tempfile
from pathlib import Path

from extraction import file_family, text_chunks
from file_runtime import complete_extraction
from source_io import iter_source_bytes, materialize_source, read_manifest, write_chunks
from spreadsheet_extract import spreadsheet_chunks
from structured_extract import structured_chunks
from provider_prepare import prepare_provider
from worker_metrics import profile_work, timed, count as metric_count


@profile_work("transform")
def transform(job: dict, s3, check_lease, *, provider=None) -> dict:
    bucket = os.environ["AWS_BUCKET_NAME"]
    with timed("source_manifest"):
        manifest = read_manifest(s3, bucket, job)
    metric_count("source_bytes", job["size_bytes"])
    family = file_family(job["file_path"])
    prefix = f"extractions/{job['org_id']}/{job['root_id']}/{job['extraction_id']}/chunks"
    if family not in {"text", "jsonl", "unknown"}:
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "source" + Path(job["file_path"]).suffix.lower())
            materialize_source(s3, bucket, manifest, path)
            if family == "spreadsheet":
                key, count = write_chunks(s3, bucket, prefix, spreadsheet_chunks(path))
            elif family == "structured":
                key, count = write_chunks(s3, bucket, prefix, structured_chunks(path))
            else:
                if provider is None:
                    from google import genai

                    with genai.Client(api_key=os.environ["GEMINI_API_KEY"],
                            http_options={"timeout": 60000, "retry_options": {"attempts": 1}}) as client:
                        requests = prepare_provider(job, path, client, s3, bucket)
                else:
                    requests = prepare_provider(job, path, provider, s3, bucket)
                if requests:
                    return {"status": "waiting_provider"}
                key, count = write_chunks(s3, bucket, prefix, [])
    else:
        with timed("text_extract"):
            key, count = write_chunks(s3, bucket, prefix, text_chunks(iter_source_bytes(s3, bucket, manifest)))
    metric_count("chunks", count)
    check_lease()
    complete_extraction(job, key, count)
    return {"status": "complete"}
