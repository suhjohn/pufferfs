# PufferFs

Hybrid search for your filesystem that agents can use. Sync a folder, then search it from your CLI.

## Architecture

- **CLI** (Go) — `pufferfs sync`, `pufferfs query`, `pufferfs watch`
- **Server** (Go) — API gateway for sync orchestration and query proxy
- **Compute** (Python/Modal) — file→chunks and chunks→embeddings on GPU
- **Search** (Turbopuffer) — hybrid BM25 + vector search, with one or more physical namespaces per synced root
- **Storage** (S3-compatible) — files, chunk images, and state

## Quick Start

### Prerequisites

- Go 1.23+
- Python 3.12+
- [Modal](https://modal.com) account
- [Turbopuffer](https://turbopuffer.com) API key
- S3-compatible storage (AWS S3, Cloudflare R2, etc.)
- PostgreSQL

### Install CLI

**macOS / Linux (installer script)**

```bash
curl -fsSL https://pufferfs.com/install.sh | sh
```

The installer auto-detects your OS and architecture (`darwin`/`linux`, `amd64`/`arm64`), downloads the latest release archive from `pufferfs.com/releases/`, verifies the SHA-256 checksum, and installs the binary to `/usr/local/bin`.

Pin a version or override the install directory:

```bash
curl -fsSL https://pufferfs.com/install.sh | PUFFERFS_VERSION=0.2.1 INSTALL_DIR="$HOME/.local/bin" sh
```

**From source (development)**

```bash
go install github.com/pufferfs/pufferfs/cmd/pufferfs@latest
```

### Configure

```bash
pufferfs init
# Edit ~/.tpfs/config.toml with your credentials
```

### Sync a directory

```bash
# Dry run first
pufferfs sync ./my-project --dry-run

# Sync
pufferfs sync ./my-project --name my-project

# Start a sync in the background, then check/wait on the job
pufferfs sync ./my-project --name my-project --background
pufferfs sync status --root my-project
pufferfs sync jobs --root my-project
pufferfs sync wait --root my-project --job-id <sync-job-id>

# Create a user-owned root instead of an org root
pufferfs sync ./my-project --name my-project --scope user
```

### Search

```bash
pufferfs query "how does authentication work"
pufferfs query "login flow" --mode fts
pufferfs query "database schema" --glob "*.sql"
```

### Delete a test root

```bash
pufferfs root delete my-project
# or skip the confirmation prompt
pufferfs root delete my-project --yes
```

This deletes PufferFS metadata, storage artifacts, and all Turbopuffer namespaces
for the synced root. It does not delete the original source files.

### Watch (continuous sync)

```bash
pufferfs watch ./my-project
```

For a supervised background sync, install a user service instead of running
`watch` with `&`:

```bash
pufferfs service install ./my-project --name my-project
pufferfs service start my-project
pufferfs service status my-project
pufferfs service logs my-project
pufferfs service restart my-project
pufferfs service stop my-project
pufferfs service uninstall my-project
```

`service install` writes a macOS `launchd` LaunchAgent or Linux `systemd --user`
unit that runs `pufferfs sync ./my-project --follow --name my-project`. The OS
supervisor restarts it on failure and captures logs.

### CLI upgrades and versioning

PufferFS CLI releases are SemVer git tags (`v0.3.0`, `v0.3.1`, ...). GoReleaser
builds macOS and Linux archives for `amd64` and `arm64` and publishes them to
GitHub Releases with `checksums.txt`.

```bash
git tag v0.3.0
git push origin v0.3.0
```

CLI installs upgrade with:

```bash
pufferfs upgrade
```

`pufferfs upgrade` reads the public release manifest at
`https://api.pufferfs.com/cli/version`, downloads the matching GoReleaser
archive, verifies its SHA-256 checksum, replaces the current binary, and
restarts installed user services. Pass `--manifest-url` to use a custom
manifest. The CLI also checks the configured server's manifest at most once per
day and prints an upgrade notice when a newer release is available. Set
`PUFFERFS_NO_UPDATE_CHECK=1` to disable the passive check.

The CLI SemVer and sync wire protocol are separate. CLI versions are injected at
release build time, while sync compatibility is controlled by
`models.SyncProtocolVersion`.

## Server

```bash
# Set environment variables
export DATABASE_URL="postgres://localhost:5432/pufferfs?sslmode=disable"
export JWT_SECRET="$(openssl rand -base64 32)"
export TURBOPUFFER_API_KEY="tbp_..."
export AWS_ACCESS_KEY_ID="..."
export AWS_SECRET_ACCESS_KEY="..."
export AWS_ENDPOINT_URL="https://..."
export AWS_BUCKET_NAME="pufferfs"
export MODAL_CHUNK_ENDPOINT="https://..."
export MODAL_EMBED_ENDPOINT="https://..."
export MODAL_QUERY_EMBED_ENDPOINT="https://..."
export MODAL_CHUNK_SHARD_ENDPOINT="https://..."
export MODAL_EMBED_SHARD_ENDPOINT="https://..."
export MODAL_INDEX_SHARD_ENDPOINT="https://..."

# Optional: advertise CLI release compatibility at GET /cli/version.
export PUFFERFS_CLI_LATEST_VERSION="0.3.0"
export PUFFERFS_CLI_MIN_VERSION="0.2.0"
export PUFFERFS_CLI_DOWNLOAD_BASE_URL="https://pufferfs.com/releases"

# Optional: enable platform provisioning/deletion APIs.
export PUFFERFS_ADMIN_KEY="..."

go run ./cmd/server
```

### Platform admin APIs

When `PUFFERFS_ADMIN_KEY` or `PUFFERFS_ADMIN_KEY_HASH` is configured, PufferFS
accepts that key on `/admin/*` routes. If neither value is configured, `/admin/*`
is blocked:

```bash
curl -X POST "$PUFFERFS_SERVER_URL/admin/orgs" \
  -H "Authorization: Bearer $PUFFERFS_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"Example Tenant","slug":"example-tenant","external_id":"tenant-example"}'

curl -X POST "$PUFFERFS_SERVER_URL/admin/users" \
  -H "Authorization: Bearer $PUFFERFS_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"email":"user@example.com","name":"User","external_id":"user-123"}'

curl -X PUT "$PUFFERFS_SERVER_URL/admin/orgs/$ORG_ID/members/$USER_ID" \
  -H "Authorization: Bearer $PUFFERFS_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"role":"viewer"}'

curl -X POST "$PUFFERFS_SERVER_URL/admin/orgs/$ORG_ID/users/$USER_ID/api-keys" \
  -H "Authorization: Bearer $PUFFERFS_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"member-query","scopes":["query"]}'

curl -X POST "$PUFFERFS_SERVER_URL/admin/orgs/$ORG_ID/roots" \
  -H "Authorization: Bearer $PUFFERFS_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"shared","source_path":"/shared","scope":"org"}'

curl -X POST "$PUFFERFS_SERVER_URL/admin/orgs/$ORG_ID/roots" \
  -H "Authorization: Bearer $PUFFERFS_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"private\",\"source_path\":\"/private\",\"scope\":\"user\",\"owner_user_id\":\"$USER_ID\"}"

curl -X DELETE "$PUFFERFS_SERVER_URL/admin/roots/$ROOT_ID" \
  -H "Authorization: Bearer $PUFFERFS_ADMIN_KEY"

curl -X DELETE "$PUFFERFS_SERVER_URL/admin/orgs/$ORG_ID" \
  -H "Authorization: Bearer $PUFFERFS_ADMIN_KEY"

curl -X DELETE "$PUFFERFS_SERVER_URL/admin/users/$USER_ID" \
  -H "Authorization: Bearer $PUFFERFS_ADMIN_KEY"
```

The admin key is a platform control-plane key. Do not pass it to tenant clients.
Provision member API keys instead; a `query`-only key can search allowed roots
but cannot create, sync, upload, or delete roots.

Roots can be `scope:"org"` or `scope:"user"`. Org roots are visible to org
members. User roots are visible to their owner and org `admin`/`owner` members.
Org admins can view and delete user roots; regular members cannot see other
members' user roots.

Admin root/org deletion removes PufferFS metadata, storage artifacts, and
Turbopuffer namespaces. Admin user deletion also removes that user's owned
roots. These delete APIs do not delete original source files.

Each root is a logical sync and access-control unit. Its index can be split
across multiple short Turbopuffer namespaces; set `PUFFERFS_TP_NAMESPACE_SHARDS`
before creating roots to choose the shard count. The default is `1`. Indexing
routes each file by a stable hash of `file_path`, and queries fan out across the
root's namespaces before merging top results.

### Queue-backed sync workers

Set `NATS_URL` on the server to enqueue sync shards into NATS JetStream. The API
then only writes shard manifests and publishes job pointers; thin Go dispatchers
pull/ack jobs, invoke Modal shard compute endpoints, and coordinate commit:

```bash
nats-server -js
export NATS_URL="nats://127.0.0.1:4222"

go run ./cmd/server
go run ./cmd/worker --stage=chunk --concurrency=16
go run ./cmd/worker --stage=embed --concurrency=8
go run ./cmd/worker --stage=index --concurrency=16
go run ./cmd/worker --stage=commit --concurrency=2
go run ./cmd/worker --stage=cleanup --concurrency=4
```

The worker binary also reads `PUFFERFS_WORKER_STAGE` and
`PUFFERFS_WORKER_CONCURRENCY`, which is useful for hosted worker services:

```bash
export PUFFERFS_WORKER_STAGE="chunk"
export PUFFERFS_WORKER_CONCURRENCY="16"
go run ./cmd/worker
```

The Docker image builds both `/pufferfs-server` and `/pufferfs-worker`. Its
default command starts the server, and worker services can override the command
with `/pufferfs-worker --stage=chunk --concurrency=16`.

`go test ./internal/queue` starts an embedded JetStream server locally and
verifies enqueue, pull, ack, and delayed redelivery semantics.

For a clustered JetStream deployment, set `PUFFERFS_QUEUE_REPLICAS=3` on the API
and every worker before the queue streams are created. Without this value,
JetStream uses the server default replica count.

Cleanup jobs are enabled by default and batch-delete transient sync/raw-source
transport artifacts after they are no longer needed. Set
`PUFFERFS_CLEANUP_SYNC_ARTIFACTS=false` to disable them. OCR page images are
preserved because indexed chunks keep their exact `image_path` object keys.

Queued syncs enqueue chunk shards in bounded waves. The default maximum is 32
in-flight shards per root; override it with `PUFFERFS_SYNC_MAX_IN_FLIGHT_SHARDS`
when tuning large syncs.

Embedding-cache lookups are batched at 500 hashes with up to 4 concurrent
queries by default. Tune with `PUFFERFS_EMBEDDING_CACHE_QUERY_BATCH_SIZE` and
`PUFFERFS_EMBEDDING_CACHE_QUERY_CONCURRENCY`.

## Modal Functions

Deploy the Modal functions:

```bash
cd modal
pip install -r requirements.txt
export PUFFERFS_MODAL_APP_NAME="pufferfs-dev"
export PUFFERFS_MODAL_S3_SECRET_NAME="pufferfs-s3-dev"
export PUFFERFS_MODAL_GEMINI_SECRET_NAME="pufferfs-gemini-dev"
export PUFFERFS_MODAL_TURBOPUFFER_SECRET_NAME="pufferfs-turbopuffer-dev"
modal deploy app.py
```

Unset those variables to use the defaults: `pufferfs`, `pufferfs-s3`,
`pufferfs-gemini`, and `pufferfs-turbopuffer`.

This creates these web endpoints:

- `chunk_file_endpoint` — file → chunks
- `embed_chunks_endpoint` — chunks → embeddings
- `embed_query_endpoint` — query text → embedding vector
- `chunk_shard_endpoint` — sync shard pointer → chunk artifact
- `embed_shard_endpoint` — chunk artifact → indexed rows artifact
- `index_shard_endpoint` — indexed rows artifact → Turbopuffer

## Deployment environments

Run development and production as separate environments, not just separate keys
pointing at shared state. Each environment should have its own Postgres
database, object storage bucket/prefix, Turbopuffer key or namespace set, Modal
app, Modal secrets, and NATS JetStream deployment if queued sync is enabled.

For production CI/CD, GitHub Environment setup, component deploys, and release
automation, see [Production Deployment](docs/production-deployment.md).

API services need:

```bash
DATABASE_URL="postgres://..."
JWT_SECRET="..."
AWS_ENDPOINT_URL="https://..."
AWS_BUCKET_NAME="pufferfs-dev"
AWS_ACCESS_KEY_ID="..."
AWS_SECRET_ACCESS_KEY="..."
TURBOPUFFER_API_KEY="tbp_..."
MODAL_CHUNK_ENDPOINT="https://..."
MODAL_EMBED_ENDPOINT="https://..."
MODAL_QUERY_EMBED_ENDPOINT="https://..."
```

Queue-backed API and worker services also need:

```bash
NATS_URL="nats://..."
MODAL_CHUNK_SHARD_ENDPOINT="https://..."
MODAL_EMBED_SHARD_ENDPOINT="https://..."
MODAL_INDEX_SHARD_ENDPOINT="https://..."
```

Tenant clients and sandboxes should only receive:

```bash
PUFFERFS_SERVER_URL="https://..."
PUFFERFS_API_KEY="pfs_sk_member_..."
```

## How It Works

1. **Sync**: CLI scans directory → computes diff against previous state → uploads changed files to S3 → server calls Modal to chunk and embed → routes rows by `file_path` hash → upserts to the root's Turbopuffer namespace shards
2. **Query**: CLI sends query to server → server embeds query via Modal → fans out hybrid search (BM25 + vector ANN) across the root's Turbopuffer namespace shards → reciprocal rank fusion → results returned to CLI
3. **Watch**: OS file watcher (fsnotify) → debounced incremental syncs

## File Type Support

| Type | Strategy |
|------|----------|
| Code (.py, .js, .ts, .go, etc.) | Line-based chunks (~300 lines, 50-line overlap) |
| Markdown / Text | Section/heading-based chunking |
| PDF | Page → image → text extraction |
| DOCX | Paragraph-based chunking |
| PPTX | Slide-based chunking |
| Images | LLM captioning (placeholder) |

## Project Structure

```
├── cmd/
│   ├── pufferfs/      # CLI
│   └── server/        # API server
├── internal/
│   ├── auth/          # API key middleware
│   ├── config/        # Configuration management
│   ├── diff/          # Filesystem diffing
│   ├── ignore/        # .gitignore-style exclusion
│   ├── server/        # Server handlers, Modal/TP clients
│   └── storage/       # S3-compatible storage
├── modal/             # Python Modal functions
│   ├── app.py         # Modal app definition
│   ├── chunkers.py    # File type-specific chunking
│   └── models.py      # Shared data models
└── pkg/
    └── models/        # Shared Go data types
```
