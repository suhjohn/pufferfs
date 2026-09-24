"""Turbopuffer schema and native Qwen embedding contract."""

import os

EMBEDDING_MODEL = "qwen/qwen3-embedding-8b"
EMBEDDING_DIMENSIONS = 4096
EMBEDDING_BATCH_DOCUMENTS = int(os.getenv("PUFFERFS_EMBEDDING_BATCH_DOCUMENTS", "64"))
if not 1 <= EMBEDDING_BATCH_DOCUMENTS <= 256:
    raise ValueError("embedding batch documents must be 1..256")

SCHEMA = {
    **{name: {"type": "string"} for name in (
        "file_path", "absolute_path", "file_id", "root_id", "version_id", "extraction_id", "segment_id",
        "content_hash", "file_hash", "file_type", "source_manifest_ref", "location_json",
    )},
    **{name: {"type": "uint"} for name in ("chunk_index", "version_sequence", "extraction_sequence", "page_number", "line_start", "line_end")},
    "content": {"type": "string", "full_text_search": True},
}


def write_options(vector_disabled):
    schema = dict(SCHEMA)
    if vector_disabled:
        return {"schema": schema}
    schema["content"] = {**SCHEMA["content"], "embed": {
        "model": EMBEDDING_MODEL, "attribute": "vector",
        "dims": EMBEDDING_DIMENSIONS, "dtype": "f32",
    }}
    return {"schema": schema, "distance_metric": "cosine_distance"}


def turbopuffer_client(*, timeout=120, max_retries=4):
    import os
    from turbopuffer import Turbopuffer

    options = {"api_key": os.environ["TURBOPUFFER_API_KEY"],
               "region": os.getenv("TURBOPUFFER_REGION", "gcp-us-central1"),
               "timeout": timeout, "max_retries": max_retries, "compression": True}
    if os.getenv("TURBOPUFFER_API_URL"):
        options["base_url"] = os.environ["TURBOPUFFER_API_URL"]
        if "{region}" not in options["base_url"]:
            options.pop("region")
    return Turbopuffer(**options)
