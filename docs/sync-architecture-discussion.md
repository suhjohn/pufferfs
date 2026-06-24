# PufferFS Sync Architecture: How We Got Here

## Executive summary

We started with a practical performance problem: PufferFS sync/indexing was too slow for real document workspaces and clearly would not scale to very large trees. The conversation evolved from “how do we speed this up?” into a full to-be architecture for high-throughput sync:

- **Postgres is control plane only**: roots, jobs, generations, coarse progress.
- **S3/R2 is the data plane**: source bundles, manifests, queue state, chunks, index artifacts.
- **Queue jobs are batch pointers**: no per-file Postgres rows, no per-file durable queue rows.
- **Small files are packed into bundle objects**: no one S3 PUT per small file.
- **Work runs as durable stages**: chunk → embed → index.
- **Turbopuffer rows use generation validity windows**: unchanged files do not get rewritten every sync.
- **Visible generation is the atomic correctness boundary**: partial failed syncs do not leak into query results.

The current PR implements that architecture in a pragmatic first full version:

PR: <https://github.com/suhjohn/pufferfs/pull/2>

Latest implementation includes:

- object-storage queue broker,
- stage-separated sync pipeline,
- async CLI polling,
- Turbopuffer validity windows,
- tombstone/close behavior for move/delete,
- real integration test coverage against Modal + Turbopuffer.

---

## 1. Original problem: sync/indexing throughput

The original concrete problem was that syncing real docs was too slow. The earlier implementation bottlenecked on:

- per-file processing,
- slow Modal document chunking/OCR paths,
- embedding calls that were too small/sequential,
- Turbopuffer writes that were not streamed/batched enough,
- request-local/in-memory sync state.

For a normal small workspace, this could be made acceptable with worker tuning. But once we started reasoning about **10M files**, the problem changed: it was no longer just “make this request faster.” It became:

> Build a high-throughput, failure-safe, incremental indexing pipeline that can stream and batch work continuously while preserving correct snapshot visibility for queries.

---

## 2. The first implementation slice

Before the full architecture, PR #2 had already implemented a minimal viable architectural foundation:

- packed small-file bundles,
- bundle manifests,
- server-side reads by source key / offset / length,
- generation rows,
- `roots.visible_generation_id`,
- `generation_id` on Turbopuffer rows,
- visible-generation query filtering,
- generation-scoped index-row artifacts,
- streaming Turbopuffer writes.

That version was tested successfully against real services:

- R2 / S3-compatible object storage,
- Modal chunk/embed endpoints,
- Turbopuffer.

It validated the core direction, but it was not yet the full architecture. The missing pieces were:

- durable object-storage queue broker,
- independent chunk/embed/index stages,
- validity-window row reuse,
- async job UX,
- metadata rewrite / tombstone behavior for moves/deletes.

---

## 3. Key constraint: no per-file writes in Postgres

A major discussion point was avoiding one write per file anywhere expensive or coordination-heavy.

The agreed Postgres model:

```text
roots
  id
  visible_generation_id

sync_jobs
  id
  root_id
  status
  generation_id / progress counters
  manifest refs

sync_generations
  id
  root_id
  base_generation_id
  seq
  base_generation_seq
  status: building / visible / failed / superseded
  manifest_ref
  visible_at
```

Postgres should not contain:

```text
no row per file
no row per chunk
no row per embedding
no row per work item
no vector blobs
```

This was the main architectural line: Postgres should coordinate, not carry the data plane.

---

## 4. Key constraint: no one S3 PUT per small file

At 10M tiny files, one S3 object per file would be operationally and economically bad:

```text
10M files -> 10M PUTs + 10M GETs
```

So we agreed on packed source bundles:

```text
CLI uploader
  groups small changed files into large binary bundle objects
  writes manifest rows with path/hash/offset/length

Workers
  read bundle once or range-read slices
  process individual files from offsets
```

Example object layout:

```text
bundles/{root_id}/bundle-000001.bin
syncs/{sync_id}/inputs/batch-000001.jsonl
```

Manifest row shape:

```json
{
  "path": "docs/a.md",
  "file_hash": "...",
  "source_key": "bundles/{root_id}/bundle-000001.bin",
  "source_offset": 1048576,
  "source_length": 2931
}
```

This changes 10M tiny-file ingest from millions of object calls to hundreds/thousands of large object calls, depending on bundle size.

---

## 5. Queue model: object-storage broker, batch jobs only

The next decision was that durable work scheduling should not be a Postgres row per file.

Instead, queue state lives in S3/R2 objects. Jobs are batch pointers:

```json
{
  "job_id": "...",
  "sync_id": "...",
  "generation_id": "...",
  "stage": "chunk|embed|index|cleanup",
  "payload_ref": "syncs/.../inputs/batch-000001.jsonl",
  "status": "queued|running|done|failed",
  "lease_until": "...",
  "attempts": 0
}
```

The broker owns queue state files:

```text
syncs/{generation_id}/queues/chunk.queue.json
syncs/{generation_id}/queues/embed.queue.json
syncs/{generation_id}/queues/index.queue.json
```

The implemented broker supports:

- `Push`,
- `Claim`,
- `Complete`,
- `Fail`,
- leases,
- retry attempts,
- CAS-style writes using object ETags,
- deterministic queue object keys.

This is the minimum durable queue substrate needed to split the old request-local sync into independent stages.

---

## 6. Pipeline shape: chunk → embed → index

The desired full pipeline became:

```text
StartSync
  create sync_job
  create building generation
  write input manifests
  push chunk jobs
  return sync_job_id

Chunk stage
  claim input manifest jobs
  read bundled/standalone source bytes
  call Modal chunk endpoint
  write chunk artifact
  complete chunk job and push embed job

Embed stage
  claim chunk artifact jobs
  dedupe/cache by content_hash
  call Modal embed endpoint for misses
  write index-row artifact
  complete embed job and push index job

Index stage
  claim index-row artifact jobs
  batch Turbopuffer writes
  close stale rows for modified/moved/deleted paths
  complete index job

Commit
  save state/proof
  mark generation visible
  mark sync_job completed
```

The current implementation follows this shape in:

```text
internal/server/object_queue.go
internal/server/sync_pipeline.go
```

---

## 7. Generation visibility and correctness

A central correctness requirement was:

> Turbopuffer may receive rows before a sync is committed, but users should only query the last committed snapshot.

Earlier PR #2 used simple generation filtering:

```text
generation_id == visible_generation_id
```

That works for correctness, but not for 10M-file incremental reuse, because unchanged rows would need to be copied into every generation.

So we moved to validity windows.

Each Turbopuffer row now has:

```text
valid_from_generation
valid_from_generation_seq
valid_to_generation
valid_to_generation_seq
```

Active row filter:

```text
valid_from_generation_seq <= visible_generation_seq
AND (
  valid_to_generation_seq == 0
  OR valid_to_generation_seq > visible_generation_seq
)
```

This means:

- unchanged files remain active without rewriting rows,
- modified files add new rows and close old rows,
- deleted paths close old rows,
- moved paths close old-path rows and write new-path rows,
- generation flip remains the atomic commit boundary.

---

## 8. Embedding cache discussion

We discussed whether an embedding cache was necessary and whether it belonged in Postgres.

The refined conclusion:

> The real requirement is not “a Postgres embedding cache.” The real requirement is durable vector reuse by `(model_version, content_hash)`.

This mapping could live in:

- a Postgres cache table for current/simple implementation,
- packed S3/R2 embedding blocks,
- a dedicated Turbopuffer namespace,
- previous active rows when available.

We agreed that at 10M-file / 30M-chunk scale, Postgres should not be the long-term vector store. But the current code still uses the existing embedding cache path as a practical reuse mechanism while keeping the broader architecture compatible with a future S3/TP-backed embedding store.

Important nuance:

- unchanged validity-window rows need no embedding lookup at all,
- pure deletes need no embedding,
- known moves can reuse/copy existing row metadata,
- retries/rebuilds/duplicate content still benefit from content-hash vector reuse.

---

## 9. Move/delete behavior

The old model could delete rows or rewrite whole generations.

