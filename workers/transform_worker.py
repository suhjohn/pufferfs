"""Per-file transformation entrypoint, independent of the API server."""

from __future__ import annotations

import os
import tempfile
from pathlib import Path

from extraction import file_family
from file_runtime import complete_extraction
from source_io import materialize_source, read_manifest
from decoded_segments import prepare_decoded_segments
from segment_runtime import begin_segmented_extraction, lock_transform
from spreadsheet_extract import spreadsheet_chunks
from structured_extract import structured_chunks
from provider_prepare import prepare_provider
from worker_metrics import profile_work, timed, count as metric_count


@profile_work("transform")
def transform(job: dict, s3, check_lease, *, provider=None, stopping=None) -> dict:
    bucket = os.environ["AWS_BUCKET_NAME"]
    with timed("source_manifest"):
        manifest = read_manifest(s3, bucket, job)
    metric_count("source_bytes", job["size_bytes"])
    family = file_family(job["file_path"])
    if family in {"text", "jsonl", "unknown"}:
        from native_segments import transform_native_segments
        return transform_native_segments(job, s3, bucket, manifest, check_lease, stopping)
    if family not in {"text", "jsonl", "unknown"}:
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "source" + Path(job["file_path"]).suffix.lower())
            materialize_source(s3, bucket, manifest, path)
            if family == "spreadsheet":
                begin_segmented_extraction(job)
                key, count = prepare_decoded_segments(job, s3, bucket, spreadsheet_chunks(path), lock_transform)
            elif family == "structured":
                begin_segmented_extraction(job)
                key, count = prepare_decoded_segments(job, s3, bucket, structured_chunks(path), lock_transform)
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
                begin_segmented_extraction(job)
                key, count = prepare_decoded_segments(job, s3, bucket, [], lock_transform)
    metric_count("chunks", count)
    check_lease()
    complete_extraction(job, key, count)
    return {"status": "complete"}
