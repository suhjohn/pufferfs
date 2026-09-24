# Production-process E2E tests

The current harness runs the real CLI, Go API, ingestion worker and background
worker as separate processes. Postgres 17 and S3-compatible LocalStack are real
network services. Gemini, Turbopuffer and the optional vision endpoint are real
external providers. No in-process application calls, mock providers or database
state injection are used.

```text
CLI --> API --> Postgres (capture + durable file_work)
 |                 ^                   |
 +--> S3 <---- ingestion worker <-------+ claims
       ^           |                   |
       |           +--> provider       +--> background worker
       |                                    | collect / publish / clean
       +------------------------------------+
                                            +--> Turbopuffer [black box]
```

Compose starts workers. Workers claim their own Postgres jobs. HTTP/TCP fault
relays delay/disconnect actual provider or storage traffic; they do not fabricate
successful model responses. Test drivers inspect durable effects read-only.

## Running

```sh
set -a; source .env; set +a
bash scripts/test-e2e.sh
```

`GEMINI_API_KEY` and `TURBOPUFFER_API_KEY` are required. Vision suites also
require `MODAL_PROXY_TOKEN` and the configured vision URL. Missing credentials
fail; tests never silently select a fake or skip model calls. Compose disables
implicit `.env` loading and forwards only named provider settings. Production
AWS/database credentials are not passed to local services.
Only the vision overlay enables the fallback endpoint. Other suites exercise
Gemini's retry path even when the invoking shell has vision settings in `.env`.

Each script creates a unique Compose project and synthetic tenant. Scripts clean
external provider/index resources before deleting local state. A cleanup failure
retains its project for recovery. Sanitized logs/results go to `artifacts/`.
Do not edit a shell driver while that same file is executing.

## Coverage

| Script (`scripts/`) | Workflows |
| --- | --- |
| `test-e2e.sh` | Native/corpus formats; live follow; 1,024-file capture; multipart recovery; all search/read modes; authorization; updates/deletes; worker outage and API restart |
| `test-e2e-capture-handoff.sh` | Atomic scheduling; capture replay through two APIs; concurrent duplicate prevention; API restart; multiple worker replicas |
| `test-e2e-index-recovery.sh` | Lost write response; worker SIGKILL; lease expiry; database restart; bounded claims; stale writes; root cleanup |
| `test-e2e-index-checkpoints.sh` | Confirmed-batch checkpoints, ambiguous-write replay after SIGKILL, SIGTERM draining and exact reads/search after API restart |
| `test-e2e-concurrent-uploads.sh` | Shared pack/part concurrency bound, out-of-order durable acknowledgments, CLI SIGKILL, exact retained bytes and replacement publication |
| `test-e2e-search-admission.sh` | Shared global/tenant capacity across two APIs, competing tenants, client cancellation, provider timeout and API crash lease expiry |
| `test-e2e-cleanup-pages.sh` | Multiple object/multipart pages, failed-root isolation, prompt partial continuation and worker restart |
| `test-e2e-capture-summary.sh` | 1,025-file bounded status response, selected path/hash status, fresh ACLs and publication/read/search after API restart |
| `test-e2e-catalog-changes.sh` | Concurrent captures, durable paged CLI cache, SIGKILL/resume, zero-file unchanged deltas, ACL cursor reset and publication/tombstones |
| `test-e2e-follow-changes.sh` | Changed-path capture without unrelated tree access, nested directory moves, ignore changes, real OS event overflow, offline rewrite/restart and append extent reuse |
| `test-e2e-embedding-capacity.sh` | Shared token/request windows across two APIs/two background workers, measured token settlement, normal expiry/restart and repeated external 429 recovery |
| `test-e2e-segments.sh` | Large native source/parser checkpoints, SIGTERM/SIGKILL, unchanged-prefix download/index reuse, pre-EOF base64 tail, shared-segment proofs/cleanup, rewrite/truncation, deletion and restarts |
| `test-e2e-index-renewal.sh` | Live lease-renewal failure during a real database outage |
| `test-e2e-installer.sh` | Generated release manifest, installer, current CLI self-upgrade and checksum-verified real release archives in isolated HTTP/client containers |
| `test-e2e-upgrade.sh` | Old v0.8.2 processes create published and pending data; production migrations; new processes resume exact reads/search/deletion |
| `test-e2e-provider-recovery.sh` | Provider submission/result loss; retries; two collectors; process crashes |
| `test-e2e-retention.sh` | Source/artifact retention; journaling; authorization; safe cleanup |
| `test-e2e-media.sh` | Real audio/video extraction, transcript and timing contracts |
| `test-e2e-formats.sh` | Additional document/image/spreadsheet formats |
| `test-e2e-vision-fallback.sh` | Real fallback endpoint; partial success and cancellation (`PUFFERFS_E2E_VISION_CASE=partial` or `cancel`) |
| `test-e2e-api-access.sh` | Two-API permissions, search/read, keys, membership and browser sessions |
| `test-e2e-capture-batches.sh` | Capture batching, replay, conflicts and retained source extent authorization |
| `test-e2e-capture-spool.sh` | Local disk limits, source packing, verified append reuse, interrupted spool recovery, 32 MiB/two-part multipart resume, expired sessions and lost completion responses |
| `test-e2e-manifest-packs.sh` | Metadata manifest packing, transport corruption and recovery |
| `test-e2e-provider-discovery.sh` / `test-e2e-provider-deletion.sh` | Submission discovery and deletion during provider work |

