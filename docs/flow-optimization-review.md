# Flow optimization review — September 23, 2026 PDT

This records the original baseline. The subsequent implementation and
E2E results are tracked in [the implementation report](flow-optimization-implementation.md).

Reviewed `b1a5887` and read-only production state. These are proposed changes,
not implemented optimizations. No provider benchmark, recovery, re-extraction,
or production mutation was run for this review. The September 11 scaling
reviews describe an older deployment; their SQS/GPU/cache findings should not
be applied to the current system without checking the code again.

## Current deployment

```text
Local agent --capture/status/search/read--> API server [ECS, 2 tasks]
    |                                          |
    +--signed source uploads--> S3             +--catalog/work--> Postgres
                                |                                  |
                                | sources           claim transform work
                                v                                  v
                         Ingestion worker [ECS, 1 task / 4 file slots]
                                |                    |
                                | chunks -> S3       +--> extraction provider
                                |                         [external black box]
                                +--advance file_work--> Postgres
                                                           |
                                                   claim index work
                                                           v
                         Background worker [ECS, 1 task / 4 index slots]
                                | collection + maintenance loops
                                +--read chunks--> S3
                                +--write text--> Turbopuffer [external black box]
                                +--publish completed head--> Postgres
```

The API enqueues work transactionally in `file_work`; workers claim rows with
expiring ownership and ECS starts the containers. The background role also
polls external extraction results, assembles chunks and cleans obsolete data.
Gemini and the optional Modal vision endpoint remain external black boxes.

## Evidence and priorities

### 1. Operational recovery and useful progress — first

Production currently has 2,702 complete and 2,728 failed latest extractions for
non-deleted, current file versions. The failed set references 3,240,885 old
chunks. No current-version transform/index work was pending or running in this
snapshot. This is exhausted work, not a backlog that more workers will drain.
Retrying old index artifacts also does not apply the newer base64 replacement;
that requires fresh extraction.

The failed-work CloudWatch alarm is `ALARM`, with `AlarmActions=[]`. Both work
alarms have no notification actions. Connect an intended notification channel,
provide explicit bounded recovery, and report accepted-to-searchable age,
completed chunks, accepted embedding tokens, repeated tokens and HTTP 429s.
Do not reset retry limits on every deployment or start an unbounded paid replay.

The current metrics report per-file spans and declared total chunks, while
indexing ignores the provider write response. The backlog age is based on
`next_attempt_at` and excludes provider-waiting work from its age calculation;
it is not total user wait time. Loop freshness also needs monitoring: the
process heartbeat alone does not prove collection/maintenance is progressing.

Evidence: [metrics](../workers/worker_metrics.py),
[backlog reporting](../workers/maintenance.py),
[alarms](../infra/pulumi/index.ts), [status](../cmd/pufferfs/capture_status.go).

### 2. Checkpoint successful index batches and drain on shutdown — high

Every index attempt reads the chunk artifact from the beginning and sends all
batches before publishing. If a later batch fails after SDK retries, the next
file attempt resends earlier successful writes. The existing real-provider
benchmark measured 193,080 repeated embedding tokens: 7.6% extra in one small
64-document reindex run. This is an observed example, not a universal saving.

Persist a confirmed contiguous chunk offset tied to the immutable artifact,
row format and live lease. Resume after that offset. A response lost before a
durable checkpoint remains ambiguous and may require replay; this does not
promise exactly-once billing. Record positions rather than batch numbers so a
batch-size configuration change cannot change the checkpoint's meaning.

Also stop at a safe checkpoint on SIGTERM. Today shutdown sets a flag checked
between files, while ECS gives a container 120 seconds before termination. A
long file can therefore be killed during deployment, wait for its lease to
expire, and replay from the beginning.

Evidence: [index loop](../workers/index_worker.py),
[runtime](../workers/runtime.py), [retry/lease logic](../workers/file_runtime.py),
[measured replay](indexing-performance.md).

### 3. Share provider capacity fairly — high

The database orders work by due time and ID. Each worker independently claims
a file and lets the SDK retry provider calls. There is no shared token/request
budget or per-organization allocation across replicas. Four large files can
occupy every index slot; a bulk import can delay another tenant's small update.

