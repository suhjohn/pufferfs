# PufferFS Architecture and Functionality

PufferFS runs an API server, two queue consumers, transformation workers,
a collector dispatcher and collectors, a CPU index worker, and a reconciler.
They share a repository and deploy independently. Modal hosts CPU workers;
ECS hosts the API and consumers. Turbopuffer generates document and query
embeddings with `qwen/qwen3-embedding-8b`, 4096 float32 dimensions.

## Deployment topology

```text
[CLI / local agent - user machine]
    | capture metadata                    | immutable source packs
    v                                     v
[API server - ECS] --------------------> [S3]
    | catalog / permissions               ^
    v                                     | sources, chunks, manifests,
[Postgres]                                | replayable text mutations
    ^                                     |
    | durable state / leases              |
    +-- [Transformation workers] ---------+
    +-- [Collector workers] --------------+
    +-- [Index workers] ------------------+
    +-- [Reconciler] ---------------------+

WORK DISPATCH
[API]
    | enqueue work IDs
    v
[Transform SQS FIFO + DLQ] --> [Transform consumer - ECS]
                                      | authenticated HTTP
                                      v
                             [Transform worker - Modal CPU]
                                |                 |
                    native text |                 | inline media in JSONL
                                |                 v
                                |          [Gemini Batch API]
                                |                 ^
                                |                 | poll results
                                |          [Collectors - Modal CPU]
                                |                 ^
                                |                 | spawn invocations
                                |          [Collector dispatcher]
                                |          Modal, every minute
                                |
                                |          [Collectors]
                                |             |     |
                                |             |     +--> [DeepSeek V4.1 Flash]
                                |             |          Modal shared endpoint
                                |             |          failed images only
                                v             v
                            [Index SQS FIFO + DLQ]
                                      |
                                      v
                             [Index consumer - ECS]
                                      | authenticated HTTP
                                      v
                             [Index worker - Modal CPU]
                                      | text + metadata writes
                                      v
                             [Turbopuffer]
                              - full-text index
                              - native Qwen embeddings
                              - vector index

[API] -- authorized search / published reads --> [Turbopuffer]

[Reconciler - Modal CPU, every minute]
    --> repairs delivery to both SQS queues
    --> cleans obsolete Turbopuffer rows and S3 artifacts
    --> records progress in Postgres
```

DLQ means dead-letter queue: messages whose delivery attempts are exhausted.

| Role | Hosting / trigger | Input → output / handoff |
| --- | --- | --- |
| Local agent | User machine; sync/watch | Files → S3 packs and API version registration |
| API server | ECS; authenticated HTTP | Capture metadata → Postgres and transform SQS; queries → Turbopuffer |
| Transform consumer | ECS; polls transform SQS | Receipts → bounded worker HTTP calls; renews visibility and acknowledges durable results |
| Transform worker | Modal CPU; HTTP | S3 originals → native chunks or Gemini batches; ready extractions → index SQS |
| Collector dispatcher | Modal CPU; minute schedule | Configured count → spawned collector invocations |
| Collectors | Modal CPU; dispatcher | Leased Gemini work and optional DeepSeek image recovery → S3 text, durable batch state, index SQS and provider cleanup |
| Index consumer | ECS; polls index SQS | Receipts → one authenticated index endpoint |
| Index worker | Modal CPU; HTTP | S3 chunks → durable text mutations → Turbopuffer → Postgres publication |
| Reconciler | Modal CPU; minute schedule | Durable records → repaired delivery and source/artifact/index cleanup |

ECS starts the consumers; they receive SQS receipts and invoke HTTP workers.
Workers claim a Postgres lease before processing. Modal starts/scales worker
containers and invokes scheduled functions. The collector dispatcher starts
collectors explicitly. Four Modal applications contain these CPU roles:
`transform_app`, `collector_app`, `index_app`, `reconciliation_app`.
DeepSeek is a separate managed inference endpoint. The production collector has
its optional image fallback configured; see the [rollout and validation record](inline-media-and-vision-fallback.md).

## Durable publication

Postgres retains tenants, permissions, source references, file/version/extraction
identities, work leases, provider-batch coordination, namespace routing and cleanup
records. It is not the work queue. SQS carries bounded IDs, not source bodies.

S3 retains original packs, source manifests, ordered extracted text, provider
manifests and replayable text mutations. Rendered pages and converted media are
temporary. Embedding vectors live in Turbopuffer; there is no local vector cache.

