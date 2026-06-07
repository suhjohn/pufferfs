# PufferFS Architecture and Functionality

This document summarizes the current PufferFS codebase as implemented in this
repository. It focuses on the executable surfaces, core data flows, storage
layout, access model, and supported product functionality.

## Product Shape

PufferFS indexes a local filesystem root so users and agents can search it with
hybrid text/vector retrieval. The system is split into:

- A Go CLI for syncing, querying, watching, root management, service
  supervision, and direct-install upgrades.
- A Go API server that owns authentication, tenancy, root metadata, sync
  orchestration, query proxying, billing, and administrative provisioning.
- Optional Go worker processes that consume queued sync stages from NATS
  JetStream.
- Python Modal functions that chunk files, embed chunks and queries, and can
  execute full shard-level chunk/embed/index work.
- Turbopuffer namespaces for BM25/vector/hybrid search.
- S3-compatible object storage for uploaded source files, bundled transport
  files, root state, sync artifacts, page images, and queue artifacts.
- PostgreSQL for control-plane state plus small durable caches: orgs, users,
  API keys, roots, ACLs, sync jobs/generations, root state refs, embedding
  cache, content proofs, and subscriptions.
- A small React web app for login, dashboard, organization, and optional
  billing management.

## Repository Map

- `cmd/pufferfs`: CLI entrypoint and commands.
- `cmd/server`: HTTP API server wiring.
- `cmd/worker`: NATS-backed sync stage worker.
- `cmd/runtime`: container runtime switch that execs server or worker.
- `internal/server`: API handlers, DB access, sync pipeline, dispatchers,
  Turbopuffer client, Modal client, billing, cleanup, query helpers, local
  chunking, namespace routing.
- `internal/auth`: JWT, API key, admin key, OAuth, session cookie, and CORS
  middleware.
- `internal/config`: TOML/environment configuration loading.
- `internal/diff`, `internal/merkle`, `internal/ignore`: filesystem scanning,
  hashing, diffing, content proofs, and ignore handling.
- `internal/queue`: NATS JetStream queue abstraction.
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

- `sync [path]`: scan a directory, compute a diff, upload changed file content,
  submit a sync request, poll for async completion, and update local cache.
- `sync --dry-run`: show changes, total upload size, and ignored patterns
  without uploading.
- `sync --background` / `sync --detach`: submit the same server-side sync job
  but return immediately with a `sync_job_id`.
- `sync status`, `sync jobs`, and `sync wait`: inspect recent sync jobs, poll a
  specific job, or block until it reaches `completed`/`failed`.
- `sync --follow` and `watch`: run an initial sync, then use `fsnotify` to
  debounce filesystem changes and rerun sync.
- `query`: search a synced root with `fts`, `vector`, or `hybrid` mode, optional
  path glob, and `top-k` control.
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
Turbopuffer clients, optionally attaches a NATS queue, configures optional
Stripe billing, and exposes HTTP routes. Normal routes accept either JWT
session credentials or tenant API keys. `/admin/*` routes use a separate
platform admin key when configured.

Important API surfaces:

- Health and readiness: `GET /healthz`, `GET /readyz`, `GET /health`.
- CLI version manifest: `GET /cli/version`.
- OAuth/session: `GET /auth/google`, `GET /auth/callback`,
  `POST /auth/logout`, `GET /auth/me`.
- API keys: create, list, delete.
- Org management: get org, list/add/remove members.
- Platform admin: provision/delete orgs and users, upsert org membership,
  create member API keys, create/delete roots.
- Roots: create, list accessible roots, get metadata, delete, upload file,
  upload bundle, sync init (create sync session), sync artifact upload,
  sync abort, sync finalize, get state, sync status, list sync jobs.
- ACLs: create/list/delete path-prefix ACLs.
- Query: `POST /query`.
- Billing: get subscription, create Stripe checkout session, receive Stripe
  webhook when billing is enabled.

### Workers

`cmd/worker` runs one stage at a time: `chunk`, `embed`, `index`, `commit`, or
`cleanup`. It connects to the same database/storage/Modal/Turbopuffer
dependencies as the server plus NATS JetStream. Workers pull batches, process
jobs concurrently, heartbeat long jobs, retry with backoff, mark failed
generations/jobs after max attempts, and skip work for generations already
failed.

### Container Runtime

The Docker image builds `/pufferfs-server`, `/pufferfs-worker`, and
`/pufferfs-runtime`. The default command runs `/pufferfs-runtime`, which execs
the server unless `PUFFERFS_PROCESS=worker` or `PUFFERFS_WORKER_STAGE` is set.

## Core Data Model

The control plane is PostgreSQL:

