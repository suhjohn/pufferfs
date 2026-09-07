"""Summarize isolated worker-throughput runs from their sanitized console logs.

Usage: python3 scripts/worker-throughput-report.py BASELINE.log OPTIMIZED.log
Only work IDs in a successfully validated benchmark phase are included.
"""

import json
import math
from pathlib import Path
import re
import statistics
import sys


def report(path):
    jobs, phases = {}, []
    for line in Path(path).read_text().splitlines():
        line = re.sub(r"\x1b\[[0-9;]*m", "", line)
        start = line.find('{"event":')
        if start < 0:
            continue
        try:
            row, _ = json.JSONDecoder().raw_decode(line[start:])
        except json.JSONDecodeError:
            continue
        if row.get("event") == "file_work_metrics" and row["status"] == "complete":
            jobs[row["work_id"]] = row
        elif row.get("event") == "worker_throughput":
            phases.append(row)
    if not phases:
        raise ValueError(f"No validated benchmark phase in {path}")
    for phase in phases:
        for stage in ("transform", "index"):
            work = [row for row in phase["work"] if row["stage"] == stage]
            measured = [jobs[row["work_id"]] for row in work]
            durations = sorted(row["total_seconds"] for row in measured)
            totals, counts = {}, {}
            for row in measured:
                for name, value in row["counts"].items():
                    counts[name] = counts.get(name, 0) + value
                for name, seconds in row["seconds"].items():
                    totals[name] = totals.get(name, 0) + seconds
            count = sum(row["chunk_count"] for row in work)
            print(json.dumps({"log": str(path), "run_id": phase["run_id"], "phase": phase["phase"],
                "stage": stage, "files": len(work), "chunks": count,
                "worker_seconds": round(sum(durations), 3),
                "chunks_per_worker_second": round(count / sum(durations), 2),
                "median_file_seconds": round(statistics.median(durations), 3),
                "p95_file_seconds": durations[math.ceil(.95 * len(durations)) - 1],
                "regions": sorted({row["region"] for row in measured}),
                "application_db_statements": counts.get("db_statements", 0) - counts.get("db_health_checks", 0),
                "counts": counts,
                "inclusive_phase_seconds": {k: round(v, 3) for k, v in totals.items()},
                "mutation_payload": {k: phase[k] for k in
                    ("mutation_records", "mutation_json_bytes", "vector_json_bytes") if k in phase}
                    if stage == "index" else {},
                "capture_to_publication_seconds": phase["capture_to_publication_seconds"]}))


if __name__ == "__main__":
    for filename in sys.argv[1:]:
        report(filename)
