# PufferFS Architecture and Functionality

PufferFS turns changing local files into independently recoverable, searchable
versions while preserving source bytes and access controls.

The per-file pipeline is now the only implementation in this checkout. It is
not yet a production rollout: the deployed fleet may still run an older release.
Existing data needs a recapture audit before upgrading readers. See the
[implementation ledger](ingestion-implementation.md) for verification evidence.

## Per-file pipeline: deployment roles

```text
LOCAL AGENT --immutable source packs-----------------------------> S3
    |
    +--captured version metadata--> API SERVER --catalog---------> Postgres
                                       |
                                       +--work IDs--> TRANSFORM SQS FIFO + DLQ
                                                          |
                                              TRANSFORM CONSUMER [ECS]
                                                          | authenticated HTTP
                                              TRANSFORM WORKER [Modal CPU]
                                               reads original ranges from S3
                                                   /              \
                                          native parsing      temporary images/audio
                                               |                    |
                                               |               GEMINI BATCH
                                               |                    | results
                                               |          BATCH COLLECTOR [Modal CPU]
                                               +---------+----------+
                                                         |
                                         chunks to S3 + completion to Postgres
                                                         |
                                               INDEX SQS FIFO + DLQ
                                                         |
                                               INDEX CONSUMER [ECS]
                                                   /              \
                                          authenticated HTTP   authenticated HTTP
                                                /                  \
                                    INDEX CPU [Modal CPU]    INDEX GPU [Modal GPU]
                                        no vectors             Nomic embeddings
                                                \                  /
                                         vectors/mutations to S3
                                         mutations to Turbopuffer
                                         publication to Postgres

SEARCH: API --> QUERY EMBEDDER [separate Modal GPU pool] --> API --> Turbopuffer
RECOVERY: RECONCILER [scheduled Modal CPU] --> Postgres/S3 --> SQS/Turbopuffer
```

| Runtime role | Where / trigger | Consumes → produces / handoff |
| --- | --- | --- |
| Local agent | User machine; CLI sync or filesystem watch | Captured bytes → immutable S3 packs and API version registration; does not wait for indexing |
| API server | ECS; authenticated client HTTP | Capture metadata → Postgres catalog and transform SQS messages; serves catalog/read/search/ACL APIs |
| Transform consumer | ECS service; polls transform SQS | Claims receipts → bounded HTTP worker invocations; maintains visibility and acknowledges durable completion/handoff |
| Transform worker | `transform_app.py`, Modal CPU; HTTP invocation | S3 originals → native chunks in S3 or temporary Gemini inputs and durable Batch mappings; publishes ready index IDs |
| Batch collector | `collector_app.py`, Modal CPU; minute schedule | Provider mappings/results → S3 text chunks, extraction completion and index SQS messages |
| Index consumer | ECS service; polls index SQS | Claims receipts → CPU or Nomic index endpoint according to root configuration |
| CPU index worker | `index_cpu_app.py`, Modal CPU; HTTP invocation | Chunks/deletions → durable S3 mutations, real index writes and per-file publication; no embedding computation |
| GPU index worker | `index_gpu_app.py`, Modal GPU; HTTP invocation | Chunks and reusable vectors → Nomic vectors, durable mutations, index writes and per-file publication |
| Query embedder | `QueryEmbedder` in `query_app.py`, Modal GPU; API HTTP | Query text → Nomic query vector returned to API; separate deployment, endpoint-auth secret only |
| Reconciler | `reconciliation_app.py`, Modal CPU; minute schedule | Durable delivery/cleanup records → repaired SQS sends and bounded S3/index cleanup |

These roles share a repository, not a single server process. ECS starts the two
consumer services; they claim **SQS** receipts and invoke worker HTTP endpoints.
Modal starts/scales the worker containers and invokes scheduled roles. The query
role has its own application definition and container pool; it shares pinned
Nomic loading code with bulk indexing but receives no worker/database/provider
credentials. Transform, collector, CPU index, GPU index and reconciliation also
have separate application definitions.

Postgres is the catalog/ownership/recovery ledger, not an execution queue.
The API enqueues transformation jobs; native transforms and the collector enqueue
index jobs. Reconciliation repairs committed-but-unsent handoffs. Small job
messages carry IDs and artifact references, never file bytes, chunks or vectors.
Each FIFO queue has its own dead-letter queue for exhausted delivery attempts.

S3 retains source packs/manifests, chunks, vectors and replayable mutations.
Rendered document pages and converted media are temporary and never stored in
S3. PDF/Office/presentation inputs use local page rendering and Gemini Batch;
media uses temporary bounded clips and Gemini Batch. JSONL uses generic text
chunking, not a session-specific adapter. Publication is per file, with captured
and indexed heads tracked separately; no root-wide commit barrier is used.

The capture protocol and retention limits are documented in
[configuration](configuration.md). Compose preserves these role boundaries while
substituting local Postgres/LocalStack and CPU Nomic execution for cloud hosting;
it does not verify AWS IAM or production GPU behavior.

## Persistence and publication

Postgres stores tenants, permissions, file/version/extraction identities, source
extent references, provider submission mappings, work attempts, publication
pointers and cleanup records. Embedding bodies are in S3, not Postgres.

A captured version is immutable. Its transform writes ordered text chunks to S3.
The index worker builds durable vector/mutation packs, applies all mutation
batches to Turbopuffer, then advances the file's indexed extraction pointer.
Search validates candidates against those pointers; read pins one file's
publication throughout pagination. Pending or superseded rows are never public.
Replayed messages reuse durable artifacts and acknowledged progress.

## Shared product surfaces

The API also handles tenant authentication, root grants, deny-prefix ACLs,
content proofs, ignore policies, email login/invites, and optional billing.
The web console manages roots and credentials; the local CLI supports sync,
status/wait, search/read, supervised watch services and upgrades.

See [formats](file-ingestion-and-chunking.md), [API](api-reference.md),
[configuration](configuration.md), and [security](security-and-data-handling.md).

## Historical data

Executed SQL migrations remain intact. Read-only root inventory/audit and old
artifact-prefix cleanup remain so upgrading does not discard recoverable data.
No previous generation worker, upload endpoint, NATS backend or PostgreSQL
vector-body cache is used by this runtime. Removal of code does not delete
existing cloud services, queues, objects or database tables.
