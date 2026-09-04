# PufferFS Architecture and Functionality

This document summarizes the current PufferFS codebase as implemented in this
repository. It focuses on the executable surfaces, core data flows, storage
layout, access model, and supported product functionality.

## Product Shape

PufferFS indexes a local filesystem root so users and agents can search it with
hybrid text/vector retrieval. The system is split into:

- A Go CLI for syncing, querying, root management, service
  supervision, and direct-install upgrades.
- A Go API server that owns authentication, tenancy, root metadata, sync
  orchestration, query proxying, billing, and administrative provisioning.
- Optional Go worker processes that consume queued sync stages from SQS or
  NATS JetStream.
- Python Modal functions that convert rich files and embed chunks and queries.
- Turbopuffer namespaces for BM25/vector/hybrid search.
- S3-compatible object storage for temporary source transport objects, durable
  root state, sync artifacts, and page images.
- PostgreSQL for control-plane state plus small durable caches: orgs, users,
  API keys, roots, ACLs, sync jobs/generations, root state refs, embedding
  cache, content proofs, and subscriptions.
- A small React web app for login, dashboard, organization, and optional
  billing management.

## Repository Map

- `cmd/pufferfs`: CLI entrypoint and commands.
- `cmd/server`: HTTP API server wiring.
- `cmd/worker`: SQS/NATS-backed sync stage worker.
- `cmd/runtime`: container runtime switch that execs server or worker.
- `internal/server`: API handlers, DB access, sync pipeline, dispatchers,
  Turbopuffer client, Modal client, billing, cleanup, query helpers, local
  chunking, namespace routing.
- `internal/auth`: JWT, API key, admin key, OAuth, session cookie, and CORS
  middleware.
- `internal/config`: TOML/environment configuration loading.
- `internal/diff`, `internal/merkle`, `internal/ignore`: filesystem scanning,
  hashing, diffing, content proofs, and ignore handling.
- `internal/queue`: SQS FIFO and NATS JetStream queue backends.
- `internal/storage`: S3-compatible object storage client.
- `pkg/models`: shared Go request/response/domain models.
- `modal`: Modal app, file chunkers, and Python models.
- `migrations`: PostgreSQL schema evolution.
- `web`: React/TanStack Start web console.
- `infra/pulumi`: AWS infrastructure.
- `deploy/nats`: NATS container support.
- `skills/deploy-pufferfs`: local Codex deployment skill/runbook.

## Executable Surfaces

### CLI

The CLI root command is `pufferfs` and includes:

- `whoami`: display the authenticated email, user and organization IDs, role,
  and current credential scopes; `--json` returns the `/auth/me` response.
- `sync [path]` / `sync --root <path>`: scan a directory, compute a diff,
  upload changed file content, submit a sync request, poll for async
  completion, and update local cache. If `--name` is omitted, the root name
  defaults to the directory basename.
- `sync --root <path> --include <glob> [--exclude <glob>]`: update a subset of
  files. Multiple includes are additive, excludes win, and the CLI patches
  selected changes into the current committed root state so unselected files
  remain visible.
- `sync --dry-run`: show changes, total upload size, and ignored patterns
  without uploading.
- `sync --background` / `sync --detach`: submit the same server-side sync job
  but return immediately with a `sync_job_id`.
- `sync status`, `sync jobs`, and `sync wait`: inspect recent sync jobs, poll a
  specific job, or block until it reaches `completed`/`failed`.
- `sync --follow`: run an initial sync, then use `fsnotify` to debounce
  filesystem changes and rerun sync.
- `query`: search a synced root with `fts`, `vector`, or `hybrid` mode, optional
  path glob, and `top-k` control.
- `read`: retrieve deterministic page or line ranges from a known indexed file;
  page reads can return/download rendered page images.
- `root delete`: delete PufferFS metadata, object storage artifacts,
  Turbopuffer namespaces, and local cache for a root.
- `service`: install/start/stop/restart/status/logs/uninstall a user-level
  background sync service using launchd on macOS or systemd user services on
  Linux.
- `init`: write local config.
- `upgrade`: self-upgrade direct installs using the server's CLI release
  manifest, SHA-256 verification, archive extraction, binary replacement, and
  optional service restart.

CLI config is read from `~/.tpfs/config.toml` and can be overridden with
environment variables such as `PUFFERFS_SERVER_URL`, `PUFFERFS_API_KEY`,
`TURBOPUFFER_API_KEY`, and AWS storage settings. Per-root cache lives under
`~/.tpfs/roots/<rootID>/`.