- `organizations`, `users`, `org_members`, `api_keys`.
- `roots`: logical sync/access unit with org, scope, owner, source path,
  simhash, visible generation, and visible generation sequence.
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

- `files/<rootID>/<path>`: individually uploaded large or empty source files.
- `bundles/<rootID>/<bundleID>`: packed small-file transport bundles, bundle
  manifests, and CLI-uploaded gzip state refs.
- `states/<rootID>/<generationID>.json.gz`: compressed root state snapshots
  written by the server when a sync request carries inline state instead of a
  state ref.
- `syncs/<generationID>/manifests/*.jsonl`: manifest shards uploaded by the
  client during the manifest-session flow.
- `syncs/<generationID>/proofs/content-proof.json`: generation-scoped content
  proof artifact.
- `syncs/<generationID>/state/state.json.gz`: generation-scoped compressed state.
- `syncs/<generationID>/request.json`: queued sync request payload.
- `syncs/<generationID>/inputs/*.jsonl`: file-change shards (derived from
  manifests or inline changes).
- `syncs/<generationID>/chunks/*.jsonl`: chunk-stage artifacts.
- `syncs/<generationID>/index_rows/*.jsonl`: embed-stage/index-row artifacts.
- `syncs/<generationID>/queues/*.queue.json`: in-process object-queue state.
- `syncs/<generationID>/done/*.done`: queued shard completion markers.
- `chunks/<rootID>/...`: rendered document page images and indexed image
  artifacts.

Root deletion removes root file objects, bundles, states, chunk artifacts, sync
artifacts for known generations, and active Turbopuffer namespaces.

## Sync Architecture

### Client-Side Sync

The CLI builds a Merkle tree over the target directory using SHA-256 leaf hashes
and deterministic directory hashes. It honors built-in ignores, `.gitignore`,
`.tpfsignore`, and `~/.tpfs/.tpfsignore`. It skips matching Merkle subtrees and
falls back to flat state diffing when local tree cache is unavailable.

The model defines statuses for added, removed, modified, moved, renamed,
copied, moved-and-modified, and unchanged files. The current CLI diff paths
actively produce added, removed, modified, moved, renamed, and unchanged.
Move/rename detection matches removed and added files by content hash; large
moved files can be treated more conservatively via
`PUFFERFS_MOVE_REUSE_MAX_BYTES`.

Likely secret filenames are excluded by the ignore matcher before state is
created. For included files, the CLI uploads changed source content to the
server:

- Small non-empty files are concatenated into bundle objects up to
  `PUFFERFS_UPLOAD_BUNDLE_MAX_BYTES`.
- Files over `PUFFERFS_UPLOAD_BUNDLE_SMALL_FILE_BYTES` and empty files are
  uploaded as standalone objects.
- Each file change carries `source_key`, `source_offset`, and `source_length`
  so server/Modal can read exact bytes.
- The complete root state is gzip-compressed and uploaded through the bundle
  endpoint as a `state_ref`.

For large trees, the CLI uses the manifest-session flow:

1. Call `POST /roots/{id}/sync/init` to create a sync generation and obtain a
   `generation_id` and `manifest_prefix`.
2. Upload file-change manifest shards (JSONL) under the generation's artifact
   namespace via `POST /roots/{id}/sync/{generation_id}/upload`.
3. Upload the content proof and compressed state via the same artifact endpoint.
4. Submit a small finalize request (`POST /roots/{id}/sync`) with `generation_id`
   and `change_refs` pointing to the uploaded shards — no inline `changes` needed.

If client upload fails before finalize, `DELETE /roots/{id}/sync/{generation_id}`
aborts the session and cleans up artifacts.

For backward compatibility the sync request still accepts inline changes without
a prior `sync/init` call. If the server reports a stale base generation, the CLI
reloads remote state, recomputes the diff, and retries once against the latest
generation.

### Server-Side Sync

Every sync creates a `sync_job` and a building `sync_generation`. The visible
snapshot does not change until the generation commits.

There are two execution modes:

- Without NATS, the server runs the object-storage queue pipeline in-process.
- With NATS configured, the server writes request/shard artifacts and enqueues
  chunk jobs; dedicated workers advance chunk, embed, index, commit, and cleanup
  stages.

The pipeline shape is:

1. Prepare input shards from non-unchanged file changes.
2. Chunk stage:
   - Added/modified code, text, and markdown can be chunked locally in Go.
   - PDFs, Office docs, and images go to Modal.
   - Modified/removed/moved paths emit close operations for active prior rows.
   - Moves/renames query active old rows and copy row metadata/vector into new
     generation rows when safe.