Keep 64-document writes as the tested baseline. Add bounded admission based on
estimated tokens, requests and in-flight bytes, a shared cooldown after 429s,
and fair scheduling across tenants. Reserve interactive query capacity too.
Permanent request errors should fail with actionable reasons; throttling
should wait within a finite recovery budget instead of consuming the same
attempt policy as malformed data. Coordinate the SDK and job retry layers.

Turbopuffer documents organization/model limits and whole-request admission;
the default for new organizations is 2M tokens/minute and 1,024 requests/minute.
The account's actual quota is not confirmed. Raising it remains necessary if
the desired sustained rate exceeds it. [Provider contract](https://turbopuffer.com/docs/embedding).

### 4. Bound execution by segments while publishing whole files — high for large files

Extraction writes one gzip artifact, hashes it, then uploads it. Indexing starts
after extraction finishes. Each entire file owns one execution slot and its
index writes are sequential. Memory buffers are bounded, but retry work,
temporary disk and slot occupancy still scale with file size.

Use independently readable, bounded segment artifacts and a durable manifest
of ordered ranges/counts/hashes. Schedule segments fairly, checkpoint each,
then publish one file head only after every segment and full-source validation
succeed. This reduces retry scope and prevents a few huge files monopolizing
slots. It does not increase a shared provider quota. Starting paid indexing
before whole-source verification also needs an explicit wasted-work tradeoff.

Evidence: [artifact IO](../workers/source_io.py),
[transformation](../workers/transform_worker.py),
[publication](../workers/index_publish.py).

### 5. Make sync and status proportional to changes — high at large file counts

Every sync enumerates the remote catalog, loads a local head file for each
active entry, and walks the local filesystem. Watch events set a root-wide dirty
flag rather than retaining changed paths. Status also walks every remote file,
including when waiting on a subset. The CLI requests 500 entries/page: one
million catalog entries require approximately 2,000 page requests per pass,
even to report a few counters. Paging bounds each response, not total work.

Add an authorized summary/status endpoint, a durable catalog change cursor,
and local changed-path tracking. Keep initial/full reconciliation and watcher
overflow recovery. Cursor design must handle concurrent commits without
skipping changes; subset waiting must still validate the selected versions.

Evidence: [sync](../cmd/pufferfs/capture_sync.go),
[discovery](../cmd/pufferfs/sync_capture.go), [watcher](../cmd/pufferfs/watch.go),
[catalog paging](../cmd/pufferfs/capture_client.go),
[status](../cmd/pufferfs/capture_status.go).

### 6. Extend append reuse through extraction and indexing — high for growing logs

Current append reuse saves source uploads, but the worker reads and verifies
the complete source, extracts all chunks and writes new extraction-specific
rows. A small suffix appended repeatedly to a large log causes repeated
processing of its history. Over many constant-size appends this can approach
quadratic cumulative work in the number of appends.

For formats with a proven append contract, retain a verified parse/chunk
boundary and reuse unchanged output. Reprocess the unfinished tail plus new
bytes. Stable chunk reuse needs a deliberate version/publication and cleanup
design; merely skipping earlier writes would leave the new extraction missing
rows. Account for partial lines, UTF-8 and base64 markers crossing boundaries.
Rewrites and unsupported formats retain full processing. Keep source packing
and verified append reuse.

Evidence: [verified prefix reuse](../internal/sourcecapture/append.go),
[transform](../workers/transform_worker.py),
[extraction-scoped row IDs](../workers/index_mutations.py).

### 7. Pipeline uploads with bounded concurrency — medium

Source packs are uploaded sequentially; multipart parts also advance one at a
time. On a high-latency connection, network round trips can leave bandwidth
unused. Benchmark a small bounded upload pool and overlap packing/uploading
while retaining a single durable journal writer and exact acknowledgments.
Preserve checksums, append verification and restart recovery. No upload speedup
was measured in this review.

Evidence: [pack uploads](../cmd/pufferfs/capture_journal.go),
[multipart uploads](../cmd/pufferfs/capture_multipart.go).

### 8. Keep collection and cleanup capacity bounded and independent — medium/high at scale

Collection, assembly and provider cleanup take turns in one loop. A large
assembly can delay all other collection work in that process. Maintenance
runs root, index, artifact and source cleanup sequentially; a top-level failure
can prevent later classes from running that pass. Give those classes separate
failure boundaries and service budgets; add separately scalable roles only
when measurements justify them.

Root cleanup removes at most 1,000 objects from a prefix, then partial progress
waits five minutes. At that cadence, a million-object prefix takes roughly
3.5 days even with instant operations. Drain multiple pages within a bounded
time/byte budget and resume successful partial work promptly and fairly.

Successful index and artifact cleanup is rescheduled daily. Obsolete artifact
cleanup handles only five targets per invocation, including revisits. This
recurring demand grows with retained history. Prefer newly dirty generations
and cheaper batched checks, but keep safety rescans until old external writes
are provably unable to land. A lease expiry is not proof that an already
submitted external write has settled.

Evidence: [collection](../workers/collection.py),
[maintenance](../workers/maintenance.py), [root cleanup](../workers/root_cleanup.py),
[index cleanup](../workers/index_cleanup.py),
[artifact cleanup](../workers/artifact_cleanup.py).

### 9. Bound aggregate search work — medium as tenants/query traffic grow

Search limits namespace concurrency to 16 per HTTP request. Multiple requests
multiply that limit across API replicas. Each namespace may be queried again
when uncommitted/obsolete extraction candidates occupy the result ranks.
Add process and tenant admission limits, deadlines and cancellation, measure
publication-filter retry rounds, and offer narrower root scope to callers.
Keep candidate authorization and published-version validation. The current
batched catalog lookup and pinned read snapshot already avoid earlier designs'
root-wide publication scans; preserve those improvements.

Evidence: [search publication](../internal/server/search_publication.go),
[query routing](../internal/server/query_namespaces.go),
[file reads](../internal/server/file_read_rows.go).

### 10. Optimize hosting and deployment from measured load — medium

Both worker tasks currently reserve 2 vCPUs/4 GiB. The sampled idle hour had
average worker CPU below 0.3% and memory below 3.5%. That supports investigating
idle cost, not sizing for PDF/video/large-file peaks from idle measurements.
Benchmark per-role memory/CPU at load, right-size independently, then consider
backlog-age scaling with provider and database admission caps. Maintain a way
to wake workers and run collection/maintenance if any role scales to zero.

ECS runs in AWS us-west-2; the worker task has no region override and the TP
client defaults to gcp-us-central1. Benchmark Oregon placement before migrating
an existing namespace. Region changes require a data migration, and same-cloud
public endpoints do not automatically eliminate egress costs.
[Provider region guidance](https://turbopuffer.com/docs/regions).

Deployment verification should assert expected live roles and retirement
postconditions, then run an isolated capture-to-search/read/delete canary.
An HTTP health response alone would not detect the orphan Modal workers we
just fixed. These canaries complement the existing full E2Es.

## Suggested implementation order and validation

Preserve the existing improvements: packed source/manifest uploads, verified
append upload reuse, transactional capture plus work registration, bounded DB
pools, 64-document embedding requests, batched search publication lookups and
atomic per-file publication. Capture persists manifests before its short
database commit; it does not hold that transaction over extraction/indexing.
There is no evidence here that replacing Postgres or restoring the removed
SQS/Modal CPU layers would address the measured bottleneck.

1. Notification wiring, per-batch progress/usage, failure classification,
   controlled recovery and deployment inventory/canary checks.
2. Confirmed-write checkpoints, safe shutdown and shared provider admission.
3. Summary/status endpoint and incremental catalog/watch behavior.
4. Bounded segments, followed by append output reuse for supported formats.
5. Upload pipelining, collection/cleanup isolation, query admission and measured
   infrastructure adjustments.

Use production-process E2Es with synthetic isolated resources and real
providers. Extend coverage for late batch failure, lost responses, worker
termination around checkpoints, deployment shutdown, two competing tenants,
concurrent catalog changes, watcher overflow, append-boundary edits and
late writes after deletion. Verify source/read/search/update/delete and actual
provider request counts. Existing E2Es remain necessary, but do not establish
the new checkpoint, fairness, change-cursor or incremental-append contracts.

The largest demonstrated current waste is repeated provider work. The largest
structural opportunities are whole-file execution and whole-catalog scans.
Their potential gains are workload-dependent; this review does not establish
another 10x throughput improvement.
