# Worker throughput and S3 cache batching

This is a chronological implementation and measurement ledger. Compatibility
notes and migration runs below describe earlier snapshots; current behavior is
defined by the [fresh-schema audit](fresh-schema-audit.md). Old source manifests,
vector-locator backfill and partial index checkpoints are no longer supported.

The API server, transformation consumer/worker, index consumer/worker and
scheduled reconciler remain separately deployable roles. Their queue ownership
and deployment topology are shown in the [role diagram and table](architecture-and-functionality.md#per-file-pipeline-deployment-roles).
This optimization is implemented in the checkout; it has not been deployed to
production. One consumer process can have multiple invocation slots; each slot
has one outstanding worker request. Modal independently starts worker containers
up to the configured cap. Increasing consumer processes does not require a
shared process-local work coordinator.

## Changes

- Python workers reuse a bounded Postgres connection pool instead of establishing
  a TLS connection for every transaction. The default is two connections per
  process, zero reserved minimum, and one-minute idle expiry. Capacity remains
  available for background lease renewal while foreground IO holds a transaction.
- Native text extraction constructs no Gemini client. Completed transformation
  hands off its exact index work ID, avoiding a global pending-work scan. SQS
  delivery confirmations use a batch database update.
- Redundant foreground lease renewals were removed. Background renewals and
  live-lease checks at durable completion/mutation registration remain.
- Each immutable S3 embedding pack holds up to 512 float32 vectors (1.5 MiB for
  the pinned 768-dimensional model). The encoder retains its internal 64-text
  GPU microbatch, so larger storage batches do not enlarge GPU microbatches.
- New vectors create a compact `embedding_packs` directory, with ordered content
  hashes, model revision and dimensions. Their array position locates the S3
  bytes. New workers never insert individual `embedding_locations` rows.

The database still stores hashes and their GIN lookup index, plus work ownership,
publication and cleanup metadata. Five rows does not mean five hashes or a
corresponding reduction in every byte of database storage. Each new cache pack
uses two write statements and one locking SELECT across two transactions:
register its cleanup target, lock the live pack, then publish its directory and
timestamp after successful S3 upload.
Chunks, vector bodies and replayable search mutations remain in S3.

Concurrent cold misses may create overlapping immutable packs. Reads resolve
these deterministically without requiring a single writer; duplicates are safe
and expire through ordinary cache retention. A row lock spans each bounded S3
read/write so retirement cannot remove a pack in use. Cache retirement clears
its hash directory and keeps a tombstone for late-object cleanup.

## Controlled single-worker measurement

Four synthetic JSONL files contained 8, 64, 128 and 768 chunks, totaling 968.
Both runs used one transformation slot in Docker Compose and one bulk GPU slot
on an L4 in Modal's reported `us-west4` region. Postgres, S3 and SQS were isolated
real cloud resources; Turbopuffer and query embedding were real. Each phase
verified captured source bytes, exact line reads and FTS/vector/hybrid search.

| Phase / stage | Baseline chunks/s | Pack + pool chunks/s | Improvement |
| --- | ---: | ---: | ---: |
| Cold cache, transformation | 106.63 | 232.77 | 2.18× |
| Cold cache, indexing | 8.96 | 25.71 | 2.87× |
| Warm cache, transformation | 103.26 | 260.57 | 2.52× |
| Warm cache, indexing | 23.18 | 60.27 | 2.60× |

Rates divide validated chunks by summed worker invocation duration, excluding
queue delay and container startup. They are service rates, not sustained
end-to-end production throughput. Cold capture-to-publication elapsed time was
148.96 → 83.30 seconds; warm elapsed time was 48.85 → 22.73 seconds. Four files
are too few for tail-latency conclusions or claims of a universal optimum.

For the 768-chunk file, indexing took 75.60 → 26.98 seconds while encoding stayed
16.91 → 17.04 seconds. It wrote two cache directories instead of 768 locator
rows; the entire corpus used five directories instead of 968 locators. Across
all four files, cache-store spans fell from 43.00 to 2.38 seconds. Encoding now
accounts for much of the large-file duration.

Baseline run: `ed318869d10e44a8acb1dc8c1de3f980`.
Optimized run: `4065fe05f74746f299b4d229cf51e786`.
The optimized GPU snapshot predates removal of one foreground heartbeat per
embedding batch; measurements above are for that snapshot, not an estimate of
the final source. Its cache/pool behavior is the same. Earlier unpinned runs
landed in different regions and are excluded from this comparison.

## Configuration and migration

`PUFFERFS_WORKER_DB_MAX_CONNS` configures the Python pool (2–16, default 2).
`PUFFERFS_MODAL_WORKER_REGION` optionally pins transformation and bulk GPU
placement at deployment time. Choose a location near database/object storage;
no developer/account-specific location is built into production code.

Migration 041 now replaces the embedding cache schema without converting old
locators. Current vectors use only bounded pack directories; there is no dual
cache table, backfill, or rolling worker compatibility path.

## Instrumentation and reproduction

One `file_work_metrics` JSON log is emitted per invocation, including work ID,
stage, status, source bytes/chunk counts, region, total seconds and phase spans.
Spans are inclusive and must not be added as disjoint time. Counts cover
application statements in the request thread, excluding transaction-control
messages, pool background activity and background heartbeat threads. No source
text, SQL parameters or credentials are logged.

Load repository `.env` without printing it, then run the isolated cloud runner:

```sh
uv run --with boto3 --with 'psycopg[binary]' --with modal \
  tests/e2e/cloud_index.py --scenario worker-throughput --workers 1 \
  > /tmp/worker-throughput.log 2>&1
python3 scripts/worker-throughput-report.py /tmp/worker-throughput.log
```

See [cloud requirements](production-deployment.md) for provisioning permissions,
TLS certificates and any managed-database login suffix. Set the same explicit
worker region for comparative runs. `--workers 2` starts two independent
transform processes and two of each consumer process, each with one slot, and
permits two bulk GPU containers. The report still shows per-worker service
rates, not aggregate wall-clock throughput.

`--scenario cloud-index` checks the real AWS multipart/append/deletion path.
`bash scripts/test-e2e-index-recovery.sh` checks actual process kills, lost
provider responses, a Postgres restart, stale writes and deleted-root cleanup.
The local recovery suites use real Postgres and LocalStack S3/SQS,
CPU Nomic and real Turbopuffer. They do not prove cloud failure domains or GPU
crash recovery. Provider-based media extraction and every cache-retirement race
are outside this benchmark's coverage.

## Validation evidence

- Recovery run `e1afbf20f6484e6c9039e1aa2d7bce8f`: passed lost-response
  SIGKILL/replay, actual Postgres restart with existing worker processes, stale
  publication, deleted-root visibility and scheduled cleanup. Cleanup passed.
- Migration run `40b8276c2dc64902b8c805a77ee31852`: passed a populated pre-041
  deployment upgrade, unchanged cached vector bytes/offset, append reuse and
  exact read/all search modes. The fixture asserts migration 041 has not run
  before legacy publication. Cleanup passed.
- Two-worker cloud run `63fdd98556dc4f4f8ed84718f1e950af`: passed cold and warm
  968-chunk phases, exactly one attempt per job, five packs, no new legacy
  locator rows, exact reads and all search modes. Two real bulk GPU containers
  initialized; two transform processes and two of each consumer process ran.
  This verifies operation with independent workers, not linear scaling: Modal
  placed this run in `us-west1`, and startup/shared-cluster conditions differed
  from the controlled single-worker comparison. Temporary resources cleaned.
- Cache-retention run `09b3c0251cdb464f99346505c377759b`: passed reuse, actual
  scheduled expiry, empty retired directory, S3 deletion, unchanged searchable
  results/mutation artifacts and subsequent re-encoding in 156.77 seconds.
  Cleanup passed. Broader retention/security scenarios were not rerun here.

The first migration harness incorrectly started new consumers during the legacy
phase; consumers also apply migrations, so that run did not exercise backfill.
The corrected run above uses old binaries for all Go roles. An initial final
cloud-index run hit the shared provisioning database's connection limit while
another cloud test ran; its indexing completed, but an assertion could not open
a connection. That run is a failed validation, not a throughput result.

The sequential final cloud-index rerun `2354b7fd5e7946a587d11f999ea58456`
passed in 118.77 seconds: actual AWS multipart init/resume/upload/complete,
scheduled abort/deletion, CLI/SQS/GPU publication, retained source bytes,
read/search and append reuse of 130 vectors. Cleanup passed in 1.86 seconds;
all temporary cloud resources were removed. `go build ./...`, Python syntax
compilation, shell syntax checks and `git diff --check` passed. Production
endpoints, worker configuration and schema were not changed.

## Critical sync-to-index follow-up

The active optimization scope is capture/sync through transformation and index
publication. The broader API audit is paused. These follow-up edits remain local:

- `PUFFERFS_MODAL_WORKER_CLOUD` optionally constrains the worker cloud alongside
  `PUFFERFS_MODAL_WORKER_REGION`. Modal `us-west` alone can select different
  cloud regions; see [Modal placement](https://modal.com/docs/guide/region-selection).
  The benchmark comparison uses `aws` plus `us-west` after observing a placement
  mismatch. Defaults and production deployments remain unchanged.
- API delivery confirmation uses one `UPDATE` per accepted SQS batch (up to ten
  jobs), replacing one per job. Both Go and Python confirmation queries exclude
  rows already marked by another publisher; duplicate sends remain recoverable.
- Native transformation completion uses one data-modifying CTE statement instead
  of three client statements. The live-attempt check gates extraction completion
  and creation of the index delivery record in the same transaction. It still
  changes the same three metadata rows; this saves round trips, not row storage.
- The index worker resolves routing once per claimed attempt and passes the
  registered artifact and batch count from preparation to publication. It uses
  acknowledgment progress from its locked claim, removing the second routing
  read and work-row reload. Replay attempts also avoid the work-row reload.
  Lease renewal, acknowledgment compare-and-set, immutable S3 artifacts and the
  final locked catalog publication check remain in place. No process-local
  shared cache, leader, or new worker coordination is introduced.

Worker metrics count client statements, not individual writes inside a compound
statement. The throughput report now includes summed counters for each validated
phase. The fresh single-worker runs below used the same four-file, 968-chunk fixture.
Application statement counts exclude connection health probes, transaction
control and background lease renewal. Each worker completed in one attempt.

| Phase / role | Before statements | After statements | Before chunks/s | After chunks/s |
| --- | ---: | ---: | ---: | ---: |
| Cold transformation | 28 | 20 | 216.46 | 238.78 |
| Warm transformation | 28 | 20 | 240.91 | 242.58 |
| Cold indexing | 76 | 68 | 35.72 | 35.31 |
| Warm indexing | 66 | 58 | 126.28 | 129.65 |

These are summed worker service rates, excluding queue/container startup.
Index throughput is essentially unchanged at this sample size; fewer metadata
calls do not establish a material throughput improvement. Warm capture through
publication was 13.557 → 13.543 seconds. Cold elapsed time was 59.869 → 231.483
seconds, dominated in the second run by waiting for a GPU worker to start.
Constraining placement can reduce network latency but restrict available
capacity; it is optional and has not been applied to production.

Baseline `13da335788d048d885c31458ad3e6e9e` and matched follow-up
`b2000b3a15564b0a87a1f7cdda05732e` both reported GPU region `us-west-2`.
The initial follow-up `e9176c46c82c49f4b3ea2afeb4e4723b` also passed, but reported
`us-west1`; its timing is excluded from the comparison. GPU dependency layers
were rebuilt from the existing version ranges, and fixture order/kernel warmup
can differ; this is not a controlled attribution of small timing differences.
All three runs verified retained source bytes, exact reads, all search modes,
cache reuse, five vector packs and no new per-vector database rows. Cleanup
passed and removed temporary cloud resources.

Recovery run `6ddecb2ea90945eca0f6706f14ea1ec2` passed actual worker SIGKILL after
provider acceptance, identical payload replay after the normal five-minute
lease, unchanged S3 vectors/mutations, database restart with existing worker
processes, stale-version publication fencing, root deletion and late-write
cleanup. Cleanup passed. This used separate production Docker Compose roles,
real Postgres, LocalStack S3/SQS, CPU Nomic and real Turbopuffer; it does not prove
GPU crash recovery or every cloud failure domain. `go build ./...`, changed
Python syntax compilation and `git diff --check` passed.

At this measurement point, capture persisted source manifests with sequential
S3 PUTs; the packed-manifest change below removes those calls. Final index
publication still uses several statements under its root/catalog/work locks.
The subsequent capture batching below removes
per-file metadata calls and the immediate global delivery scan. Further source
artifact batching and reducing serialized worker processing remain scoped
targets. Auth/login, groups/grants, billing and unrelated API work are outside
the active scope.

## Batched capture registration

The API server now registers up to 128 files with four metadata statements after
its existing root lock and authorization checks: create missing catalog rows,
read heads/replay receipts, validate and bind source ranges, and write the new
versions/extents/extractions/work records. These changes remain local and need
no schema migration. Deployment roles and SQS ownership remain as shown in the
[existing role diagram](architecture-and-functionality.md#per-file-pipeline-deployment-roles).

| Registration work, 128 files | Previous client statements | Batched statements |
| --- | ---: | ---: |
| New files, three source extents each | 1,664 | 4 |
| Appends, three reused extents plus one new extent each | 2,048 | 4 |
| Identical accepted capture replay | 768 | 2 |

Previous totals follow the pre-change call graph. The new counts are asserted
through `pg_stat_statements` in the two-API E2E. These totals exclude transaction
control, root/actor/ACL checks, content proofs, SQS handoff and worker activity.
A compound SQL statement can contain several table operations: this is a client
round-trip reduction, not a claim that 1,664 row writes became four row writes.

Each new version now sets `extents_indexed_at` in its initial insert, eliminating
one additional version-row update per file. New versions, extent edges,
extraction/work IDs and captured heads become visible atomically. Result slots
preserve request order independently of database row order. Identical replays
retain their original revision, IDs and version metadata, including when newer
heads are already published; they do not revalidate expired historical bytes or
repeat the version/extraction/work writes. Root/catalog row locks still have
physical overhead even when application values do not change.

Source validation reads the requested objects once under row locks. It checks
prior ranges with indexed lookups, rather than transferring each prior version's
entire extent directory to Go. An uploader's unbound pack can serve multiple
files within this transaction. A previously bound pack only permits ranges from
that same file's previous version; reusing a capture ID cannot authorize a new
path. Any unavailable or retired source aborts the batch, including source-pack
binding and new catalog rows. Source bytes and manifests remain in S3.

Immediate SQS handoff now looks up only the work IDs returned by this capture.
The scheduled reconciler retains its global recovery scan. Duplicate publishers
remain safe through SQS deduplication and database attempt ownership. The root
registration lock is retained, so same-root writes remain serialized during
this shorter metadata phase; different roots and API servers need no shared
process-local coordinator.

`bash scripts/test-e2e-capture-batches.sh` uses two actual API processes, production
migrations, real Postgres, LocalStack S3/SQS, native transformation, CPU indexing
without vectors, and real Turbopuffer. It checks 128-file creation and append,
exact source bytes/read/search, metadata statement counts, unchanged replay row
versions, mixed old receipts/new files, empty files and tombstones, concurrent
identical captures and conflicting writers. Existing capture E2E workflows also
exercise packed sibling isolation, foreign-tenant uploads, another uploader's
unbound pack, and revocations during actual S3 response holds. Source-retention
checks wait for the real signed-upload deadline; they do not alter timestamps.

The cloud run `f06828df5cfc4e049ed016cba2bf7ec9` passed in 178.77 seconds with one
native transform slot and one real GPU index slot: actual AWS multipart source
operations, append reuse, durable S3 vectors/mutations and all search/read modes.
Cleanup passed in 4.48 seconds and removed its temporary resources. This verifies
cloud compatibility; it is not a before/after capture throughput measurement.

A separate local comparison ran the previous API capture implementation and the
new API against the same services, with fresh synthetic roots and identical
128-file/384-extent inputs. API response time was 0.3015 seconds before and
0.2992 seconds after, excluding observation queries. This single comparison does
not establish a latency improvement. Postgres was colocated in Docker and workers
were active; the query-count and row-update savings are the supported results.
Both comparison roots published all files, passed retained-source checks, sampled
reads and FTS, and were cleaned up along with the temporary baseline API and its
protected environment files.

Two-API run `68e3c418cea945f2aaf0341da502a036` passed every capture scenario above,
then observed actual source expiry and physical cleanup, replayed an accepted
historical capture without restoring old heads, re-uploaded the frozen bytes of
an unaccepted capture, and verified contents/tombstones/search after restarting
both API processes. Cleanup passed in 8.10 seconds; no test containers or cloud
resources remain. Go build, Python/shell syntax checks and `git diff --check`
passed. The suite does not cover provider-media extraction or claim a measured
maximum throughput. The next section covers the subsequent source manifest
packing change.

## Packed source manifests

The API server persists the manifests for a capture in one immutable S3 JSONL
object, then runs the existing batched registration transaction and SQS handoff.
Transformation workers read each file's record with one S3 range GET. Roles,
worker ownership, work leases and horizontal scaling are unchanged. No database
migration, pack directory rows or additional database queries are required.

A capture with 128 nondeleted files previously made 128 sequential manifest
PUTs; the new writer makes one. An all-tombstone capture makes none. This reduces
capture-side network requests; it does not reduce the per-file manifest GETs or
establish a transformation/indexing throughput gain.

The object key is `sources/ORG/ROOT/manifests/PACK_SHA256.jsonl`. Each catalog
reference adds `#OFFSET:LENGTH:RECORD_SHA256`. Records are canonical manifest JSON
followed by a newline; the range excludes the newline. Identical records are
stored once and sorted by digest, making identical batches stable across API
processes and file order. Packs are bounded to 16 MiB. Readers check owner prefix,
range bounds, exact response length, record checksum, manifest format, catalog
hash/size and source extent ownership before consuming source bytes. Source
reconstruction still verifies the complete file hash before extraction succeeds.

Replay compares the manifest's record digest within its owner prefix rather than
its position in a pack. Reordered, subset and mixed old/new requests therefore
retain existing version IDs and stored references. Accepted legacy `.json`
receipts compare against the same content identity. A retry can upload an unused
pack; manifests retain their existing root lifetime and root deletion removes
all manifest objects, including shared packs. Source extent retirement does not
partially delete shared manifests.

This is a local change, not a deployed format migration. New readers accept both
standalone and packed references. Old workers cannot read packed references,
and old API writers do not recognize packed replay identities. Before enabling
the new writer, stop capture traffic, replace/drain all old manifest readers
(transformation, collector/reconciler and operator tooling) and replace all API
writers. Resume capture only after that cutover. A writer/reader rollback after
packed captures requires packed-format support; mixed old/new writers are not
supported by this change.

Cloud run `6dc3055bed144205b6918a3400475d19` passed with one transformation
slot and one GPU index slot, two shards, actual AWS multipart lifecycle, packed
manifest range reads, appends, retained source verification, exact reads and
FTS/vector/hybrid search. The append reused 130 cached vectors. The first
130-chunk file took 1.19 seconds in transformation and 4.58 seconds inside the
GPU index worker; the complete scenario took 417.91 seconds, including startup
and all validation. These are not matched before/after throughput measurements.
Cleanup passed in 4.39 seconds and removed the temporary cloud resources.

A matched local comparison used the pre-packing API source snapshot and the
current API against the same Postgres, S3 HTTP relay, queues and active workers.
Each API accepted a fresh 128-file/384-extent capture with identical payloads.
The old API created 128 manifest objects in 0.3869 seconds of capture-request
time; the new API created one in 0.1060 seconds (3.65× faster for this pair).
Both datasets subsequently indexed all files and passed retained-source hashes,
exact reads and FTS. This is one local pair, not a sustained throughput or
production latency benchmark. Logs and the synthetic driver are under
`tests/e2e/artifacts/manifest-comparison*`. Both isolated roots and their S3
objects were deleted; the temporary baseline API and its environment file were
removed.

Two-API run `d379d3d9b5ee4a46b5a092037d3cd55e` passed 128-file creation and
append with four registration statements, two-statement accepted retries without
version rewrites, mixed historical/new receipts, empty files, tombstones and
concurrent capture races. Capture permissions were rechecked after S3 IO;
revoked access could not commit a capture. The real 15-minute signed-upload
expiry elapsed before the scheduled reconciler removed obsolete/unaccepted
packs and abandoned multipart uploads. Historical receipt replay and re-upload
of the original retained pending bytes passed with packed manifests.

The preceding format-transition phase in the same Compose project verified an
actual older API's standalone manifests with current workers, replay through the
new API without changing existing versions, a single PUT for 128 new manifests,
reordered/subset retries, zero manifest PUTs for tombstones, and the operator
source verifier across 130 legacy/packed versions. Flipped and truncated range
responses were rejected before publication; normal SQS retries published exact
source bytes after each network fault cleared. Root deletion removed all shared
and unused manifest objects. The initial attempt failed because the test relay
dropped the HEAD response's content length; the corrected relay passed this
phase. No production bypass or database-state injection was used.

Exact reads, tombstones and search passed after both API processes restarted.
Final cleanup passed in 7.94 seconds and removed the test containers, volumes,
baseline image and network. `go build ./...`, changed Python/shell syntax checks
and `git diff --check` passed. Logs are in
`tests/e2e/artifacts/manifest-packs.log`. These runs exercise native extraction;
provider-media refresh and cloud corruption faults were not run in this change.

## Fewer index publication round trips

The index worker's successful final publication now uses three client database
statements instead of five: lock the root, lock/read the file and current work
attempt together, then update the catalog head and complete the work in one
statement. Superseded publication uses three statements instead of four. The
transaction count and physical metadata updates remain unchanged; this removes
network round trips, not the durable publication ledger.

The root lock stays in its own statement. Under the worker's existing Postgres
READ COMMITTED transactions, the following statement sees a fresh committed
head after waiting for a concurrent capture or publication. A materialized file
selection locks the catalog before the dependent work selection locks the live
attempt. It returns only five publication fields, replacing two full-row
results with one small result. Attempt token, status, live lease, mutation
reference, acknowledgment count and extraction ordering checks remain explicit.
Deleting a file/root also cascades through versions/extractions/work, so an
absent file cannot leave a publishable orphan work row.

The completion statement updates the work only through the catalog update's
returned row. Both updates commit together, under the same root/file/work locks
as before. Source/vector/mutation artifacts remain in S3, and no process-local
coordination, cache, schema migration or additional persistence is introduced.
The existing [deployment roles and queue ownership](architecture-and-functionality.md#per-file-pipeline-deployment-roles)
remain unchanged. This change is local, not deployed.

Cloud run `401a5ee15c58404cbb93fb2cc3067810` passed with one transformation
slot and one L4 GPU index slot in AWS `us-west-2`, actual S3/SQS, multipart
lifecycle, appends, retained-source checks, exact reads and all search modes.
Against the prior equivalent cloud scenario `6dc3055bed144205b6918a3400475d19`,
worker logs show:

| Index case | Application statements before | After | Write statements before → after |
| --- | ---: | ---: | ---: |
| 130 chunks, cold cache | 15 | 13 | 9 → 8 |
| 131 chunks, append with 130 cache hits | 16 | 14 | 10 → 9 |
| Each one-chunk cold-cache file | 15 | 13 | 9 → 8 |

Application counts exclude pool health checks and transaction-control messages.
The cold 130-chunk worker took 7.11 seconds versus 4.58 previously, with encoding
5.21 versus 3.10 seconds; the append took 1.23 versus 1.20 seconds. These runs
establish the query reduction, not a throughput improvement. The full scenario
passed in 178.59 seconds and cleanup passed in 5.16 seconds, removing temporary
cloud resources. Startup and provider timings are not controlled comparisons.

Recovery run `82b7576b76914abb94c4f93d3caae308` verified actual SIGKILL after
provider acceptance, a new attempt after the normal five-minute lease, identical
mutation replay and unchanged vector/mutation objects. Existing worker pools
recovered after a real Postgres restart. With two API processes, a newer capture
and a newer tombstone each superseded an older live index attempt whose real
provider response was held in transit. Releasing that response produced one
acknowledged, superseded attempt, preserved its mutation object, and exposed
only the newer contents/deletion through both APIs. The first test run stopped
because a Compose dependency startup downscaled the API to one process; starting
already-provisioned consumers with `--no-deps` fixed the harness. The rerun
confirmed both API processes remained present.

The same corrected recovery run passed stale-write visibility after a worker
crash, root deletion during an in-flight write, and scheduled removal of late
provider rows using durable cleanup artifacts. Final cleanup passed in 2.44
seconds and removed the Compose resources. Python/shell syntax checks and
`git diff --check` passed. Recovery used real Postgres, LocalStack S3/SQS and
real Nomic/Turbopuffer through separate production role processes; it does not
prove GPU crash recovery or every provider-media extraction revision. Logs are
`tests/e2e/artifacts/index-recovery.log` and
`tests/e2e/artifacts/pufferfs-cloud-c03488eb2310.log`.

At this measurement point, remaining costs included per-mutation provider
writes and durable acknowledgments, work claim/lease writes, and publication
serialized by the root lock. The next change removes per-batch database
acknowledgments; neither change establishes maximal throughput or finishes the
broader simplification objective.

## Acknowledge the artifact at publication

Index workers now persist acknowledgment of the complete mutation artifact in
the final publication transaction. They no longer write a checkpoint after each
provider batch or renew the lease before each batch/deletion page. The existing
one-minute background renewal owns lease maintenance; a renewal failure signals
the foreground loop to stop before its next provider request. Final publication
still locks root, catalog and work and checks the current attempt, live lease,
artifact reference/count, starting checkpoint and captured/extraction ordering.

For each newly applied mutation batch, this removes two foreground database
statements, two transactions and two updates of the work row. A bounded
multi-page tombstone also avoids one foreground renewal per extra deletion page.
Periodic background renewals still write to Postgres. The acknowledgment count
is written alongside the existing final work update, whether the attempt
publishes or finishes superseded; it adds no separate row or statement.

The immutable S3 mutation artifact remains the retry record. If an attempt
crashes after the provider accepted a prefix, its successor may replay that
prefix. This trades more provider requests on failure for fewer database writes
on successful processing. Upserts retain extraction-specific IDs, tombstone
filters retain version bounds, and search/read remain gated by the catalog
publication. A final acknowledgment is never recorded merely because an
artifact exists: every remaining provider call must succeed and the artifact
must be fully consumed before the live-owner publication transaction runs.

Existing partial checkpoints written by older workers remain supported. A new
worker starts from that checkpoint and records the total only at publication.
The source/vector/mutation formats and schema are unchanged. New workers are
compatible with those checkpoint records; this does not remove the separate
packed-source-manifest reader/writer cutover requirement above.

Single-worker cloud run `327d8acfeeee441e8676937b60c2fac9` passed both cache
phases for four files / 968 chunks / six mutation batches, with one local
transformation slot and one AWS `us-west-2` L4 GPU index slot. Exact source bytes,
all lines, FTS/vector/hybrid search, five S3 cache directories and cache reuse
were verified; no legacy per-vector rows were created.

| Phase | Transform chunks/s | Index chunks/s | Index application statements | Capture → publication |
| --- | ---: | ---: | ---: | ---: |
| Cold cache | 251.64 | 33.16 | 48 | 79.603 s |
| Warm cache | 242.29 | 126.04 | 38 | 11.771 s |

Rates use summed worker invocation time, excluding queue/container startup.
Transformation remained at 20 application statements per phase. The six index
batches needed no foreground lease or acknowledgment updates. Against the older
`b2000b3a15564b0a87a1f7cdda05732e` run's 68/58 index statements, the last two
publication changes remove 20 calls per phase: eight from final publication and
twelve from per-batch renewal/acknowledgment. Its 35.31/129.65 index chunks/s do
not establish a speedup against the new run; provider, encoding and startup
variation remain. Foreground index transactions are now 31 cold / 26 warm and
write statements 27 / 17; background lease renewals and pool checks are separate.
The full scenario passed in 146.18 seconds, cleanup in 3.81 seconds, and temporary
cloud resources were removed.

Database-outage run `14143b4456944ffc82f5b8287f2d6270` passed with the live CPU
index process unchanged across a 100-second Postgres outage. After background
renewal failed, the worker sent no further batch, retried in the same process,
replayed all three immutable batches, and published all 1,025 lines correctly
through both API processes. Its mutation object was unchanged. Cleanup passed
in 0.86 seconds. Logs: `tests/e2e/artifacts/index-renewal.log`.

Two-API recovery run `e386bd05c1ec4a1d85066d860869a9ad` passed worker SIGKILL,
normal lease retry with identical mutation bytes, database restart with live
pools, newer capture/tombstone supersession of a live attempt, stale-write
visibility, root deletion and cleanup of late writes. Cleanup passed in 2.34
seconds. These results use the new acknowledgment/renewal behavior, with real
Nomic/Turbopuffer and separate production processes on Postgres/LocalStack;
they do not demonstrate cloud GPU crash recovery. The cloud benchmark above
separately validates GPU execution.

Checkpoint-transition run `4535e055f9a448aabfec1563fcbe0eae` passed both
three-batch cases. The older worker's real checkpoint of one resumed with two
provider batches; the new worker's checkpoint of zero resumed with all three.
Each required a new attempt after actual SIGKILL and the normal lease, unchanged
S3 mutation metadata and repeated provider payload hashes. All 1,025 lines and
FTS were verified on both APIs, then verified again after both API processes
restarted. Cleanup passed in 1.28 seconds. The first run's replay succeeded but
its validator requested 1,025 lines in one API call and hit the existing
1,000-item limit; the corrected validator uses two legal ranges and passed the
full rerun. The renewal verifier likewise used the rebuilt corrected runner.

Python/shell syntax checks and `git diff --check` passed. The source changes
remove ten lines across `index_publish.py` and `index_worker.py`. No migration
or production deployment was performed. Test containers, volumes and networks
were removed. Detailed logs: `tests/e2e/artifacts/index-checkpoints.log`,
`tests/e2e/artifacts/index-recovery.log`,
`tests/e2e/artifacts/pufferfs-cloud-8b70b69cc4e1.log` and the renewal log above.

## Keep packed vectors through mutation preparation

The preceding warm run spent 3.27 seconds preparing mutations and 3.77 seconds
writing them to the provider. Index preparation now retains each vector as
little-endian float32 bytes from its S3 cache read or initial model-output pack.
Mutation construction base64-encodes those bytes, a format supported by
[Turbopuffer's write API](https://turbopuffer.com/docs/write#vectors), instead of
expanding them into Python floats and decimal JSON arrays. Each 768-dimensional
vector occupies 4,098 JSON bytes including string quotes. Precision, model,
normalization, cache revision and S3 vector pack format are unchanged.

New immutable mutation artifacts contain these strings. Publication still
passes persisted vector values through to the provider, including numeric
arrays in existing artifacts. Mutation row/byte bounds, lease ownership and
publication checks are unchanged. There is no new schema, database query,
shared cache or coordination between workers; the representation change applies
independently to each worker. Smaller rows can also fit more chunks in a bounded
provider batch.

The cloud benchmark and cloud-index assertions decode each persisted mutation
vector and compare it byte for byte with the actual S3 cache pack. The benchmark
also records compact, uncompressed mutation JSON bytes, vector JSON bytes and
mutation record count. These are logical payload sizes, not compressed network
traffic measurements.

Cloud run `43b2e37a1f78444db9594c510bc247eb` passed with the same four-file,
968-chunk corpus, one transformation slot and one L4 index slot in AWS
`us-west-2`. Against the immediately preceding numeric-vector run:

| Measurement | Numeric vectors | Packed/base64 vectors |
| --- | ---: | ---: |
| Cold index chunks / worker second | 33.16 | 38.20 |
| Warm index chunks / worker second | 126.04 | 158.13 |
| Warm mutation preparation | 3.268 s | 1.332 s |
| Warm provider writes | 3.773 s | 4.172 s |
| Provider batches per phase | 6 | 5 |
| Cold capture → publication | 79.603 s | 60.417 s |
| Warm capture → publication | 11.771 s | 11.833 s |

Warm worker throughput was 25.5% higher and mutation preparation took 59.2%
less time in this pair. End-to-end warm latency did not improve, and provider
timings varied despite fewer requests. Cold encoding also varied (20.444 to
19.332 seconds); this pair does not isolate every source of the cold improvement
or establish sustained maximum throughput. The transformation implementation
was unchanged: observed rates were 225.18 cold / 241.44 warm chunks per worker
second, versus 251.64 / 242.29 previously.

Each new phase contained 8,815,488 bytes of compact mutation-record JSON,
including 3,966,864 bytes of vector JSON. The largest file used two batches
instead of three. Database counts stayed at 48 cold / 38 warm application
statements for indexing, and 20 for transformation. Five S3 cache packs served
all 968 warm hits; both phases verified exact vector bytes, source retention,
all source lines and FTS/vector/hybrid search. The scenario passed in 119.44
seconds and cleanup in 4.06 seconds; temporary cloud resources were removed.
Log: `tests/e2e/artifacts/pufferfs-cloud-cc8ba3ad382f.log`.

Recovery run `70c7f26e6e9148ac82a61d90c826a0cb` passed actual worker SIGKILL
and normal-lease replay with the new vector representation, preserving both
the mutation payload hash and S3 object metadata. Database restart, live
capture/tombstone supersession, late stale writes, root deletion and scheduled
late-write cleanup also passed. Cleanup completed in 2.18 seconds and all local
test resources were removed. This suite uses real Nomic on CPU and real
Turbopuffer with Postgres/LocalStack; cloud GPU execution is covered separately
by the benchmark above. Log: `tests/e2e/artifacts/index-recovery.log`.

Cloud correctness run `5096aafdbe8b47389c753e55bec306a7` also passed in AWS
`us-west-2`: 130 exact cached/mutation vectors, append reuse of all 130 with
one new encoding, retained sources/read, provider-distance ranking and global
top-k across populated/empty roots and both shards, multipart operations and
scheduled root cleanup. The initial index worker took 4.399 seconds and the
131-chunk append 0.925 seconds. The scenario's 538.2 seconds included roughly
six minutes before the first GPU attempt; read-only checks showed the message
in flight, pending index work with zero attempts and no running container in
the temporary index app. That startup delay is not a serialization measurement.
Cleanup passed in 4.70 seconds; temporary cloud resources were removed.
Log: `tests/e2e/artifacts/pufferfs-cloud-0398c3689065.log`.

Python syntax checks and `git diff --check` passed. The retention scenario's
vector validator was updated for both provider encodings, but its full expiry
suite was not rerun for this representation change. Existing numeric-vector
artifact replay remains a pass-through code path; this turn's crash test used
new base64 artifacts. No production deployment was performed.

## Send the committed transformation handoff directly

The transformation completion transaction already records the extraction and
its index work atomically. It now returns that work's immutable delivery
identifiers from the claimed job after the commit succeeds. The transformation
worker sends those identifiers through the shared SQS batch sender, removing
the immediate five-table query that reconstructed them from the database.
This removes one read statement and transaction per completed native
transformation. The SQS success acknowledgment still updates the existing work
row; this change removes no recovery state and adds none.

The collector and reconciler keep the bounded pending-delivery scan. If the
worker dies or its queue request fails after completion, the committed index
work remains discoverable there. Both immediate and repair delivery use the
same eight message fields, FIFO grouping, deduplication ID and acknowledgment
code. A delayed immediate send can duplicate a repaired delivery; normal work
claiming handles duplicates and stale/deleted files. Delivery does not depend
on which API or transformation process handled the file.

End-to-end run `bc192a513c374e84b65c4e5cb46b2afe` passed with two API
processes, two transformation processes and two transform consumers. All 24
initial CLI captures transformed exactly once without a delivery-ledger read.
Twelve immediate SQS deliveries contained the expected eight fields; another
twelve committed their chunks and index work during a real network outage.
With transformation processes stopped, scheduled reconciliation delivered those
original work IDs without changing the chunks. Both APIs then returned exact
search/read results after process restarts, updates and tombstones, again
without normal transformations scanning the ledger. Logs recorded 11 and 15
completed transformations on the two processes (including the two updates),
and five/seven failed queue sends respectively.

Capture/outage verification passed in 51.15 seconds, repaired-state checks in
0.24 seconds after reconciler startup, and publication/update checks in 27.41
seconds. Cleanup passed in 3.35 seconds and removed local resources. This
scenario uses real Postgres and LocalStack S3/SQS, production roles and real
Turbopuffer; all files are native text with vectors disabled. It does not claim
Gemini or GPU coverage. Log: `tests/e2e/artifacts/transform-handoff.log`.

Single-worker cloud run `151fa47baba14c6893a1405c2ebcc4d9` passed the same
four-file / 968-chunk cold and warm benchmark with one local transformation
slot and one L4 GPU index slot in AWS `us-west-2`. Compared with the preceding
base64-vector run `43b2e37a1f78444db9594c510bc247eb`:

| Measurement | Before direct handoff | Direct handoff |
| --- | ---: | ---: |
| Transform application statements per phase | 20 | 16 |
| Transform transactions per phase | 16 | 12 |
| Transform write statements per phase | 12 | 12 |
| Cold transform chunks / worker second | 225.18 | 252.59 |
| Warm transform chunks / worker second | 241.44 | 269.80 |
| Warm transform queue-publish phase | 1.165 s | 0.878 s |
| Warm capture → publication | 11.833 s | 9.898 s |

Statement and transaction reductions match the removed read in each file's
handoff. Rates are one observed pair, not an isolated estimate of the change's
causal speedup. Index code was unchanged: its application counts stayed at
48 cold / 38 warm, while warm provider time varied from 4.172 to 2.826 seconds.
That accounts for much of the end-to-end difference. The observed index rates
were 40.08 cold / 215.81 warm chunks per worker second. Cold capture-to-publication
was 224.230 seconds versus 60.417 previously, including a long wait before the
GPU worker began; cold worker processing itself totaled 24.149 seconds.

Both phases verified exact sources, all source lines, vector bytes against the
five S3 cache packs, cache reuse, five mutation batches, and FTS/vector/hybrid
search. The scenario passed in 285.20 seconds and cleanup in 4.28 seconds;
temporary cloud resources were removed. Python/shell syntax checks and
`git diff --check` passed. No migration or production deployment was performed.
Log: `tests/e2e/artifacts/pufferfs-cloud-8056f6a4126c.log`.

## Return capture deliveries and batch their acknowledgment

Capture registration now returns committed work references alongside its
public version receipts. Its existing catalog/replay read also retrieves the
original work's pending/unacknowledged state for historical retries. New work
is already known from the insertion; acknowledged retries produce no delivery.
The API no longer reconstructs those identifiers with a separate five-table
query after registration and captured-proof persistence.

The sender groups the existing reference-only messages by stage and sends at
most ten per SQS request. It collects the IDs from fully confirmed batches and
marks them in one database update after sending. If a later batch fails, the
earlier confirmed batches are still marked; unconfirmed work remains available
to API retry or scheduled reconciliation. A crash before the final mark can
replay more accepted messages, using the existing stable deduplication IDs and
work claims. No source/index publication happens merely because delivery was
acknowledged, and no lock is held across the queue calls.

For a successful new 128-file, single-stage capture, the previous sender made
one delivery SELECT plus thirteen acknowledgment UPDATEs. The new sender makes
one acknowledgment UPDATE. Registration, authorization and proof queries are
additional and unchanged in count. A fully acknowledged capture replay makes
no delivery SELECT, UPDATE or SQS call. The implementation also removes the
separate delivery record type, database scan method and sender-only interfaces;
the queue message type now carries the committed references directly.

Two-API end-to-end run `a6a43b74a6744a1e8d7ba1e4ca9ae27d` passed these
assertions with execution consumers initially stopped, so worker updates could
not contaminate the handoff SQL counts. The 128-file request made thirteen real
SQS batch calls and one acknowledgment statement, with zero delivery SELECTs;
the observed request took 0.092 seconds. Both subsequent acknowledged retries
made no SQS calls or acknowledgment statements. These are current-run timings,
not a before/after latency experiment.

The HTTP relay then disconnected every send after the first accepted batch of
a 25-file capture. Only ten IDs were marked; an identical retry through the
other API sent exactly the remaining fifteen IDs in two batches and one
acknowledgment, preserving the first ten timestamps. Mixed transform/index
stages also shared one acknowledgment. Historical replay after newer captures
and eight concurrent identical captures remained valid. A separate twelve-file
total outage left no acknowledgments; the reconciler repaired those committed
IDs after both APIs restarted, with the API's queue path still disconnected.

Read-only queue inspection verified all 167 transform references and the one
tombstone index reference, each with the existing eight fields and FIFO group.
After normal consumers started, both APIs returned exact retained sources,
reads, FTS results and tombstones; the concurrent case executed once per stage.
Capture/fault checks passed in 8.60 seconds, repaired-state assertions in 0.37
seconds after reconciler startup, publication checks in 122.09 seconds and
cleanup in 3.33 seconds. Local resources were removed. The suite uses real
Postgres, LocalStack S3/SQS and real Turbopuffer; it does not claim faulted AWS
SQS coverage. Log: `tests/e2e/artifacts/capture-handoff.log`.

Real AWS/Modal run `c3070328abab4827906389a364ccd6f9` passed the capture
change through GPU publication, exact cache/mutation vector bytes, 130-vector
append reuse, source reads, cross-shard search ranking and multipart/root
cleanup. The scenario completed in 356.61 seconds including GPU startup;
cleanup took 4.50 seconds and removed its temporary cloud resources. This run
predates the cache-upload lock change below. Log:
`tests/e2e/artifacts/pufferfs-cloud-1b2f7e707de2.log`.

Existing capture-batch suite `c30ce052953c494b99ca6ab5ab49308f` passed with
the new registration/handoff code. It covered 128-file creation and append,
immutable/historical/mixed receipts, empty files and tombstones, same-capture
and competing-capture races through two APIs, cross-tenant upload/reference
rejection, sibling extent isolation and revocation during manifest upload.
User/org/group grant deletion, permission/role downgrades, group/org membership
removal and API-key revocation all rejected in-flight capture; authorized
resumption then indexed/read successfully.

After the real fifteen-minute upload deadlines elapsed, scheduled cleanup
removed obsolete/unaccepted source packs and abandoned multipart uploads.
Historical receipt replay, retained-byte re-upload and same-file append reuse
still passed. Both API processes restarted and returned the same captured
contents, tombstones and search/read results. Cleanup passed in 8.39 seconds
and removed local resources. No timestamps were edited. This suite used the
API change with native, no-vector workers; cache behavior is covered by the
separate tests below. Log: `tests/e2e/artifacts/capture-batches.log`.

## Lock a new cache pack without a redundant UPDATE

Cache upload previously updated `last_used_at` to acquire the live pack's lock,
performed the S3 PUT, then updated the same row again with its content hashes.
It now uses `SELECT ... FOR UPDATE` for the first step and writes the timestamp
with the content hashes after the PUT. Both statements still use the same
transaction-start `NOW()`, so the retention timestamp is unchanged.

This removes one UPDATE per new pack without removing the lock or the earlier
committed cleanup allocation. Retirement still cannot pass an active upload,
and an allocation retired before the lock is acquired still prevents the PUT.
The number of statements, transactions and S3 requests is unchanged. Locking
itself still has database cost; this removes a redundant row update, not all
physical database writes. Cache-hit timestamp refreshes are unchanged.

Cache-retention run `f80ee508c1e149b286e2113ee227fc50` passed with the new
lock statement and the configured 120-second cache lifetime. Two publications
reused one pack; scheduled cleanup cleared its directory and deleted its S3
bytes while search and immutable mutation artifacts stayed valid. A third
publication encoded the same text into a new pack without altering the earlier
work or mutation objects. The two misses each used five write statements and
the hit used four, matching removal of the extra UPDATE only on misses.
This local suite uses real Nomic on CPU, real Turbopuffer and Postgres/LocalStack.
It passed in 155.25 seconds; cleanup took 1.09 seconds and removed local
resources. Log: `tests/e2e/artifacts/retention.log`.

Cloud benchmark `af94ce4535484339966954ee36f2a249` passed the combined capture
and cache changes with one transformation slot and one AWS `us-west-2` L4
index slot, four files and 968 chunks per phase. Cold index write statements
fell from 27 to 22 across five packs; total application statements stayed at
48 and transactions at 31. Warm indexing stayed at 38 statements, 17 write
statements and 26 transactions. Transformation remained at 16 application
statements, twelve write statements and twelve transactions per phase.

Observed rates were 262.42/278.15 transform chunks per worker second and
39.18/205.28 index chunks per worker second for cold/warm cache respectively.
Capture-to-publication took 60.869 seconds cold and 9.705 seconds warm. The
preceding run's warm value was 9.898 seconds; provider and startup variation
remain, and this pair does not prove a throughput gain from the cache lock
change. Both phases verified retained source bytes, all source lines, exact
vector bytes, five cache packs, five mutation batches and all search modes.
The scenario passed in 120.20 seconds and cleanup in 3.97 seconds; temporary
cloud resources were removed. Log:
`tests/e2e/artifacts/pufferfs-cloud-c06af21975ce.log`.

The production Go build, Python/shell syntax checks and `git diff --check`
passed. No migration or production deployment was performed for these capture
handoff and cache-lock changes.

## Return index routing with the work claim

Index work now obtains its active namespace directory from the existing claim
read, scoped to the claimed root and organization. A conditional JSON aggregate
returns the namespace, shard index and shard count only for index work;
transformation work does not request routing. The worker uses the existing
shard validator and path hash against that returned directory, removing the
separate post-claim SELECT and transaction.

The directory belongs to that attempt's database snapshot. It is not persisted
again or cached between requests, and workers do not coordinate through memory.
Root deletion, captured-version changes and lease ownership are still checked
by final publication; late writes remain subject to the existing cleanup and
visibility rules. A new attempt reads its routing again through its own claim.

Two-shard cloud benchmark `0c8da04174b744ff955fbbb6a934356f` passed with one
transformation slot and one AWS `us-west-2` L4 index slot. For four files and
968 chunks, index application statements fell from 48/38 to 44/34 cold/warm,
and transactions from 31/26 to 27/22. Write statement counts remained 22/17.
Transformation stayed at sixteen application statements and twelve transactions
per phase. Exact sources, vector bytes, five cache packs, five mutation batches
and all search modes passed.

Observed index rates were 39.06 cold / 197.15 warm chunks per worker second;
warm capture-to-publication was 9.727 seconds versus 9.705 in the preceding
run. The query reduction is verified; these timings do not establish a speedup.
The scenario passed in 119.10 seconds and cleanup in 4.07 seconds, removing
temporary cloud resources. This measurement predates the consumer change
below. Log: `tests/e2e/artifacts/pufferfs-cloud-d80166fadcfd.log`.

Two-API recovery run `4aad3599a47346ecb51bf3bd81c9f9b5` passed the routing
change with real worker SIGKILL, normal-lease replay of identical mutation
payloads, unchanged vector/mutation objects, database restart, live capture and
tombstone supersession, late stale writes, root deletion and scheduled cleanup.
Cleanup passed in 2.40 seconds and removed local resources. This run used the
new Python routing claim with the pre-consumer-change Go image; it uses real
Nomic on CPU and real Turbopuffer with Postgres/LocalStack. Log:
`tests/e2e/artifacts/index-recovery.log`.

## Consume the worker's validated durable response

The consumer's HTTP client already requires HTTP 200, a decodable bounded JSON
body, and a durable work status: complete, superseded, or waiting for the
provider for transformation work only. Busy responses keep the existing
lease-wait loop; transport, HTTP, decoding and other-status errors leave the
SQS receipt unacknowledged. The consumer now uses that validated result directly
instead of querying `file_work.status` again after a successful response.

This removes one SELECT per successful worker invocation in both stages.
The pre-invocation read remains: it validates the queued identities, chooses
the CPU/GPU endpoint, avoids invoking finished work and checks an existing
lease. The workers still commit their durable transition before returning
success. No new state, cache, schema or response format is introduced.

Two-API run `88ec9c89a1414f7a9e0176257f970a08` passed with both changes.
Its PostgreSQL statement counters recorded zero post-response status queries
after 165 completed transformations, two superseded transformations and 166
completed index invocations. The scenario also checked 128-file delivery
batching, no-op/historical retries, partial queue failure and retry through
the other API, concurrent captures, reconciliation after API restarts, exact
source bytes, both APIs' reads/FTS and tombstones. Capture assertions passed in
8.22 seconds, repaired-state assertions in 0.38 seconds, publication checks in
125.90 seconds and cleanup in 3.35 seconds. Local resources were removed.
Log: `tests/e2e/artifacts/capture-handoff.log`.

Single-shard AWS/Modal run `48d696492a6349fd96c750318f87ce9a` passed both
changes through GPU indexing, byte-exact cache/mutation assertions, reuse of
130 vectors on append, source reads, real provider ranking/global top-k and
multipart/root cleanup. It passed in 177.08 seconds and cleanup in 4.05 seconds;
temporary cloud resources were removed. Together with the two-shard benchmark
and local suites this exercises both routing configurations. These runs did not
exercise Gemini transformation; the provider recovery coverage below was added
subsequently. The durable-status classification is unchanged. Log:
`tests/e2e/artifacts/pufferfs-cloud-d7e35163ac87.log`.

Go build, Python syntax checks and `git diff --check` passed. Neither change
was deployed to production.

## Batch provider result bookkeeping

The collector already writes one packed text artifact to S3 for each provider
batch. It now updates all successful request pointers in one SQL statement and
all failed request errors in at most one more statement, followed by the
existing batch update in the same transaction. An all-successful 64-request
batch uses two UPDATE statements instead of 65; a mixed batch uses three.
The number of request rows updated, S3 objects and completion transactions is
unchanged. Request identities, per-request retry state and the batch/status
guards remain in Postgres; extracted text remains in S3.

The collector also waits for a terminal provider response before reading the
request mappings. Each pending poll avoids one joined SELECT and decoding up
to 64 mappings. A terminal poll uses an additional short read transaction;
the database connection is released before the provider call. This introduces
no cache or process-local coordination and retains the existing replay guards.

Real-provider recovery run `c23aac52c6dd49ea984af80d7f5f99c0` passed. After
SIGKILL following Gemini acceptance, the normal lease/listing paths recovered
the same paid job without a second submission or media upload. The mixed-page
case deleted two of four real provider uploads before submission: Gemini
returned two successes and two failures, both batched updates committed, and
only the failed pages were regenerated. Successful S3 result objects remained
unchanged; ordered page content, original source bytes and public search passed.
The lost-response recovery phase took 287.00 seconds (including normal lease
expiry), partial-result recovery 212.42 seconds and cleanup 1.66 seconds.
Compose resources were removed. Log: `/tmp/pufferfs-provider-batched-recovery.log`.

While that suite held collection stopped, an additional read-only assertion
confirmed a durable `waiting_provider` work row with an empty transform SQS
queue. This exercises acknowledgement using the worker's committed response
without the removed consumer status query.

Full Compose run `8eb010f7d5b24ddf8e6b798ab6f16682` also passed: 1,024 corpus
files plus the native/follow/recovery fixtures, 11 provider requests in ten
packed S3 results, exact captured bytes and extracted content, CLI search/read,
vector/hybrid search, authorization, no-op sync, multipart recovery, unavailable
workers, API/consumer restart and mixed malformed/valid queue deliveries.
The main verification phase took 828.37 seconds, resumed publication 58.77
seconds and cleanup 8.80 seconds. All isolated Compose resources were removed.
Log: `/tmp/pufferfs-provider-batched-results.log`.

Both suites use real Gemini and Turbopuffer with separate production roles in
Compose, Postgres/LocalStack and Nomic on CPU rather than Modal GPU hosting.
Python syntax and `git diff --check` passed. No throughput improvement is
claimed from these statement counts, and this change has not been deployed.

## Batch provider request reservation

Provider preparation previously reserved each page/clip with an INSERT followed
by a full-row SELECT inside a single transaction. Reservation now validates
the bounded input in memory, locks the work attempt, inserts the batch, inserts
all request mappings with one `jsonb_to_recordset` statement and reads their
identities together. The final read selects only request key, batch, location
and MIME type. At 64 requests this changes reservation from 130 statements
(65 writes) to four (two writes), with the same rows and one transaction.
Per-upload registration, lease renewal and paid-submission operations are
additional and are not included in these reservation counts.

The work ownership/lease lock, deterministic request identities and
`ON CONFLICT DO NOTHING` remain. Validation still compares the persisted batch,
location and MIME type, now also explicitly requiring the complete set of keys.
It uses a separate statement snapshot after the INSERT so a conflicting
reservation's committed row cannot disappear behind the INSERT snapshot.
No shared-process cache, new schema, API contract or durable state is added.

Expanded two-API run `4189692c2a704f6c84c5171c3596cdb2` passed. PostgreSQL
counters observed exactly four reservation statements for the first 64 pages:
one ownership read, one batch INSERT, one 64-row request INSERT and one 64-row
identity read. After SIGKILL following provider acceptance, recovery retained
the original job and input IDs, reserved the final page in one additional batch
and published all 65 pages. Each API independently served FTS and ordered page
reads. The four-page partial-failure scenario also passed, including durable
`waiting_provider` acknowledgement with collection stopped, batched reservation,
unchanged successful S3 results and retries confined to failed pages.

The first capture/accepted-response phase took 102.59 seconds; lost-response
recovery 1,237.31 seconds, partial-batch preparation/completion 440.37 seconds,
partial recovery 162.53 seconds and cleanup 2.06 seconds. These include real
provider processing, normal leases/schedules and provider cleanup; they are
not reservation throughput measurements. All isolated Compose resources were
removed. Log: `/tmp/pufferfs-provider-batched-reservation.log`.

The suite runs separate production roles with real Gemini/Turbopuffer,
Postgres/LocalStack and local CPU Nomic rather than Modal GPU hosting. Python
syntax, shell syntax and `git diff --check` passed. This change is not deployed.


## Provider S3 batch-manifest redesign

The per-request reservation/result SQL optimizations above describe earlier
worktree stages. The current provider redesign removes those per-input tables
entirely, retaining one batch row and storing input/retry/cleanup detail in
bounded S3 manifests. See [the current representation, call accounting and
verification record](provider-batch-manifests.md). This is a local change, not a
production rollout or a million-file throughput measurement.
