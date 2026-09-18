"""Summarize validated E2E phases and their real-provider relay observations.

Usage: python3 scripts/embedding-throughput-report.py ARTIFACT_DIRECTORY
The network snapshots are cumulative; subtract earlier observations per run.
No document content or credentials are read by this report.
"""

from collections import Counter
import json
from pathlib import Path
import sys


def report(directory):
    directory = Path(directory)
    phases = {}
    for line in (directory / "worker-throughput.jsonl").read_text().splitlines():
        phase = json.loads(line)
        phases[(phase["run_id"], phase["phase"])] = phase
    observed = {}
    for line in (directory / "worker-throughput-network.jsonl").read_text().splitlines():
        snapshot = json.loads(line)
        run_id = snapshot["run_id"]
        phase = phases[(run_id, snapshot["phase"])]
        seen = observed.setdefault(run_id, set())
        writes = [event for event in snapshot["events"]
                  if event["id"] not in seen and event["operation"] == "write"
                  and event.get("upsert_count", 0)]
        seen.update(event["id"] for event in snapshot["events"])
        tokens = sum(event.get("response_metadata", {}).get("performance", {}).get("embedding_tokens", 0)
                     for event in writes)
        payloads, repeated = set(), []
        for event in writes:
            if event.get("upstream_status") != 200:
                continue
            if event["payload_sha256"] in payloads:
                repeated.append(event)
            payloads.add(event["payload_sha256"])
        elapsed = phase["capture_to_publication_seconds"]
        print(json.dumps({
            **{key: value for key, value in phase.items() if key not in {"work", "event"}},
            "chunks_per_minute": round(60 * phase["chunks"] / elapsed, 1),
            "accepted_embedding_tokens": tokens,
            "repeated_successful_writes": len(repeated),
            "repeated_embedding_tokens": sum(event["response_metadata"]["performance"].get("embedding_tokens", 0) for event in repeated),
            "accepted_tokens_per_minute": round(60 * tokens / elapsed),
            "write_http_status_counts": dict(Counter(str(event.get("upstream_status", event["state"])) for event in writes)),
            "file_attempts": sum(row["attempt_count"] for row in phase["work"]),
        }))


if __name__ == "__main__":
    report(sys.argv[1])
