"""Summarize completed production capacity windows without exposing file contents.

The input directory contains capacity-events.jsonl, capacity-metrics.jsonl,
production-query-capacity.events.jsonl, production-pufferfs-index-gpu.log and the
resource files named by each completed sweep event. Live backlog comparisons
are observations, not controlled speedup estimates: file sizes and cache hits
can change, and publication occurs only when a whole file finishes.

Optional embedding-pack-uploads.jsonl contains S3 LastModified timestamps as
at, size_bytes and vectors for one cache format. Its matching -metadata.json
must record the inventory's started_at. Scan after the measurement window and
before retention removes its packs; pack uploads are not search publication.
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


def allocation_seconds(inventories, begin, finish, initial_count):
    cursor, count, total = begin, initial_count, 0
    for snapshot in sorted(inventories, key=lambda r: r["at"]):
        if snapshot["at"] > finish:
            break
        if snapshot["at"] > begin:
            total += count * (snapshot["at"] - cursor)
            cursor = snapshot["at"]
        count = len(snapshot["container_ids"])
    return total + count * (finish - cursor)


def worker_metrics(path, event_type="file_work_metrics"):
    result = {}
    for line in path.read_text(errors="replace").splitlines():
        start = line.find('{"event":')
        if start < 0:
            continue
        try:
            row, _ = json.JSONDecoder().raw_decode(line[start:])
        except json.JSONDecodeError:
            continue
        if row.get("event") == event_type and "started_at" in row:
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
    attempt_starts = worker_metrics(directory / "production-pufferfs-index-gpu.log", "file_work_started")
    completion_times = {(r["work_id"], r["started_at"]): r["started_at"] + r["total_seconds"] for r in workers}
    uploads_path = directory / "embedding-pack-uploads.jsonl"
    uploads = rows(uploads_path) if uploads_path.exists() else []
    upload_metadata = json.loads(uploads_path.with_name("embedding-pack-uploads-metadata.json").read_text()) if uploads else {}
    starts, phases, requested = {}, [], {}
    configuration_fields = ("consumer_replicas", "consumer_concurrency", "max_inputs",
                            "batch", "commit", "database_settings", "cpu_physical_cores",
                            "memory_mib", "gpu", "sampling_mode")
    for event in events:
        key = event.get("measurement_id", event.get("containers"))
        if event.get("event") == "capacity_sweep_requested":
            requested[key] = {k: event[k] for k in configuration_fields if k in event}
        elif event.get("event") == "capacity_sweep_started":
            starts[key] = {**requested.get(key, {}), **event}
        elif event.get("event") == "capacity_sweep_measurement_started":
            # An explicit readiness observation can exclude rollout drain time
            # while retaining the original resource samples and event history.
            phase = starts[key]
            phase["allocation_ready_at"] = phase["at"]
            phase["at"] = event["at"]
            phase["warmup_exclusion_reason"] = event["reason"]
        elif event.get("event") == "capacity_sweep_caveat":
            starts[key].setdefault("caveats", []).append(event["reason"])
        elif event.get("event") == "capacity_sweep_finished":
            phases.append((starts.pop(key), event))
    if not phases:
        raise SystemExit("No completed capacity measurement window yet")
    for start, end in phases:
        begin, finish = timestamp(start["at"]), timestamp(end["at"])
        duration = finish - begin
        samples, settings, inventories = [], [], []
        for filename in end["resource_files"]:
            # Resolve by basename so private artifact directories can be moved.
            resource = rows(directory / Path(filename).name)
            settings.extend(r for r in resource if r.get("event") == "settings")
            samples.extend(r for r in resource if r.get("event") == "sample" and begin <= r["at"] <= finish)
            inventories.extend(r for r in resource if r.get("event") == "inventory" and begin <= r["at"] <= finish)
        adaptive = start.get("sampling_mode") == "periodic_inventory"
        container_ids = set(end["observed_container_ids"] if adaptive else start["container_ids"])
        assert samples
        assert {r["container"] for r in settings} == set(end["sampled_container_ids"] if adaptive else start["container_ids"])
        assert len(settings) == len({r["container"] for r in settings})
        if not adaptive:
            assert len(settings) == start["containers"]
        gpu_seconds = start["containers"] * duration
        if adaptive:
            assert inventories
            gpu_seconds = allocation_seconds(inventories, begin, finish, start["containers"])
        result = {"start": start["at"], "end": end["at"], "containers": start["containers"],
                  "configuration": {k: start[k] for k in configuration_fields if k in start},
                  "seconds": round(duration, 3), "live_settings": settings,
                  "resource_samples": len(samples),
                  "gpu_util_percent_mean": round(statistics.mean(float(r["gpu"].split(",")[1]) for r in samples), 2),
                  "gpu_memory_mib_peak": max(float(r["gpu"].split(",")[3]) for r in samples),
                  "process_cpu_cores_mean": round(statistics.mean(r["cpu_cores"] for r in samples if r["cpu_cores"] is not None), 3),
                  "process_rss_gib_peak": round(max(r["rss_bytes"] for r in samples) / 2**30, 3)}
        if adaptive:
            result["container_lifecycle"] = {"observed_containers": len(container_ids),
                "final_containers": len(end["final_container_ids"]),
                "peak_inventory": max(len(r["container_ids"]) for r in inventories),
                "sampled_gpu_allocation_seconds": round(gpu_seconds, 3),
                "probe_count": end["probe_count"], "unavailable_probes": end["unavailable_probes"],
                "resource_coverage_fraction": end["resource_coverage_fraction"]}
        if "allocation_ready_at" in start:
            result["allocation_ready_at"] = start["allocation_ready_at"]
            result["warmup_exclusion_reason"] = start["warmup_exclusion_reason"]
        if "caveats" in start:
            result["caveats"] = start["caveats"]
        measured = [r for r in workers if r["container"] in container_ids
                    and begin <= r["started_at"] and r["started_at"] + r["total_seconds"] <= finish]
        finished_attempts = [r for r in workers if r["container"] in container_ids
                             and begin <= r["started_at"] + r["total_seconds"] <= finish]
        result["finished_attempt_statuses"] = dict(Counter(r["status"] for r in finished_attempts))
        # Count publication across all roots served by this allocation, including
        # jobs that started before the window. An already-complete duplicate
        # returns without source/chunk counts and contributes no new publication.
        published_work = {r["work_id"]: r for r in finished_attempts
                          if r["status"] == "complete" and "chunks" in r["counts"]}
        published_chunks = sum(r["counts"]["chunks"] for r in published_work.values())
        published_bytes = sum(r["counts"]["source_bytes"] for r in published_work.values())
        result["publication_from_worker_metrics"] = {
            "files": len(published_work), "chunks": published_chunks, "source_bytes": published_bytes,
            "chunks_per_second": round(published_chunks / duration, 3),
            "source_bytes_per_second": round(published_bytes / duration, 3)}
        admitted = [r for r in attempt_starts if r["container"] in container_ids
                    and r["started_at"] <= finish
                    and completion_times.get((r["work_id"], r["started_at"]), math.inf) >= begin]
        if admitted:
            admission_changes = defaultdict(list)
            for row in admitted:
                ended = completion_times.get((row["work_id"], row["started_at"]), math.inf)
                admission_changes[row["container"]].extend([
                    (max(begin, row["started_at"]), 1), (min(finish, ended), -1)])
            peaks = {}
            for container, changes in admission_changes.items():
                current = peak = 0
                for _, change in sorted(changes):
                    current += change
                    peak = max(peak, current)
                peaks[container] = peak
            result["peak_executing_attempts_from_start_events"] = peaks
            final_ids = set(end["final_container_ids"]) if adaptive else container_ids
            result["attempts_open_at_window_end"] = sum(
                completion_times.get((r["work_id"], r["started_at"]), math.inf) > finish
                and r["container"] in final_ids for r in admitted)
            if adaptive:
                result["unfinished_attempts_on_departed_containers"] = sum(
                    completion_times.get((r["work_id"], r["started_at"]), math.inf) > finish
                    and r["container"] not in final_ids for r in admitted)
            known_starts = {(r["work_id"], r["started_at"]) for r in admitted}
            result["finished_attempts_missing_start_event"] = sum(
                (r["work_id"], r["started_at"]) not in known_starts for r in finished_attempts)
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
        if uploads and timestamp(upload_metadata["started_at"]) >= finish:
            produced = [r for r in uploads if begin <= timestamp(r["at"]) <= finish]
            vectors = sum(r["vectors"] for r in produced)
            result["cache_pack_uploads"] = {"packs": len(produced), "vectors": vectors,
                "bytes": sum(r["size_bytes"] for r in produced),
                "vectors_per_second": round(vectors / duration, 3)}
            if args.gpu_second_price is not None and vectors:
                cost = gpu_seconds * args.gpu_second_price
                result["cache_pack_uploads"]["gpu_dollars_per_million_uploaded_vectors"] = round(cost * 1e6 / vectors, 3)
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
                allocated = allocation_seconds(inventories, timestamp(first["at"]), timestamp(last["at"]),
                                               start["containers"]) if adaptive else start["containers"] * wall
                cost = allocated * args.gpu_second_price
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