### API Server

`cmd/server` loads config, opens Postgres with migrations, creates S3/Modal/
Turbopuffer clients, optionally attaches a durable queue, configures optional
Stripe billing, and exposes HTTP routes. Normal routes accept either JWT
session credentials or tenant API keys. `/admin/*` routes use a separate
platform admin key when configured.

Important API surfaces:

- Health and readiness: `GET /healthz`, `GET /readyz`, `GET /health`.
- CLI version manifest: `GET /cli/version`.
- Login/session: `GET /auth/providers`, `GET /auth/google`,
  `GET /auth/callback`, `POST /auth/email/start`,
  `POST /auth/email/verify`, `POST /auth/logout`, `GET /auth/me`.
- API keys: create, list, delete.
- Org management: get org, list/add/remove members.
- Platform admin: provision/delete orgs and users, upsert org membership,
  create member API keys, create/delete roots.
- Roots: create, list accessible roots, get metadata, delete, upload file,
  upload bundle, sync init (create sync session), sync artifact upload,
  sync abort, sync finalize, get state, sync status, list sync jobs.
- ACLs: create/list/delete path-prefix ACLs.
- Query/read: `POST /query`, `POST /roots/{id}/read`, and authenticated
  `GET /roots/{id}/assets` for returned page/image artifacts.
- Billing: get subscription, create Stripe checkout session, receive Stripe
  webhook when billing is enabled.

### Workers

`cmd/worker` runs one stage at a time: `chunk`, `index`, or `commit`.
It connects to the same database/storage/Modal/Turbopuffer dependencies as the
server plus the selected queue backend. Workers pull batches, process
jobs concurrently, heartbeat long jobs, retry with backoff, mark failed
generations/jobs after max attempts, and skip work for terminal generations.

### Container Runtime

The Docker image builds `/pufferfs-server`, `/pufferfs-worker`, and
`/pufferfs-runtime`. The default command runs `/pufferfs-runtime`, which execs
the server unless `PUFFERFS_PROCESS=worker` or `PUFFERFS_WORKER_STAGE` is set.

## Core Data Model

The control plane is PostgreSQL:

- `organizations`, `users`, `org_members`, `api_keys`.
- `roots`: logical sync/access unit with org, scope, owner, source path,
  visible generation, and visible generation sequence.
- `root_index_namespaces`: physical Turbopuffer namespace shards per root.
- `root_states`: root file-state JSON or an object-storage `state_ref`.
- `sync_jobs`: user-visible sync lifecycle/progress.
- `sync_generations`: snapshot build/visibility state, base generation, and
  monotonically increasing sequence.
- `embedding_cache`: org/model/content-hash keyed cached vectors.
- `root_acls`: path-prefix deny entries.
- `content_proofs`: per-user Merkle proof for user-owned root filtering.
- `subscriptions`: Stripe-derived billing state.

The shared Go model layer defines roots, index namespaces, file states, file
change statuses, chunks, sync requests/responses, query requests/responses,
ACLs, API keys, org members, and sync jobs.

## Storage Layout

Object storage carries the high-volume data plane:

- `syncs/<generationID>/sources/files/.capture-<captureID>/<path>`:
  generation-scoped standalone source captures for large files. Each
  request gets a unique capture ID so a retried request cannot overwrite bytes
  accepted from another attempt. Legacy generation-scoped keys without a
  capture ID are still accepted when finalizing older clients.
- `syncs/<generationID>/sources/bundles/<bundleID>`: generation-scoped packed
  small-file source transport bundles.
- `files/<rootID>/<path>` and `bundles/<rootID>/<bundleID>`: legacy
  root-scoped source transport accepted for older clients.
- `states/<rootID>/<generationID>.json.gz`: durable compressed root state
  snapshots uploaded by current clients or written from inline state.
- `syncs/<generationID>/manifests/*.jsonl`: manifest shards uploaded by the
  client during the manifest-session flow.
- `syncs/<generationID>/proofs/content-proof.json`: generation-scoped content
  proof artifact.
- `syncs/<generationID>/state/state.json.gz`: legacy generation-scoped state,
  copied to the durable state key before processing.
- `syncs/<generationID>/request.json`: queued sync request payload.
- `syncs/<generationID>/inputs/*.jsonl`: file-change shards (derived from
  manifests or inline changes).
