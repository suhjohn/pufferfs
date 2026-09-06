"""The same deterministic namespace routing for publication and cleanup."""

import hashlib


def namespace_for_path(namespaces, path):
    if not namespaces:
        raise ValueError("root has no active index namespaces")
    count = namespaces[0]["shard_count"]
    if not 1 <= count <= 256 or len(namespaces) != count:
        raise ValueError("invalid namespace shard count")
    by_shard = {row["shard_index"]: row["namespace"] for row in namespaces}
    if set(by_shard) != set(range(count)) or any(row["shard_count"] != count for row in namespaces):
        raise ValueError("incomplete namespace shards")
    shard = int.from_bytes(hashlib.sha256(path.encode()).digest()[:8], "big") % count
    return by_shard[shard]
