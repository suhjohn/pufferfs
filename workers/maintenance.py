"""Bounded cleanup independent of provider collection."""

import os
import json


def report_backlog():
    from file_runtime import database
    with database() as conn:
        row = conn.execute("""SELECT
            COUNT(*) FILTER (WHERE w.status IN ('pending','running')) AS pending,
            COUNT(*) FILTER (WHERE w.status='failed' OR e.status='failed') AS failed,
            GREATEST(0,COALESCE(MAX(EXTRACT(EPOCH FROM NOW()-w.next_attempt_at))
                FILTER (WHERE w.status IN ('pending','running')),0))::double precision AS oldest_seconds
            FROM file_work w JOIN file_extractions e ON e.id=w.extraction_id
            JOIN file_versions v ON v.id=e.version_id JOIN file_catalog f ON f.id=v.file_id
            JOIN roots r ON r.id=f.root_id
            WHERE r.deleting_at IS NULL AND f.captured_version_id=v.id
                AND f.indexed_extraction_id IS DISTINCT FROM e.id
                AND w.status IN ('pending','running','failed','waiting_provider')
                AND NOT EXISTS (SELECT 1 FROM file_extractions newer
                    WHERE newer.version_id=e.version_id AND newer.sequence>e.sequence)""").fetchone()
    print(json.dumps({"event": "work_backlog", **row}, separators=(",", ":")), flush=True)

def reconcile():
    from contextlib import closing
    from aws_clients import client
    from botocore.config import Config
    from file_runtime import retire_exhausted_work
    from index_cleanup import cleanup_index
    from segment_cleanup import cleanup_segments
    from root_cleanup import cleanup_deleted_roots
    from artifact_cleanup import cleanup_obsolete_extractions
    from source_cleanup import cleanup_source_packs
    from index_client import turbopuffer_client

    result = {}

    def perform(name, operation):
        try:
            result[name] = operation()
        except Exception as error:
            result[name] = {"error": type(error).__name__}

    perform("exhausted_work", retire_exhausted_work)
    perform("backlog", report_backlog)
    with closing(client("s3", config=Config(connect_timeout=10, read_timeout=20,
                                                  retries={"total_max_attempts": 2}))) as s3:
        with turbopuffer_client(timeout=20, max_retries=0) as tp:
            def apply(namespace, mutation, _vector_disabled):
                # Re-sending a text schema without embed removes native
                # embedding configuration. Deletions must preserve schema.
                response = tp.namespace(namespace).write(**mutation)
                return getattr(response, "rows_remaining", None)

            bucket = os.environ["AWS_BUCKET_NAME"]
            perform("root_cleanup", lambda: cleanup_deleted_roots(s3, bucket, apply))
            perform("index_cleanup", lambda: cleanup_index(apply))
            perform("segment_cleanup", lambda: cleanup_segments(apply))
            perform("artifact_cleanup", lambda: cleanup_obsolete_extractions(s3, bucket))
            perform("source_cleanup", lambda: cleanup_source_packs(s3, bucket))
    print(result, flush=True)
    return result