The new model uses validity windows:

```text
delete:
  patch active rows for path:
    valid_to_generation = current_generation
    valid_to_generation_seq = current_seq

move:
  close old path rows
  create new rows at new path using existing row metadata/vector where possible
```

This is important because:

- deletes do not need rechunking/reembedding,
- moves do not need rechunking/reembedding,
- old rows remain as historical/inactive rows for cleanup later,
- queries filter them out by visible generation.

The implementation uses Turbopuffer `patch_rows` for closes so vector fields do not need to be resent.

---

## 10. Async sync UX

The architecture wants sync to be a job, not one long blocking HTTP request.

The current implementation adds:

```text
CLI POST /roots/{id}/sync?async=true
server returns sync_job_id
CLI polls /roots/{id}/sync/status?job_id=...
CLI saves local state only after completed
```

This preserves the current CLI behavior from the user’s perspective:

```text
pufferfs sync ...
```

still waits until completion before printing “Sync complete,” but internally it is now job/poll based.

---

## 11. What was implemented

Main commits pushed to PR #2:

```text
067175e feat: full distributed sync pipeline — queue broker, stage workers, validity windows, async polling
c140839 fix: preserve query results when content proof is absent
```

Important files:

```text
internal/server/object_queue.go
  object-storage broker with Push/Claim/Complete/Fail

internal/server/sync_pipeline.go
  durable chunk/embed/index stage pipeline

internal/server/turbopuffer.go
  validity-window schema fields and patch_rows support

internal/server/indexed_chunk.go
  generation-scoped chunk IDs and validity-window row fields

internal/server/db.go
  generation seq/base_generation_seq support

migrations/008_sync_generation_validity.sql
  generation sequence columns

cmd/pufferfs/sync.go
  async sync POST and polling

tests/integration_test.go
  assertions for sync artifacts and closed validity-window rows
```

---

## 12. Review feedback handled

Devin Review had three comments from the earlier PR state:

### 1. `normalizeSyncRequest` not called

Concern:

```text
sync request paths were not normalized/validated
```

Current state:

```go
req.RootID = rootID
if err := normalizeSyncRequest(&req); err != nil {
    writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
    return
}
```

This is now handled before queueing/indexing.

### 2. zero chunks on modified files could leave stale rows

Concern:

```text
modified file old rows might not be removed if new chunking returns zero chunks
```

Current state:

The new pipeline emits a `close` artifact for modified paths before new chunks are indexed. Therefore old active rows are closed even when the replacement file yields zero chunks.

### 3. content-proof fallback returned nil

Concern:

```text
querying without a stored content proof returned zero rows
```

Fixed in `c140839`:

```go
proofBytes, _, err := s.db.GetContentProof(ctx, orgID, userID, rootID)
if err != nil {
    return rows
}

var proof models.ContentProofData
if err := json.Unmarshal(proofBytes, &proof); err != nil {
    return rows
}
```

---

## 13. Validation performed

Local/static checks:

```bash
go test ./...
go vet ./...
```

Real integration test:

```bash
go test -tags=cli_integration -run TestPufferFSEndToEnd ./tests -count=1 -v -timeout=10m
```

Result:

```text
PASS
ok github.com/pufferfs/pufferfs/tests 29.373s
```

The real integration test exercised:

- CLI sync,
- packed bundles,
- S3/R2/MinIO artifacts,
- Modal chunk/embed endpoints,
- Turbopuffer indexing/querying,
- modify,
- move,
- remove,
- sync --follow,
- failed-index retry behavior,
- closed validity-window rows for moved/deleted paths.

GitHub currently reports no configured status checks/check-runs on the latest PR SHA, so there was no remote CI to wait for.

---

## 14. Current state of the architecture

The PR now represents the first full implementation of the architecture we discussed:

```text
Postgres:
  roots
  sync_jobs
  sync_generations
  visible_generation_id
  coarse progress/state/proof

S3/R2:
  bundles
  input manifests
  queue state
  chunk artifacts
  index-row artifacts

Broker:
  batch-level object queue
  CAS queue commits
  leases/retries

Workers:
  chunk stage
  embed stage
  index stage

Turbopuffer:
  derived index sink
  validity-window rows
  patch_rows closes old active rows

CLI:
  async job start
  polling until committed
  local state saved only after success
```