Source-byte packing and metadata-manifest packing remain separate optimizations.
E2Es cover shared packs, verified append reuse, retained extent authorization and
pending journals. Removed SQS/HTTP-worker tests are replaced by database
scheduling, replica and crash tests.

GitHub PR/main CI runs builds/configuration checks. The paid E2E workflow runs
manually and gates releases. All scripts can also run locally. Neither location
changes the correctness contract; the release gate gives a recorded shared run.
The release gate includes the two-API access and installer/manifest suites as
well as ingestion, recovery, migration, media and vision workflows.

## Differences from production

- Local containers replace ECS scheduling/networking and use fixed disposable
  AWS credentials; no IAM/task-role or deployment rollout claim is made.
- The recorded local runs used Linux ARM64 containers on Docker Desktop.
  GitHub runners and the production image builds use Linux AMD64. Local
  results do not verify production resource limits, ALB/TLS or architecture.
- LocalStack replaces AWS S3; production providers remain real.
- Worker test images add fixture/relay tools. Both production and test workers
  run unprivileged and use the same runtime source and requirements. The driver
  runs as root only within its isolated fixture container.
- Retention tests configure shorter supported retention periods; leases and
  heartbeat timing are ordinary production values.
- Embedding capacity tests configure a 12-request/32,768-token minute to
  exercise backpressure cheaply. Their HTTP 429s originate at a network fault
  relay; every successful embedding/search still comes from Turbopuffer.
- The watcher scenario runs its CLI as UID 1000 and overfills the actual Linux
  inotify queue while that process is stopped. It verifies Linux overflow
  recovery; macOS uses a different native event implementation.
- Segment tests use an S3 observation relay to count real source ranges, and
  wait for ordinary worker lease expiry after SIGKILL. Native text can resume
  its parser and digest. Whole-input format decoders still require their source
  containers; their output artifacts and indexing use bounded segments.
- `cloud_index.py` optionally provisions isolated managed Postgres and real AWS
  S3 with resource-scoped STS credentials, keeping processes in Compose. It does
  not verify ECS scheduling. This optional cloud run is not implicitly verified
  by a local suite.
- Upgrade tests exercise the supported one-namespace old topology. Migration
  049 explicitly rejects multi-namespace roots rather than discarding data.

Current simplification results are recorded in
[implementation verification](../../docs/simplification-implementation.md).
[Historical records](HISTORY.md), including the v0.8.2 release matrix, describe
those revisions only.

## Native embedding batch limit

`scripts/test-e2e-worker-throughput.sh` is an optional paid throughput benchmark.
It compares initial capture and forced reindex through separate production
roles, real Postgres, LocalStack S3, and real Turbopuffer/native embeddings.
See [the measurements and reproduction settings](../../docs/indexing-performance.md).

`scripts/test-e2e-base64-redaction.sh` exercises large data URLs in TXT/JSONL,
escaped payloads, read/chunk boundaries, source-byte preservation, CSV cells,
real native embeddings, FTS/vector/hybrid search, exact redacted line reads,
authorization, append, deletion, forced re-extraction and service restarts.

`scripts/test-e2e-embedding-batches.sh` captures a synthetic 257-chunk file
with vectors enabled. A network relay records row counts and holds the first
real Turbopuffer response; the driver kills the index process and waits for
normal lease recovery. It checks the default batches of 64, 64, 64, 64 and 1
documents (below the provider maximum of 256), stable replay
payloads, exactly 257 native vectors, FTS/vector/hybrid search, exact reads and
persistence through both API restarts. External providers remain real.
