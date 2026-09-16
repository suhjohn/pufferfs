# PufferFS Architecture and Functionality

PufferFS runs an API server, two queue consumers, transformation workers,
a collector dispatcher and collectors, a CPU index worker, and a reconciler.
They share a repository and deploy independently. Modal hosts CPU workers;
ECS hosts the API and consumers. Turbopuffer generates document and query
embeddings with `qwen/qwen3-embedding-8b`, 4096 float32 dimensions.

## Deployment topology

The web console's static assets are served by S3 and CloudFront. The browser
and CLI call the API through an AWS application load balancer (ALB).

```text
[Browser] <-- static assets -- [Web console: S3 + CloudFront]
    |
    | API requests                 [CLI / local agent]
    |                                  |         |
    v                                  |         | source packs
[API: ECS behind ALB] <---- requests ---+         v
    |          |                           [S3: originals + artifacts]
    |          +-- catalog / access --> [DB: Postgres]
    |
    | enqueue work IDs
    v
[TQ: Transform SQS FIFO + DLQ]
    | receive messages
    v
[Transform consumer: ECS]
    | authenticated HTTP
    v
[Transformation worker: Modal CPU] <-- captured sources -- [S3]
    |                           |
    | inline-media JSONL        | native extraction ready: work IDs
    v                           |
[Gemini Batch API]              |
    ^                           |
    | poll status/results       |
    v                           |
[Collector: Modal CPU]          |
    ^       |                   |
    | spawn | failed PNGs       |
    |       v                   |
    |   [DeepSeek V4.1 Flash]    |
    |    Modal shared endpoint  |
    |       | recovered text    |
    |       +--> Collector      |
    |                           |
[Collector dispatcher]          |
 Modal, every minute            |
                                |
[Collector] -- ready work IDs --+--> [IQ: Index SQS FIFO + DLQ]
                                    | receive messages
                                    v
                                [Index consumer: ECS]
                                    | authenticated HTTP
                                    v
                                [Index worker: Modal CPU]
                                    | text + metadata
                                    v
[API] -- authorized search/read --> [Turbopuffer]
                                    - full-text index
                                    - native Qwen embeddings
                                    - vector index

[Reconciler: Modal CPU, every minute]
    |-- repair work delivery ------------> TQ / IQ
    |-- remove obsolete data ------------> S3 / Turbopuffer
    +-- record recovery/cleanup progress -> DB

Transformation / Collector / Index workers:
    |-- source, chunks, manifests, mutations <--> S3
    +-- leases, work state, publication      <--> DB
```

Repeated labels refer to the same role or store. DLQ means dead-letter queue:
messages whose delivery attempts are exhausted.

| Role | Hosting / trigger | Input → output / handoff |
| --- | --- | --- |
| Web console | Static S3/CloudFront assets; browser interaction | User actions → authenticated API requests |
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

### 1. Capture / initial sync

```text
Local files
    |
    v
CLI: select files, detect changes, journal the capture
    |
    +-- immutable source packs ----------------------> S3
    |
    +-- capture metadata --> API: validate access and source references
                                |
                                +-- register versions/work --> Postgres
                                +-- enqueue IDs ------------> Transform SQS
                                +-- accepted response ------> CLI
```

Acceptance means the source is durably captured. `sync wait` waits separately
for extraction and searchable publication.

### 2. Extraction and model fallback

```text
Transform SQS --> ECS consumer --> Transformation worker
                                      |
                                      +-- claim work --> Postgres
                                      +-- read/verify source --> S3
                                      |
                  +-------------------+---------------------+
                  |                                         |
          Native/structured formats                 Documents/images/media
                  |                                         |
          Extract text/cells/records                Prepare PNGs/audio clips
                  |                                         |
                  |                                  Inline bytes in JSONL
                  |                                         |
                  |                                    Gemini Batch
                  |                                         |
                  |                        Dispatcher --> Collector polls
                  |                                         |
                  |                         +---------------+--------------+
                  |                         |                              |
                  |                  Successful items              Failed image items
                  |                         |                              |
                  |                         |                      DeepSeek on Modal
                  |                         |                              |
                  +-------------------------+------------------------------+
                                            |
                                  Persist/assemble ordered text in S3
                                            |
                                  Record ready work; enqueue Index SQS
```