- `syncs/<generationID>/chunks/*.jsonl.gz`: compressed chunk-stage artifacts.
- `chunks/<rootID>/...`: rendered document page images and indexed image
  artifacts.

Root deletion first marks the root as deleting and fails its active sync jobs.
It then removes root file objects, bundles, states, chunk artifacts, sync
artifacts for known generations, and all Turbopuffer namespaces. A late queue
completion repeats the whole cleanup after its work stops.

## Sync Architecture

### Client-Side Sync

The CLI first discovers regular files and their metadata while honoring
built-in ignores, `.gitignore`, `.tpfsignore`, and `~/.tpfs/.tpfsignore`.
Metadata can prove that a file still matches the committed local cache, but is
never treated as the identity of uncached content. Files that need capture are
read or streamed once; the SHA-256, size, source ranges, final flat state,
Merkle tree, and content proof are then derived from the bytes that
were actually accepted for that generation.

This is a captured-version sync, not an instantaneous filesystem snapshot. A
large regular file is streamed from an open descriptor through a fixed-length
section, so appends after capture starts are excluded without copying the tree
to a staging directory. Small files are held in the bounded bundle buffer. The
CLI checks descriptor and path identity, size, and modification time after the
read. If a file changed while its captured bytes remained valid, that exact
captured version can commit and the path is marked dirty for a follow-up sync.
If a complete version cannot be captured (for example, the opened file is
truncated before its fixed extent can be read), the path is deferred and its
previously committed version remains visible. Replacing the path does not
invalidate bytes already available through the open descriptor, but does mark
the path dirty. This applies to all regular files, independent of file type.

The model and CLI use added, removed, modified, moved, renamed, and unchanged
file statuses.
Move/rename detection matches removed and added files by content hash; large
moved files can be treated more conservatively via
`PUFFERFS_MOVE_REUSE_MAX_BYTES`.

Likely secret filenames are excluded by the ignore matcher before state is
created. For included files, the CLI uploads source content that requires
capture; a final byte-hash diff discards speculative captures whose content is
unchanged:

- Small non-empty files are concatenated into generation-scoped bundle objects up to
  `PUFFERFS_UPLOAD_BUNDLE_MAX_BYTES`.
- Files over `PUFFERFS_UPLOAD_BUNDLE_SMALL_FILE_BYTES` are uploaded as
  generation-scoped standalone objects. At 64 MiB by default, the CLI switches
  from the API proxy to direct S3-compatible multipart upload. Empty files need
  no source upload.
- Standalone source uploads, completed source bundles, and independent sync metadata uploads use a bounded
  worker pool controlled by `PUFFERFS_UPLOAD_CONCURRENCY` (default 4, max 16).
  Bundle construction stays serial. Each bundle is capped at 128 files and
  15 MiB, source references are grouped by bundle, and the server preserves
  bundle boundaries when forming work shards. A worker therefore downloads a
  packed source object once. Completed bundle requests can overlap one another
  and standalone files. A direct multipart source uses up to four concurrent
  part lanes; aggregate direct part traffic remains capped at 16 lanes.
- Replayable upload requests are retried up to three times for transport
  failures, `408`, `429`, and `5xx` responses. Buffered bundle retries replay
  the same bytes. Proxied standalone retries reopen the HTTP request over the
  same fixed descriptor extent, while each server attempt writes a unique
  object. Direct multipart parts are read once into bounded immutable payloads
  and retried independently, so the SHA-256 always describes the exact bytes
  uploaded even if the local file changes during capture.
- Each file change carries `source_key`, `source_offset`, `source_length`, and,
  for sufficiently large local text/code, contiguous line-aware
  `source_ranges` so workers can read exact regions independently.
- The complete root state is gzip-compressed and streamed to its durable
  generation state object as a `state_ref`.
- Paths observed changing during capture are persisted in the local root cache
  and forcibly recaptured on the next sync. Follow mode schedules these
  reconciliation passes at least 30 seconds apart, even if a writer never
  becomes quiet.

Upload handlers stream request bodies into bounded-memory S3 multipart uploads
instead of first copying the complete body to local disk. Each request uses at
most two concurrent object-storage part uploads with bounded read-ahead,
partial multipart uploads are explicitly aborted with a cleanup context that
survives client disconnection, and abort failures are returned rather than
silently leaving unknown cleanup state. Per-read idle deadlines stop stalled
clients without imposing a total-duration limit on an active or
storage-backpressured upload. Direct uploads bypass the API body path; the
server creates the multipart session, signs each bounded part, validates the
completed object size, and aborts failed sessions. Bucket lifecycle cleanup
also aborts incomplete sessions older than one day.

