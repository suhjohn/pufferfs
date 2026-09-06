"""Shared index schema and provider connection, without embedding dependencies."""

SCHEMA = {
    **{name: {"type": "string"} for name in (
        "file_path", "absolute_path", "file_id", "root_id", "version_id", "extraction_id",
        "content_hash", "file_hash", "file_type", "source_manifest_ref", "location_json",
    )},
    **{name: {"type": "uint"} for name in ("chunk_index", "version_sequence", "extraction_sequence", "page_number", "line_start", "line_end")},
    "content": {"type": "string", "full_text_search": True},
}


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