3. Embed stage:
   - Close operations pass through.
   - Existing rows with vectors are reused.
   - Missing vectors are resolved through the Postgres embedding cache or Modal
     embedding endpoint.
   - New rows are written as index-row artifacts.
4. Index stage:
   - Rows are routed by stable hash of `file_path` to an active root namespace
     shard.
   - Rows are upserted to Turbopuffer in batches.
   - Close operations patch active rows with `valid_to_generation` and
     `valid_to_generation_seq`.
5. Commit:
   - Store content proof when present.
   - Ensure root state is available by object ref.
   - Clean rows from failed generations for the root.
   - Mark the new generation visible and complete the sync job.
6. Cleanup:
   - Queue-backed syncs can delete transient source/sync artifacts while
     preserving OCR/page images referenced by indexed chunks.

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
- `chunk_shard_endpoint`: sync shard pointer to chunk artifact.
- `embed_shard_endpoint`: chunk artifact to index-row artifact.
- `index_shard_endpoint`: index-row artifact to Turbopuffer writes/patches.

Chunking strategies:

- Code: line-based chunks with overlap.
- Markdown/plain text: heading/section-aware chunks with overlap.
- PDF: render pages with PyMuPDF, use native text when quality checks pass,
  otherwise call Gemini vision.
- DOC/DOCX and PPT/PPTX: convert to PDF with LibreOffice, then use the PDF path.
- Images: upload image artifact and use Gemini vision/captioning when available,
  otherwise store a placeholder description.

Embeddings use `nomic-ai/nomic-embed-text-v1.5` through SentenceTransformers,
with `search_document:` prefixes for document chunks and `search_query:`
prefixes for query text. The Go server's embedding cache version is expected to
match the Modal model and can be overridden with
`PUFFERFS_EMBEDDING_MODEL_VERSION`.

## Authentication, Authorization, and Tenancy

Normal API authentication accepts:

- JWTs signed by `JWT_SECRET`, either in the Authorization header or an
  httpOnly `pf_session` cookie.
- API keys stored as SHA-256 hashes and resolved to org/user/role/scopes.

Google OAuth is optional. When enabled, the server redirects to Google, upserts
the user, creates or resolves org membership, signs a JWT, and either returns it
as JSON for legacy clients or sets an httpOnly browser session cookie and
redirects to the frontend.

Authorization layers:

- API key scopes such as `sync`, `query`, `root:delete`, `api_keys:write`, and
  `*`.
- Org roles: owner, admin, editor, viewer.
- Root scopes:
  - `org`: visible to org members; write/delete require elevated roles.
  - `user`: visible to owner and org admins/owners.
- Root ACLs: path-prefix entries currently behave as deny filters when
  `permission` is `none`.
- Admin routes use a separate platform key via `PUFFERFS_ADMIN_KEY` or
  `PUFFERFS_ADMIN_KEY_HASH`.

## Web App Functionality

The React app is an authenticated management console:

- `/login`: starts Google OAuth through the API.
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

- Multi-tenant org/user authentication with Google OAuth, JWT sessions, and API
  keys.
- Scoped tenant API keys and platform admin provisioning APIs.
- Org roots and user-owned roots.
- Root create/list/get/delete.
- Path-prefix ACL deny filtering.
- Incremental filesystem sync from CLI.
- Merkle-based local diffing, move/rename detection, and conflict retry against
  remote generation changes.
- Built-in ignore rules plus `.gitignore`, `.tpfsignore`, and global
  `~/.tpfs/.tpfsignore`.
- Default exclusion of likely secret filenames before sync state is built.
- Small-file bundle uploads and standalone large-file uploads.
- Gzip root state storage by object reference.
- Async sync job tracking and status polling.
- Optional NATS-backed queue workers for chunk/embed/index/commit/cleanup.
- In-process object-storage queue fallback.
- Local Go chunking for text/code/markdown-like files.
- Modal chunking for PDFs, Office docs, presentations, and images.
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
- `handleSyncInit` is retained only for old clients and does not perform
  namespace cloning.
- The in-process object queue is implemented via S3 queue-state JSON with CAS
  semantics; production queued sync is expected to use NATS JetStream.
- Query correctness relies on generation visibility filters. Any new query path
  must apply the same visible-generation window.
- The embedding cache version must be bumped when the Modal embedding model
  changes.
- OAuth login currently uses a fixed state value, with a code comment noting
  production should use random state stored in a cookie for CSRF protection.
- ACLs are modeled as entries but the implemented read/write checks primarily
  enforce `permission == "none"` as deny prefixes.
- Root deletion removes PufferFS copies and indexes, not source files on the
  user's machine.
