"""Read-only, per-root verification of every retained captured source version.

Run as an operator command, not a Modal deployment or a queued job. Reports
JSONL metadata, never source bodies. Only a final verified summary is success.
"""

from __future__ import annotations

import argparse
from collections import OrderedDict
from contextlib import contextmanager
import json
import math
import os
from pathlib import Path
import re
import sys
import tempfile
import time

from file_runtime import database
from source_io import READ_BYTES, iter_source_bytes, read_manifest


@contextmanager
def read_catalog(connect):
    with connect() as conn, conn.transaction():
        conn.execute("SET TRANSACTION READ ONLY")
        conn.execute("SET LOCAL statement_timeout='15s'")
        yield conn


def inventory(connect, org_id, root_id):
    with read_catalog(connect) as conn:
        row = conn.execute("""SELECT COUNT(v.id) AS versions,COALESCE(MAX(v.sequence),0) AS last_sequence
            FROM roots r LEFT JOIN file_catalog f ON f.root_id=r.id
            LEFT JOIN file_versions v ON v.file_id=f.id
            WHERE r.org_id=%s AND r.id=%s AND r.deleting_at IS NULL GROUP BY r.id""",
            (org_id, root_id)).fetchone()
    if row is None:
        raise ValueError("source root is missing, belongs to another organization, or is deleting")
    return row


class SourcePackCache:
    """Bounded private scratch space: packed files share a GET, not one per extent."""

    def __init__(self, s3, bucket, directory, deadline, *, capacity=256 << 20):
        self.s3, self.bucket, self.directory, self.deadline = s3, bucket, directory, deadline
        self.capacity, self.used = capacity, 0
        self.allowed_sizes = {}
        self.entries = OrderedDict()
        self.pack_gets = self.pack_bytes_read = 0

    def check_deadline(self):
        if time.monotonic() >= self.deadline:
            raise TimeoutError("source verification deadline exceeded")

    def get_object(self, *, Bucket, Key, Range=None):
        self.check_deadline()
        if Bucket != self.bucket:
            raise ValueError("source bucket mismatch")
        if Range is None:
            return self.s3.get_object(Bucket=Bucket, Key=Key)  # Small manifest, checked by source_io.
        size = self.allowed_sizes.get(Key)
        match = re.fullmatch(r"bytes=(\d+)-(\d+)", Range)
        if type(size) is not int or not 0 < size <= min(128 << 20, self.capacity) or match is None:
            raise ValueError("unregistered, incomplete or oversized source pack")
        start, end = map(int, match.groups())
        if not 0 <= start <= end < size:
            raise ValueError("source extent exceeds registered pack bounds")
        if Key not in self.entries:
            while self.used + size > self.capacity:
                _, (old_path, old_size) = self.entries.popitem(last=False)
                old_path.unlink()
                self.used -= old_size
            self.pack_gets += 1
            response = self.s3.get_object(Bucket=Bucket, Key=Key)
            target = None
            try:
                with response["Body"] as body, tempfile.NamedTemporaryFile(dir=self.directory, delete=False) as output:
                    target = Path(output.name)
                    remaining = size
                    while remaining:
                        self.check_deadline()
                        data = body.read(min(READ_BYTES, remaining))
                        if not data or len(data) > remaining:
                            raise ValueError("stored source pack size mismatch")
                        output.write(data)
                        remaining -= len(data)
                        self.pack_bytes_read += len(data)
                    if body.read(1):
                        raise ValueError("stored source pack exceeds registered size")
            except BaseException:
                if target is not None:
                    target.unlink(missing_ok=True)
                raise
            self.entries[Key] = target, size
            self.used += size
        target, cached_size = self.entries[Key]
        if cached_size != size:
            raise ValueError("source pack metadata changed during verification")
        self.entries.move_to_end(Key)
        body = target.open("rb")
        body.seek(start)
        return {"Body": body}  # iter_source_bytes bounds each read to its requested extent.