Audio/video use audio clips; video visuals are not indexed. Gemini jobs that
are still running are polled later. Audio and remaining failures use the
existing Gemini retry path. DeepSeek runs only after terminal image failures.
Status/output retrieval failures and ambiguous submissions do not trigger it.

### 3. Indexing and publication

```text
Index SQS --> ECS consumer --> Index worker
                                 |
                                 +-- claim work ----------------> Postgres
                                 +-- read extracted chunks -----> S3
                                 +-- save replayable mutations -> S3
                                 |
                                 v
                         Turbopuffer: text + metadata
                                 |
                         Generate native Qwen embeddings
                         when vector indexing is enabled
                                 |
                         All index writes succeed
                                 |
                                 v
                         Publish catalog head in Postgres
                                 |
                         File becomes searchable/readable
```

### 4. Search

```text
CLI / web: query --> API: authenticate and resolve permitted roots
                         |
              +----------+-----------+
              |          |           |
          Full-text    Vector      Hybrid
              |          |           |
              |          +-----------+--> Turbopuffer embeds query text
              |                      |
              +----------------------+--> Turbopuffer searches its indexes
                                              |
                                              v
                                  API checks path permissions and
                                  published catalog heads in Postgres
                                              |
                                  Rank/merge authorized current results
                                              |
                                              v
                                          CLI / web
```

### 5. Read a file / page / line range

```text
CLI / web: read path + range
    |
    v
API: authenticate; check root and path access
    |
    v
Postgres: pin the file's published extraction
    |
    v
Turbopuffer: retrieve ordered rows for that extraction
    |
    v
API: assemble requested content and locations
    |
    v
Return the requested published text/pages/rows
```

Reads return extracted content. Retained original source packs are a separate
storage/authorization path.

### 6. Continuous sync and updates

```text
sync --follow / local service
    |
    v
Observe filesystem changes; debounce
    |
    v
Capture new file versions through the same CLI/API workflow
    |
    v
Transform --> Index --> Publish replacement
                          |
                          +--> New content becomes visible
                          +--> Old extraction becomes eligible for cleanup

Interrupted local capture --> resume its persisted journal
Unchanged files -----------> no replacement processing
```

The previous publication remains readable while a replacement is pending.
Captured deletions are hidden immediately instead of waiting for indexing.

### 7. Delete and retain/clean data

```text
File deletion in sync / root deletion through API
    |
    v
Postgres: record deletion; public retrieval hides it
    |
    +--> In-flight work loses publication authority
    |
    v
Scheduled reconciler
    +--> delete stale index rows from Turbopuffer
    +--> remove eligible S3 sources/artifacts under retention policy
    +--> keep durable cleanup identities for late writes

Collector --> finish/cancel obsolete provider work
          --> clean tracked Gemini input uploads
```

Cleanup is asynchronous. Provider-generated result files have provider-managed
retention; deleting a root does not establish their immediate physical erasure.

### 8. Failure and restart recovery

```text
Worker crashes / request response is lost
    |
    v
SQS redelivery or Postgres ownership lease expires
    |
    v
Another worker claims the same durable work
    |
    +--> provider job recorded? --> resume/discover the original job
    +--> results recorded? -----> preserve successful items
    +--> index mutation saved? -> replay the saved text mutation
    |
    v
Publish only if the file version and work ownership are still current

Missed queue send --> Reconciler --> enqueue committed work again
Stale late write --> hidden from retrieval --> scheduled cleanup
Exhausted SQS delivery attempts --> dead-letter queue
```

A lease is time-limited ownership of work. Retries can repeat provider inference
before a durable result is published; billing is not exactly once.

### 9. Access control

```text
Authenticated session / API key
    |
    v
API resolves identity and required operation scope
    |
    v
Check organization membership, role and root grants
    |
    v
Apply path/folder restrictions
    |
    +-- allowed --> perform operation on authorized data
    +-- denied ---> reject or hide inaccessible data

Capture commit --> revalidate authority before publishing captured metadata
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
