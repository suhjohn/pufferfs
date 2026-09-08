"""Summarize completed production capacity windows without exposing file contents.

The input directory contains capacity-events.jsonl, capacity-metrics.jsonl,
production-query-capacity.events.jsonl, production-pufferfs-index-gpu.log and the
resource files named by each completed sweep event. Live backlog comparisons
are observations, not controlled speedup estimates: file sizes and cache hits
can change, and publication occurs only when a whole file finishes.
"""

import argparse
from collections import Counter, defaultdict
import datetime
import json
import math
from pathlib import Path
import statistics


def timestamp(value):
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def rows(path):
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def percentile(values, fraction):
    return sorted(values)[max(0, math.ceil(fraction * len(values)) - 1)] if values else None


def worker_metrics(path):
    result = {}
    for line in path.read_text(errors="replace").splitlines():
        start = line.find('{"event":')
        if start < 0:
            continue
        try:
            row, _ = json.JSONDecoder().raw_decode(line[start:])
        except json.JSONDecodeError:
            continue
        if row.get("event") == "file_work_metrics" and "started_at" in row:
            result[row["work_id"], row["started_at"]] = row
    return list(result.values())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--gpu-second-price", type=float,
                        help="Explicit current GPU price, excluding CPU/memory/network/startup")
    args = parser.parse_args()
    if args.gpu_second_price is not None and args.gpu_second_price <= 0:
        parser.error("GPU price must be positive")
    directory = args.directory
    events = rows(directory / "capacity-events.jsonl")
    observations = rows(directory / "capacity-metrics.jsonl")
    queries = rows(directory / "production-query-capacity.events.jsonl")
    workers = worker_metrics(directory / "production-pufferfs-index-gpu.log")
    starts, phases = {}, []
    for event in events:
        if event.get("event") == "capacity_sweep_started":
            starts[event["containers"]] = event
        elif event.get("event") == "capacity_sweep_finished":
            phases.append((starts.pop(event["containers"]), event))
    if not phases:
        raise SystemExit("No completed capacity measurement window yet")
    for start, end in phases:
        begin, finish = timestamp(start["at"]), timestamp(end["at"])
        duration = finish - begin
        samples, settings = [], []
        for filename in end["resource_files"]:
            # Resolve by basename so private artifact directories can be moved.
            resource = rows(directory / Path(filename).name)
            settings.extend(r for r in resource if r.get("event") == "settings")
            samples.extend(r for r in resource if r.get("event") == "sample" and begin <= r["at"] <= finish)
        assert samples and len(settings) == start["containers"]
        assert {r["container"] for r in settings} == set(start["container_ids"])
        result = {"start": start["at"], "end": end["at"], "containers": start["containers"],
                  "seconds": round(duration, 3), "live_settings": settings,
                  "resource_samples": len(samples),
                  "gpu_util_percent_mean": round(statistics.mean(float(r["gpu"].split(",")[1]) for r in samples), 2),
                  "gpu_memory_mib_peak": max(float(r["gpu"].split(",")[3]) for r in samples),
                  "process_cpu_cores_mean": round(statistics.mean(r["cpu_cores"] for r in samples), 3),
                  "process_rss_gib_peak": round(max(r["rss_bytes"] for r in samples) / 2**30, 3)}
        measured = [r for r in workers if r["container"] in start["container_ids"]
                    and begin <= r["started_at"] and r["started_at"] + r["total_seconds"] <= finish]
        finished_attempts = [r for r in workers if r["container"] in start["container_ids"]
                             and begin <= r["started_at"] + r["total_seconds"] <= finish]
        result["finished_attempt_statuses"] = dict(Counter(r["status"] for r in finished_attempts))
        active = defaultdict(list)
        for row in measured:
            active[row["container"]].extend([(row["started_at"], 1),
                                            (row["started_at"] + row["total_seconds"], -1)])
        peaks = {}
        for container, changes in active.items():
            running = peak = 0
            for _, change in sorted(changes):
                running += change
                peak = max(peak, running)
            peaks[container] = peak
        complete = [r for r in measured if r["status"] == "complete"]
        counts = Counter()
        seconds = Counter()
        for row in complete:
            counts.update(row["counts"])
            seconds.update(row["seconds"])
        result.update(fully_observed_jobs=len(measured), statuses=dict(Counter(r["status"] for r in measured)),
                      peak_inputs_from_fully_observed_jobs=peaks, counts=dict(counts),
                      inclusive_worker_seconds={k: round(v, 3) for k, v in seconds.items()})
        snapshots = [r for r in observations if begin <= timestamp(r["at"]) <= finish and "work" in r]
        if len(snapshots) >= 2:
            first, last = snapshots[0], snapshots[-1]
            wall = timestamp(last["at"]) - timestamp(first["at"])
            def published(row):
                return next(w for w in row["work"] if w["stage"] == "index" and w["status"] == "complete")
            before, after = published(first), published(last)
            delta = {k: after[k] - before[k] for k in ("files", "chunks", "bytes")}
            result["observed_root_publication"] = {"start": first["at"], "end": last["at"],
                "seconds": round(wall, 3), **delta,
                "chunks_per_second": round(delta["chunks"] / wall, 3),
                "source_bytes_per_second": round(delta["bytes"] / wall, 3),
                "max_observed_connections": max(sum(c["count"] for c in r.get("connections", [])) for r in snapshots)}
            if args.gpu_second_price is not None:
                cost = start["containers"] * wall * args.gpu_second_price
                result["observed_root_publication"].update(gpu_allocation_dollars=round(cost, 4),
                    gpu_dollars_per_million_published_chunks=round(cost * 1e6 / delta["chunks"], 3) if delta["chunks"] > 0 else None)
        result["query_latency_seconds"] = {}
        for mode in ("fts", "vector", "hybrid"):
            values = [r["seconds"] for r in queries if r.get("operation") == "query" and r.get("mode") == mode
                      and begin <= timestamp(r["at"]) <= finish]
            result["query_latency_seconds"][mode] = {"samples": len(values),
                "p50": percentile(values, .5), "p95": percentile(values, .95), "max": max(values) if values else None}
        print(json.dumps(result, separators=(",", ":")))


if __name__ == "__main__":
    main()