def verify_sources(s3, bucket, org_id, root_id, emit, *, connect=database, timeout=900):
    if not org_id or not root_id or not bucket or not math.isfinite(timeout) or timeout <= 0:
        raise ValueError("organization, root, bucket and a positive timeout are required")
    deadline = time.monotonic() + timeout
    before = inventory(connect, org_id, root_id)
    cursor = verified = tombstones = failed = source_bytes = 0
    with tempfile.TemporaryDirectory(prefix="pufferfs-source-verify-") as directory:
        cache = SourcePackCache(s3, bucket, directory, deadline)
        while True:
            cache.check_deadline()
            with read_catalog(connect) as conn:
                versions = conn.execute("""SELECT v.id,v.sequence,v.content_hash,v.size_bytes,
                    v.source_manifest_ref,v.deleted,f.path,f.root_id,r.org_id
                    FROM file_versions v JOIN file_catalog f ON f.id=v.file_id JOIN roots r ON r.id=f.root_id
                    WHERE r.org_id=%s AND r.id=%s AND r.deleting_at IS NULL AND v.sequence>%s AND v.sequence<=%s
                    ORDER BY v.sequence LIMIT 64""", (org_id, root_id, cursor, before["last_sequence"])).fetchall()
            if not versions:
                break
            for version in versions:
                cache.check_deadline()
                record = {"type": "version", "version_id": version["id"], "path": version["path"],
                          "sequence": version["sequence"], "source_manifest_ref": version["source_manifest_ref"],
                          "content_hash": version["content_hash"], "size_bytes": version["size_bytes"]}
                cursor = version["sequence"]
                if version["deleted"]:
                    tombstones += 1
                    emit(dict(record, status="tombstone"))
                    continue
                try:
                    manifest = read_manifest(cache, bucket, version)
                    keys = list({extent["object_key"] for extent in manifest.get("extents") or []})
                    with read_catalog(connect) as conn:
                        objects = conn.execute("""SELECT object_key,size_bytes FROM source_objects
                            WHERE org_id=%s AND root_id=%s AND completed_at IS NOT NULL AND object_key=ANY(%s)""",
                            (org_id, root_id, keys)).fetchall()
                    cache.allowed_sizes = {obj["object_key"]: obj["size_bytes"] for obj in objects}
                    if set(cache.allowed_sizes) != set(keys):
                        raise ValueError("source pack ownership/completion metadata is missing")
                    for _ in iter_source_bytes(cache, bucket, manifest):
                        cache.check_deadline()
                except TimeoutError:
                    raise  # No successful final summary for an interrupted audit.
                except Exception as error:
                    failed += 1
                    # SDK/connection exceptions can contain credentials or URLs.
                    record.update(status="failed", error_type=type(error).__name__)
                    if type(error) is ValueError:
                        record["detail"] = str(error)
                else:
                    verified += 1
                    source_bytes += version["size_bytes"]
                    record["status"] = "verified"
                emit(record)
        after = inventory(connect, org_id, root_id)
        cache.check_deadline()
        complete = before == after and verified + tombstones + failed == before["versions"]
        status = "verified" if complete and failed == 0 and before["versions"] else "incomplete"
        summary = {"type": "summary", "status": status, "org_id": org_id, "root_id": root_id,
                   "catalog_unchanged": before == after, "last_sequence": before["last_sequence"],
                   "version_count": before["versions"], "verified_versions": verified, "tombstones": tombstones,
                   "failed_versions": failed, "source_bytes_verified": source_bytes,
                   "pack_gets": cache.pack_gets, "pack_bytes_read": cache.pack_bytes_read}
    emit(summary)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--org-id", required=True)
    parser.add_argument("--root-id", required=True)
    parser.add_argument("--bucket", default=os.getenv("AWS_BUCKET_NAME"))
    parser.add_argument("--timeout", type=float, default=900)
    args = parser.parse_args()
    if not args.bucket or not math.isfinite(args.timeout) or args.timeout <= 0 or not os.getenv("DATABASE_URL"):
        parser.error("DATABASE_URL, --bucket/AWS_BUCKET_NAME and a positive --timeout are required")
    def emit(record):
        print(json.dumps(record, ensure_ascii=False, allow_nan=False), flush=True)
    try:
        import boto3
        from botocore.config import Config
        s3 = boto3.client("s3", config=Config(connect_timeout=10, read_timeout=30, retries={"total_max_attempts": 3}))
        summary = verify_sources(s3, args.bucket, args.org_id, args.root_id, emit, timeout=args.timeout)
        return int(summary["status"] != "verified")
    except Exception as error:
        emit({"type": "aborted", "error_type": type(error).__name__})
        return 1


if __name__ == "__main__":
    sys.exit(main())
