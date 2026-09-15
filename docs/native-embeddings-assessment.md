# Native Qwen embeddings: migration and removal inventory

## Decision

Use Turbopuffer native `qwen/qwen3-embedding-8b` embeddings with 4096 float32
coordinates and cosine distance. A single CPU index worker writes extracted
text; Turbopuffer generates the vectors. The API submits native query embedding
expressions directly to Turbopuffer.

[Native embedding documentation](https://turbopuffer.com/docs/embedding)
explains the schema, supported model, pricing, query expressions and migration
limits. Checked September 15, 2026: Qwen 8B is listed at $0.07 per million tokens.
This is inference pricing, separate from search/storage charges. Availability
was verified with real writes and searches using the repository credentials.

See [deployment topology](architecture-and-functionality.md) for the diagram and
role table. ECS runs the API and two SQS consumers. Modal hosts four applications:
transformation, index, collector/dispatcher, and reconciliation. These remain
separate deployable roles within one repository.

## Removed implementation and infrastructure

| Area | Removed | Replacement / retained responsibility |
| --- | --- | --- |
| Query service | `modal/query_app.py`, warm containers, model initialization and lock | API uses `content ANN [Embed, query]` in each namespace request |
| GPU indexing | `modal/index_gpu_app.py`, GPU allocation and encoder serialization | `modal/index_app.py`, CPU only |
| Duplicate CPU routing | `modal/index_cpu_app.py`, `MODAL_FILE_CPU_INDEX_ENDPOINT`, consumer root-flag routing | One index endpoint handles vector and no-vector roots |
| Model implementation | `modal/nomic_model.py`, model/code revisions, CPU/CUDA handling, normalization, document/query prefixes | Turbopuffer manages Qwen document and query inference |
| Model dependencies | `modal/requirements-embedding.txt`, PyTorch, Transformers, Sentence Transformers, einops, download/image stages | Lightweight index requirements |
| Vector cache | `modal/embedding_artifacts.py`, hash lookup SQL, deduplication, range reads and S3 vector packs | Vectors stored only in Turbopuffer |
| Cache maintenance | `modal/embedding_cleanup.py`, reconciler hook, retention setting | Migration 047 drops `embedding_packs`; old objects explicitly erased |
| Vector serialization | Supplied vectors/base64 in new mutation rows | Durable text mutation packs; replay rejects supplied vector fields |
| API plumbing | `EmbedQuery`, request/response types, private retry helper, embedding arguments, Modal client dependency | Native query expression; publication filtering and authorization retained |
| Consumer dependencies | Server construction, S3 client, search client, API configuration load | Direct database, queue and worker HTTP client |
| Credentials | Modal auth/endpoints supplied to API; unrelated API/model secrets supplied to consumers | API has search credentials; consumers have database and worker auth |
| Deployment | GPU/query settings, retired IAM app identities and old endpoint variables | Four CPU apps; IAM trust includes `pufferfs-index` |
| Tests | Query process, vector Docker stages, `cloud_query.py`, cache IO/relay suite, cache-retention assertions | Real native-provider vectors, schema, text replay and public search assertions |
| Benchmarks | Cache hit/pack/vector JSON metrics and GPU-specific capacity report | Initial/reindex timings, text mutation sizes, current worker metrics |

Historical migrations remain to support upgrades from existing databases.
`embedding_cache` and `embedding_locations` were removed by earlier migrations;
047 removes the final `embedding_packs` table. Historical benchmark documents
are explicitly labeled and are not measurements of the new implementation.

## What remains necessary

- Source capture, immutable source packs, native parsing and Gemini document/media extraction.
- Transform/index FIFO queues and their dead-letter queues. Consumers receive
  messages; workers claim database leases. The reconciler repairs missed deliveries.
- Collector dispatch, batch polling, provider-file cleanup and durable provider manifests.
- Chunk ordering, content hashes, deterministic row identities and immutable text mutations.
- Publication checks, stale-row cleanup, retries, root tombstones and authorized deletion.
- File/version/extraction catalog, permissions, source references and namespace routing.
- Full-text, vector and hybrid search, plus `--no-vector` roots using the same CPU worker.

## Migration constraints

1. Changing the schema does not backfill existing vectors. Old Nomic data must
   be rebuilt; this rollout uses the explicitly authorized empty-data reset.
2. Old supplied-vector mutation artifacts would bypass native inference. Stop
   old workers first, remove old work/artifacts, and reject supplied-vector replay.
3. Migration 047 is irreversible. Remove old cache data before dropping its
   directory table, and keep old worker deployments stopped.
4. Append/reindex republishes a complete extraction. Without our content cache,
   unchanged chunks can incur inference again. Retries can repeat inference too.
5. Query fan-out embeds within namespace requests, including publication retries.
   Measure production latency and billed tokens before claiming cost savings.
6. 4096 float32 vectors occupy 16 KiB per chunk before provider overhead, versus
   3 KiB for the previous 768-dimensional vectors. Native embedding removes our
   serving infrastructure; it does not imply lower total storage or search costs.
7. Native inference can return 429 or provider errors. Existing durable retries
   remain necessary. Retrieval quality and capacity require separate measurements.

## Authorized reset and verification record

The reset is restricted to the two explicitly authorized, verified sole-owner
workspaces. The exact ownership and deletion inventory is retained privately.
Account identities and local original files are retained. No other tenant’s
content is included.

Reset operations (old workers stopped before cache and table removal):

- Paused both production consumers and stopped all six old Modal applications.
- Deleted two roots through the authenticated public API: 23,501 S3 objects removed.
- Removed 20,039 old S3 embedding packs and their database directory rows.
- Erased remaining authorized source/extraction/mutation prefixes and nine provider manifests.
- Deleted and verified absence of eight old search namespaces.
- Removed three completed Gemini batch jobs and their database records.
- Removed 241 authorized index dead-letter messages; no unrelated messages were removed.
- Cleared local PufferFS caches for both deleted roots and the temporary verification roots.
- Retained metadata-only root deletion tombstones for protection against late writes.
- Twelve historical Gemini file IDs returned ambiguous 403 responses. Their
  recorded cleanup was already complete, but this reset cannot independently
  confirm deletion versus lack of access. No fresh deletion is claimed for them.

## Deployment and tests

Commit `8b36d16` deployed the backend, infrastructure, four Modal applications,
frontend and installer through a
[successful production workflow](https://github.com/suhjohn/pufferfs/actions/runs/34939860522).
Commit `e5b7c1d` then corrected maintenance schema handling through a
[successful backend deployment](https://github.com/suhjohn/pufferfs/actions/runs/34941169825).
Both API replicas and both consumers are running. The API has no Modal
credentials/endpoints; consumers receive only database and worker-auth secrets.
Migration 047 is applied, and the three retired index/query applications are
stopped. No new CLI release was published; live checks use released CLI v0.8.1.

- Go, Python compilation, shell syntax, Pulumi TypeScript, web build and GitHub
  build checks passed.
- The complete index-recovery Compose suite passed all 14 phases, including
  cleanup, in run `85682438ef2349a1b3b192842dde3e01`. It uses separate production
  roles, real Postgres, S3/SQS-compatible services and real Turbopuffer/Qwen.
  Coverage includes lost responses, worker crashes, database restart, consumer
  disconnects, bounded execution, stale writes, root deletion and recurring cleanup.
- Live CLI/API verification passed capture, model/dimension inspection, vector,
  hybrid and full-text search, exact read, update, deletion, invalid-credential rejection and
  no-vector behavior. All temporary verification roots were removed.
- A live search returned HTTP 500 after maintenance. A focused check reproduced
  loss of the native `embed` schema setting after five successful update cycles.
  The reconciler was re-sending the base schema without `embed`. Cleanup now
  sends deletion commands without schema updates; the recovery suite adds an
  assertion after scheduled cleanup. The corrected full recovery suite passed.
  Production also passed five update cycles followed by scheduled cleanup,
  physical stale-row deletion, native schema/vector inspection and all searches.
- Full corpus run `14d88bd3da2b4c3e880e743d51a9fddc` completed capture, delivery
  recovery, append-following, multipart recovery and 1,043 index jobs. Gemini
  batch items returned code 7, “The caller does not have permission,” while five
  inspected input uploads remained ACTIVE. The run was stopped and its cleanup
  passed; the full corpus suite is **not** a pass. The reconciler was upgraded
  during this run before the vector portion. No provider was mocked or replaced.
- GitHub E2E jobs remain gated by the repository’s protected `e2e` environment
  and are not claimed as passed.

A test assertion was corrected to allow absent empty shards while still
requiring the full expected published chunk count. Earlier runs used that old
assertion or were interrupted during harness editing; only the complete current
recovery run above is reported as a suite pass.

The final audit found zero roots/provider-batch records in the authorized
workspaces, no embedding table, all 29 authorized storage prefixes empty, all
eight old namespaces absent, and both work queues and both dead-letter queues
empty. The two account memberships remain intact. Four CPU Modal apps are
deployed; the retired CPU/GPU index and query apps remain stopped.

These checks do not establish production capacity, native-provider rate-limit
behavior, billed-token savings or corpus-wide retrieval quality.