---

## 15. Remaining future refinements

The current PR implements the architecture in-process on the server. It establishes durable queue/artifact boundaries, but the next scale step would be to run workers as separately deployed processes:

```text
pufferfs worker --stage chunk
pufferfs worker --stage embed
pufferfs worker --stage index
```

Other likely future refinements:

- multiple queue shards per root/stage,
- larger chunk batch endpoint for simple text/code files,
- S3/R2-backed embedding vector blocks instead of Postgres cache,
- dedicated Turbopuffer embedding namespace for vector reuse,
- cleanup workers for inactive rows/artifacts,
- observability around queue depth, stage latency, retry counts, and TP flush metrics,
- superseding/canceling older building generations when a newer sync starts.

Those are scale/operational improvements on top of the now-implemented core model.

---

## 16. Short version

We started with “sync is too slow,” then made it faster, then asked what would happen at 10M files. That forced the architecture toward:

- batch everything,
- do not put per-file state in Postgres,
- do not upload one object per tiny file,
- schedule work with object-storage queues,
- split compute into durable stages,
- keep Turbopuffer as a derived sink,
- use generation validity windows for incremental correctness,
- flip visibility atomically only when the sync is complete.

That is the architecture now implemented in PR #2.

---

## 17. Manifest-session sync (PR #17)

PR #17 introduces explicit sync sessions to move large control-plane payloads
out of the `POST /sync` request body:

```text
Client                              Server
  |                                    |
  |-- POST /sync/init ---------------->|  create SyncJob + SyncGeneration
  |<-- {generation_id, manifest_prefix}|
  |                                    |
  |-- POST /sync/{gen}/upload -------->|  store manifest shard
  |-- POST /sync/{gen}/upload -------->|  store content proof
  |-- POST /sync/{gen}/upload -------->|  store compressed state
  |                                    |
  |-- POST /sync (finalize) ---------->|  small request: gen_id + change_refs
  |<-- SyncResponse -----------------  |  after commit
```

Key properties:

- **Generation-scoped artifact namespace**: transient sync artifacts live under
  `syncs/<generation_id>/manifests/`, `syncs/<generation_id>/proofs/`, and
  `syncs/<generation_id>/sources/`.
- **Small finalize request**: the sync POST carries only a `generation_id` and
  a list of `change_refs` pointing to manifest shards, not inline file changes.
- **Abort semantics**: if the client fails before finalize, it calls
  `DELETE /sync/{generation_id}` to mark the generation failed and clean up.
- **Backward compatible**: inline `changes` without `generation_id` still works
  for small syncs or old clients.
- **Scalability contract**: the final sync request size is bounded by shard
  count, not file count. At 1M files with 5000 files/shard, the finalize request
  carries ~200 refs instead of 1M inline change records.

This is the recommended path for any sync exceeding a few thousand files. The
CLI uses this flow by default.

## 18. Subset sync

The current CLI also supports subset updates:

```bash
pufferfs sync --root /Users/me/workspace --include "docs/**" --exclude "docs/archive/**" --name workspace
```

This is not a partial-generation commit. The client treats repeated `--include`
patterns as OR, subtracts any `--exclude` matches, loads the current committed
root state when the root already exists, hashes selected files, and patches those
entries into a complete merged state map. It then uses the same manifest-session
flow as a normal sync: upload selected source bytes, upload change shards,
upload content proof and state, then finalize the generation.

Important invariants:

- Unselected files remain in the merged root state and stay visible after the
  generation commits.
- Missing selected files become removals only when they exist in the base state.
- The server still validates ignore policy, ACLs, base generation, source refs,
  and the requirement for a full `state`/`state_ref`.

During prod integration testing, the deployed Modal embed endpoint accepted the
base chunk schema but returned HTTP 500 when line metadata fields were included
in the embedding request. The server therefore strips `line_start` and
`line_end` from the Modal embed payload while preserving them in the index row.