Index rows have stable extraction-specific IDs. The index worker persists each
mutation artifact before writing, then publishes the catalog head only after
all writes succeed. An interrupted attempt replays its durable text; native
inference may run again. Search validates candidates against published catalog
heads; reads pin one publication across pages. Late/stale writes are hidden and
removed by recurring cleanup. Root deletion leaves permanent cleanup identities.

Vector-enabled schemas embed `content` into `vector`. Queries use
`["content", "ANN", ["Embed", query]]`; hybrid mode retains ANN/BM25 fusion.
Vector-disabled roots omit native embedding, using the same CPU worker and FTS.
The API no longer calls an embedding service or holds query vectors. Namespace
fan-out and publication retries can issue multiple native inference requests.

## Workflows

### Capture and extraction

```text
sync / follow
  --> detect changed files
  --> upload immutable source packs to S3
  --> API validates access and registers captured versions
  --> Postgres records transformation work
  --> API enqueues IDs in Transform SQS
  --> ECS consumer invokes transformation worker
  --> worker claims ownership and verifies captured source

      Text / structured formats:
        --> extract chunks directly
        --> store chunks in S3
        --> enqueue index work

      Documents / images / audio / video:
        --> render pages/frames or prepare audio clips
        --> embed media bytes in JSONL
        --> upload one JSONL per batch of at most 64 inputs
        --> submit Gemini batch
        --> persist batch identity and manifests
        --> collector takes over
```

Capture returns after durable acceptance. Extraction and indexing continue
asynchronously; `sync wait` waits for publication.

### Collection and image fallback

```text
minute dispatcher
  --> spawn collectors
  --> claim due provider batches in Postgres
  --> inspect Gemini status

      Still running:
        --> schedule a later check

      Terminal result:
        --> preserve successful Gemini items
        --> recover failed image items through DeepSeek
        --> preserve original page/frame order
        --> persist results; assemble complete extracted text
        --> enqueue index work
        --> clean tracked Gemini input uploads
```

Audio remains on Gemini. Remaining failed items use the bounded inference retry
path. Slow nonterminal jobs, ambiguous submissions and status/output retrieval
failures do not trigger vision fallback.

### Indexing and publication

```text
Index SQS
  --> ECS index consumer
  --> Modal index worker claims work
  --> read extracted chunks from S3
  --> persist replayable text mutations in S3
  --> write text and metadata to Turbopuffer
  --> Turbopuffer generates native document embeddings when enabled
  --> publish catalog head in Postgres after all writes succeed
```

### Search and read

```text
CLI / web --> API --> validate permissions and root selection

Search:
  --> Turbopuffer full-text, vector or hybrid query
      vector query text --> native query embedding
  --> validate candidates against current published catalog heads
  --> return authorized results

Read:
  --> select and pin one published extraction
  --> retrieve requested text/pages/rows
  --> return content and locations
```

### Updates, failures and restarts

```text
Changed file --> new captured version --> extract --> index --> publish

Worker interruption:
  --> SQS redelivery or expired Postgres lease
  --> another worker resumes durable work
  --> reuse recorded provider jobs/results or index mutations

Missed queue send:
  --> reconciler finds durable unsent work
  --> enqueue again

Late result from an older version:
  --> ownership checks reject publication
  --> stale search rows stay hidden
  --> reconciler removes obsolete data
```

Retries can repeat provider inference before a durable result is published;
the system does not promise exactly-once billing.

### Deletion

```text
Delete file/root
  --> API records deletion and hides it from normal retrieval
  --> in-flight work loses publication authority
  --> cleanup removes index rows and eligible S3 data
  --> provider cleanup continues from durable records
```

## Operations and verification

[Deployment](production-deployment.md) describes the clean cutover from the
retired Nomic stack. Old supplied-vector mutation artifacts cannot replay in the
new worker. Migration 047 drops the old embedding pack directory after old
writers and authorized cache objects have been retired.

Compose runs the same production entrypoints as separate processes with real
Postgres, LocalStack S3/SQS and real Gemini/Turbopuffer providers. Cloud E2E adds
real AWS and Modal CPU execution. Read the [E2E record](../tests/e2e/README.md)
for actual results and differences from production.

See [formats](file-ingestion-and-chunking.md), [API](api-reference.md),
[configuration](configuration.md) and [security](security-and-data-handling.md).
