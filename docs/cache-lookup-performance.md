# Cached indexing slowdown, September 11, 2026

The live index workers were spending most of their time on database access
while reusing existing embeddings. A sample of the last 100 completed/error
metrics contained 26,249 cache hits, no recorded cache misses or encoding calls,
and a median invocation duration of 127.37 seconds. There were 97 completed
attempts and three errors; this sample does not establish their error causes.
Inclusive timing spans overlap and must not be added together.

At 19:28 UTC, the current `sessions` root had about 1,480 of 5,369 files
published. Nearly 3,000 files still awaited transformation. All four observed
shared worker database backends were executing embedding-cache lookups in a
later snapshot. Slow indexing queries therefore also competed with transformation
for database capacity. The existing [role diagram and deployment table](architecture-and-functionality.md#per-file-pipeline-deployment-roles)
describe the independently deployed workers and their handoffs.

## Cause and change

The live cache had 19,045 active pack directories containing 5,190,090 hashes.
The deployed query unnested every candidate directory and checked its entries
against the requested hashes. `EXPLAIN ANALYZE` observed a sequential scan,
19,045 executions of the unnest operation, and 6–8 seconds per sampled lookup.
The stored arrays are large enough to require substantial out-of-line reads.
An ordinary many-hash array-overlap predicate also chose a sequential scan;
the 512-hash probe exceeded its 15-second statement timeout.

The patch in `modal/embedding_artifacts.py` makes a single database request
containing an indexed array-containment probe for each requested hash. A
`LATERAL` subquery with `OFFSET 0` preserves the per-hash lookup; without that
boundary, the observed planner flattened the join and scanned the directory
again. Keys are deduplicated before retrieving their arrays. All matching
packs remain available to the existing deterministic cache-selection logic.
The query still scopes matches by organization, model revision and retirement
state. No database schema, pool size, worker count, model or cache format changes.

## Read-only production query comparison

Sequential probes ran against the same live database and requested hashes at
19:34 UTC. Times include client round trips and result transfer. These are
diagnostics against existing metadata, not synthetic E2E performance tests or
a deployed throughput comparison. Concurrent production load was not controlled.

| Requested hashes | Previous query | Patched query | Returned packs |
| --- | ---: | ---: | ---: |
| 32 cached, one pack | 5.7754 s | 0.2134 s | 1 |
| 512 cached, one pack | 5.6465 s | 0.9189 s | 1 |
| 512 cached, spread across packs | 5.2612 s | 1.1721 s | 26 |
| 512 absent | 5.8696 s | 0.8954 s | 0 |

Both queries returned identical object keys, content-hash arrays and dimensions
in all four comparisons. The observed improvement was 4.5–27.1 times for these
queries. It is not an estimate of end-to-end indexing speedup. The planner may
still reasonably choose a sequential scan for a very small catalog.

## Validation and rollout

Cloud E2E run `48fef2f8873c4c0e98f8094bd8c9a9c7` passed using the production
CLI/API/consumers in Docker Compose, isolated real cloud Postgres and AWS
S3/SQS, real Turbopuffer, and one A10 index worker on Modal. Four concurrent
inputs were observed in `eu-paris-1`, with encoder batches of 32. Its four
synthetic files contained 968 chunks. Cold indexing recorded 968 cache misses;
forced reindexing recorded 968 cache hits and no encoder calls. Every job
completed on its first attempt. Persisted vector bytes, exact source/line reads,
full-text/vector/hybrid search and cleanup passed. Capture-to-publication times
were 53.651 seconds cold and 18.943 seconds warm; this small isolated catalog
does not reproduce the production cache size or establish a whole-system speedup.

The index recovery E2E (`pufferfs-index-recovery-local-56668`) also passed:
lost-response worker SIGKILL/replay, a real Postgres restart with existing
worker pools and cached vectors, consumer termination/replacement admission,
updates and tombstones during live publication, late stale writes after a
worker crash, deleted-root access denial, and scheduled artifact/search cleanup.
Test-resource cleanup passed. This suite uses Postgres 17, LocalStack S3/SQS,
CPU Nomic and separate Compose processes with real Turbopuffer; it does not
establish GPU crash recovery, provider-based document extraction, or the full
authorization matrix. See the [E2E environment documentation](../tests/e2e/README.md)
for the adapters and network boundaries. `git diff --check` passed.

These checks preceded production deployment. No end-to-end production speedup
is inferred from the query probes or isolated E2E measurements.
Private diagnostic logs and aggregate comparison results are archived under
`~/.codex/artifacts/pufferfs-cache-lookup-20260911/`; no source contents or
credentials are part of the aggregate query report.