For large trees, the CLI uses the manifest-session flow:

1. Call `POST /roots/{id}/sync/init` to create a sync generation and obtain a
   `generation_id` and `manifest_prefix`.
2. Capture and upload candidate source bytes. Build the final state, Merkle
   tree, and content proof from the successful captures.
3. Upload file-change manifest shards (JSONL) under the generation's artifact
   namespace via `POST /roots/{id}/sync/{generation_id}/upload`.
4. Upload the content proof and compressed state via the same artifact endpoint.
5. Submit a small finalize request (`POST /roots/{id}/sync`) with `generation_id`
   and `change_refs` pointing to the uploaded shards — no inline `changes` needed.

If client upload fails before finalize, `DELETE /roots/{id}/sync/{generation_id}`
aborts the session and deletes the generation's temporary transport/artifact
prefix. Rejected, failed, expired, and successfully committed syncs also delete
temporary transport objects for the generation. Durable committed state remains
under `states/<rootID>/...`, and rendered/indexed media remains under
`chunks/<rootID>/...`.

For backward compatibility the sync request still accepts inline changes without
a prior `sync/init` call. If the server reports a stale base generation, the CLI
reloads remote state, rediscovers and recaptures local candidates, recomputes
the final state and diff, and retries once against the latest generation.

For subset sync (`pufferfs sync --root <path> --include <glob> [--exclude <glob>]`),
the CLI matches root-relative glob patterns, treats repeated includes as OR, and
lets excludes subtract from that set. It merges selected changes into the current
committed root state and uploads only selected file bytes. The server still
receives and commits a complete root state, so unselected files remain visible
in the new generation.

### Server-Side Sync

Every sync creates a `sync_job` and a building `sync_generation`. The visible
snapshot does not change until the generation commits.

There are two execution modes:

- Without a queue backend, the server runs the same bounded shards directly in-process.
- With SQS or NATS configured, the server writes request/shard artifacts and
  enqueues chunk jobs; dedicated workers advance chunk, index, and commit
  stages.

Queued data shards use independent FIFO message groups so workers can process
them concurrently; commits remain ordered per root. SQS sends are bounded by
both its 10-message limit and its 1 MiB aggregate batch limit.

The pipeline shape is:

1. Prepare input shards from non-unchanged file changes, bounded by 128 records,
   estimated downstream chunk work, and packed-source bundle boundaries. A
   large local text/code source is expanded into contiguous line-aware ranges;
   each range gets a dedicated shard and can occupy a different worker lane.
2. Chunk stage:
   - Added/modified code, text, and markdown can be chunked locally in Go.
   - PDFs, Office docs, and images go to Modal.
   - Large text sources are read by byte range with global line and chunk
     coordinates; chunk artifacts stream through storage without whole-file
     materialization.
   - Modified/removed/moved paths emit close operations for active prior rows.
   - Moves/renames query active old rows and copy row metadata/vector into new
     generation rows when safe.
3. Index stage:
   - Rows are routed by stable hash of `file_path` to an active root namespace
     shard.
   - In production, Modal first downloads the compressed chunk artifact with
     bounded retries, then embeds missing vectors in FP16 batches of at most 64
     and writes Turbopuffer batches bounded by 512 rows and 8 MiB. Vectors are
     never persisted as a second S3 artifact.
   - The in-process fallback performs the same chunk-to-index transformation in
     Go. Vector-disabled roots use this path without calling Modal.
   - Close paths are grouped by namespace and patched together with
     `valid_to_generation` and `valid_to_generation_seq`.
4. Commit:
   - Finish any pending cleanup for earlier failed generations so their row
     closures cannot affect the new visibility window.
   - Store content proof when present.
   - Ensure root state is available by object ref.
   - Mark the new generation visible and complete the sync job.
5. Terminal cleanup:
   - Delete `syncs/<generationID>/` and any legacy root-scoped source transport
     refs known from the finalized request.
   - Batch object deletes at the S3 1,000-key request limit.
   - Preserve durable root state and OCR/page images referenced by indexed
     chunks.

### Generation Visibility

Index rows include:

- `generation_id`
- `valid_from_generation`
- `valid_from_generation_seq`
- `valid_to_generation`
- `valid_to_generation_seq`

Queries always add a visibility-window filter based on the root's
`visible_generation_seq`. If a root has no committed generation, the query path
fails closed by matching no uncommitted rows. This lets indexing write rows
before commit without exposing partial or failed syncs.

## Query Architecture

`POST /query` requires `query` scope/read permissions, validates root access,
loads active Turbopuffer namespaces, adds an optional file glob filter, and adds
the visible-generation filter.

Supported modes:

- `fts`: BM25 over `content`.
- `vector`: query text embedded through Modal, then ANN over `vector`.
- `hybrid`: query embedding plus BM25 and ANN, merged with reciprocal rank
  fusion.

For sharded roots, the query is executed against all active namespace shards
concurrently and result sets are merged with reciprocal rank fusion. Results are
then filtered by denied ACL path prefixes. For user-scoped roots, non-admin
users are also filtered through stored content proofs so they only receive rows
whose file path/hash are in their proof.

## Indexing and Turbopuffer

Each root can have one or more physical Turbopuffer namespaces. The shard count
is set when roots are created by `PUFFERFS_TP_NAMESPACE_SHARDS` and capped at
256. Namespace names are short deterministic hashes of org/root IDs plus shard
index. File paths are assigned to shards by SHA-256 hash.

Rows include searchable content, path metadata, file/chunk hashes, file type,
root/generation metadata, optional absolute path, optional page number/image
path, and vector. Turbopuffer schema enables full-text search on `content` and
uses cosine distance for vectors.

## Modal Compute

The Modal app defines:

- `chunk_file_endpoint`: file to chunks.
- `embed_chunks_endpoint`: chunks to embeddings.
- `embed_query_endpoint`: query text to embedding.
- `index_shard_endpoint`: compressed chunk artifact to bounded embedding and
  Turbopuffer writes.

Go workers stream text chunking directly. For queued vector syncs, Modal owns
the bounded embed-and-index transformation; the Go index path remains for
vector-disabled roots and in-process fallback. Modal is otherwise kept at the
file-conversion and embedding boundaries where it provides the specialized
CPU/GPU runtime.

Chunking strategies:

- Code: line-based chunks with overlap.
- Markdown/plain text: heading/section-aware chunks with overlap.
- PDF: render pages with `frpdf-renderer`, extract native PDF text separately,
  and call vision OCR only for pages without native text.
- DOC/DOCX and PPT/PPTX: convert to PDF with LibreOffice, then use the PDF path.
- Images: upload image artifact and use Gemini vision/captioning when available,
  otherwise store a placeholder description.

Embeddings use a pinned `nomic-ai/nomic-embed-text-v1.5` revision through
SentenceTransformers in FP16 on CUDA, with `search_document:` prefixes for
document chunks and `search_query:` prefixes for query text. Bulk shard work
and latency-sensitive query embedding run in separate Modal container pools.
The Go server's embedding cache version is expected to match the Modal model
and can be overridden with
`PUFFERFS_EMBEDDING_MODEL_VERSION`. The server preserves line metadata on index
rows, but strips `line_start` and `line_end` from the Modal embed payload so
older deployed embed containers that only know the base chunk schema remain
compatible.

## Authentication, Authorization, and Tenancy

Normal API authentication accepts:

- JWTs signed by `JWT_SECRET`, either in the Authorization header or an
  httpOnly `pf_session` cookie.
- API keys stored as SHA-256 hashes and resolved to org/user/role/scopes.

Login providers resolve through a shared identity-completion path. Google OAuth
and email one-time-code login both prove an email address, upsert a
`user_identities` row, accept any pending invite for that email, create or
resolve org membership, and then issue either a browser session cookie or a CLI
API key.

Authorization layers:

- API key scopes such as `sync`, `query`, `root:delete`, `api_keys:write`, and
  `*`.
- Org roles: owner, admin, editor, viewer.
- Root scopes:
  - `org`: visible to org members; write/delete require elevated roles.
  - `user`: visible to owner and org admins/owners.
- Groups and root grants: reusable org groups can receive root-level `read`,
  `sync`, `delete`, or `admin` permissions. Restricted roots are visible through
  these grants plus org admin override.
- Root ACLs: path-prefix entries currently behave as deny filters when
  `permission` is `none`.
- Admin routes use a separate platform key via `PUFFERFS_ADMIN_KEY` or
  `PUFFERFS_ADMIN_KEY_HASH`.

## Web App Functionality

The React app is an authenticated management console:

- `/login`: starts email-code login or Google OAuth through the API.
- `/_app` layout: requires a valid session cookie, shows navigation and logout.
- `/dashboard`: lists accessible roots.
- `/organization`: shows org name and members.
- `/billing`: optional, hidden and redirected unless `VITE_ENABLE_BILLING` is
  true; shows subscription state and starts Stripe checkout.
- `/auth/callback`: frontend landing page after backend OAuth cookie setup.

The web app talks to the Go API with `credentials: "include"` and depends on
server CORS/cookie domain configuration for cross-subdomain deployments.

## Billing

Billing is optional and only enabled when `ENABLE_BILLING=true` and Stripe
secret configuration is present. Supported behavior:

- Read current org subscription state.
- Create a Stripe subscription checkout session for org admins.
- Verify Stripe webhook signatures manually with HMAC-SHA256.
- Reconcile selected Stripe events into the `subscriptions` table.

## Deployment Architecture

The Docker image is a distroless static runtime containing the server, worker,
runtime selector, and migrations.

Pulumi defines:

- VPC, public/private subnets, internet gateway, NAT gateway, route tables.
- ECR repository and Docker image build/push.
- Private S3 artifact bucket.
- Static web S3 bucket with CloudFront and optional custom domain certificate.
- ECS cluster, API service behind an ALB, and worker services.
- NATS JetStream cluster support with ECS services, service discovery, EFS
  storage, and security groups.
- IAM roles/policies for ECS, S3, Secrets Manager, and EFS.
- CloudWatch logs.
- Optional ACM certificates and listeners for API/web custom domains.

The production deployment doc describes GitHub Actions gates, environment
variables/secrets, Pulumi stack configuration, frontend/installer publishing,
and CLI release publishing.

## Supported Functionality Summary

PufferFS currently supports:

- Multi-tenant org/user authentication with email-code login, Google OAuth, JWT
  sessions, and API keys.
- Scoped tenant API keys and platform admin provisioning APIs.
- Org roots and user-owned roots.
- Root create/list/get/delete.
- Path-prefix ACL deny filtering.
- Incremental filesystem sync from CLI.
- Merkle-based local diffing, move/rename detection, and conflict retry against
  remote generation changes.
- Built-in ignore rules plus server-managed org/user policies, `.gitignore`,
  `.tpfsignore`, and global `~/.tpfs/.tpfsignore`.
- Default exclusion of likely secret filenames before sync state is built.
- Small-file bundle uploads and direct multipart large-file uploads.
- Gzip root state storage by object reference.
- Async sync job tracking and status polling.
- Optional SQS/NATS-backed queue workers for chunk/index/commit.
- Direct in-process execution when no durable queue is configured.
- Local Go chunking for text/code/markdown-like files.
- Subset sync with `sync --root <path> --include <glob> [--exclude <glob>]`.
- Modal chunking for PDFs, Office docs, presentations, images, structured
  files, and media files.
- Modal embeddings for chunks and query text.
- Embedding cache keyed by org, model version, and content hash.
- Turbopuffer hybrid, vector, and full-text search.
- Namespace sharding per root with fan-out/fusion query merge.
- Generation-based snapshot visibility and failed-generation cleanup.
- Content-proof filtering for user-scoped roots.
- Continuous sync via filesystem watcher.
- Managed background sync services on macOS and Linux.
- Direct-install CLI upgrade checks and upgrades.
- Optional Stripe subscription state and checkout.
- Static web console for root/member/billing visibility.

## Important Boundaries and Caveats

- The web console is not a replacement for the CLI; it currently does not expose
  sync or query workflows.
- `handleSyncInit` creates the generation-scoped upload session used by the
  current CLI; it does not perform namespace cloning.
- With no external queue configured, the request executes the same bounded
  chunk/index stages directly. Production uses SQS or NATS JetStream.
- Query correctness relies on generation visibility filters. Any new query path
  must apply the same visible-generation window.
- The embedding cache version must be bumped when the Modal embedding model
  changes.
- Standalone Modal embedding calls receive only fields needed by the embedding
  model. The index-shard endpoint downloads complete-row artifacts before GPU
  work because it writes those rows directly to Turbopuffer.
- OAuth login uses signed random state bound to a short-lived httpOnly state
  cookie for CSRF protection.
- ACLs are modeled as entries but the implemented read/write checks primarily
  enforce `permission == "none"` as deny prefixes.
- Root deletion removes PufferFS copies and indexes, not source files on the
  user's machine.
