# Configuration Reference

This is the single source of truth for PufferFS configuration: CLI config file
keys, and every environment variable read by the CLI, API server, and workers.
Values are taken from the code (`internal/config`, `internal/server`,
`internal/auth`, `cmd/*`); defaults shown are the in-code defaults.

There are three configuration surfaces:

1. **CLI config file** — `~/.tpfs/config.toml`, for end users.
2. **CLI environment variables** — override the config file and tune client
   behavior.
3. **Server / worker environment variables** — for operators self-hosting the
   API server and queue workers.

---

## 1. CLI config file (`~/.tpfs/config.toml`)

Created by `pufferfs init`. The file is written with `0600` permissions in a
`0700` directory.

```toml
[server]
url     = "https://api.example.com"
api_key = "pfs_sk_..."

[turbopuffer]
api_key = ""          # only needed for direct-to-Turbopuffer setups
region  = ""

[storage]
endpoint_url      = ""   # S3-compatible endpoint (e.g. R2)
bucket            = ""
access_key_id     = ""
secret_access_key = ""
```

| Key | Meaning |
| --- | --- |
| `server.url` | PufferFS API server base URL. |
| `server.api_key` | Tenant API key (`pfs_sk_...`). |
| `turbopuffer.api_key` / `region` | Turbopuffer credentials (advanced/direct setups). |
| `storage.*` | S3-compatible storage endpoint and credentials. |

Per-root local cache (root metadata, flat file state, Merkle snapshot) lives
under `~/.tpfs/roots/<root-id>/`. Global ignore rules live at
`~/.tpfs/.tpfsignore` (gitignore syntax, applies to all projects for the current
user). Project-level ignore rules use `.tpfsignore` files placed anywhere in the
synced tree.
See [developer-guide.md § What Gets Synced](./developer-guide.md#what-gets-synced)
for full ignore-rule documentation.

---

## 2. CLI environment variables

Environment variables override `config.toml`. Empty values are ignored (the
config-file value is kept).

| Variable | Overrides / controls | Default |
| --- | --- | --- |
| `PUFFERFS_SERVER_URL` | `server.url` | — |
| `PUFFERFS_API_KEY` | `server.api_key` | — |
| `TURBOPUFFER_API_KEY` | `turbopuffer.api_key` | — |
| `AWS_ENDPOINT_URL` | `storage.endpoint_url` | — |
| `AWS_BUCKET_NAME` | `storage.bucket` | — |
| `AWS_ACCESS_KEY_ID` | `storage.access_key_id` | — |
| `AWS_SECRET_ACCESS_KEY` | `storage.secret_access_key` | — |
| `PUFFERFS_NO_UPDATE_CHECK` | Disable the once-per-day CLI upgrade check when set. | unset |

### CLI sync tuning

These control how the CLI packs uploads and handles moves. Byte values are
plain integers (bytes).

| Variable | Meaning | Default |
| --- | --- | --- |
| `PUFFERFS_UPLOAD_BUNDLE_SMALL_FILE_BYTES` | Files at or below this size are packed into bundles; larger files upload standalone. | 8 MiB (`8<<20`) |
| `PUFFERFS_UPLOAD_BUNDLE_MAX_BYTES` | Max size of a single packed bundle object. | 256 MiB (`256<<20`) |
| `PUFFERFS_MOVE_REUSE_MAX_BYTES` | Max file size for which moved-file index reuse is attempted; larger moves are handled conservatively. | 64 MiB (`64<<20`) |
| `PUFFERFS_SYNC_POLL_TIMEOUT` | How long the CLI polls an async sync job before giving up. Go duration. | 35m |

> Server enforced upload caps are separate: 512 MiB per single file, 1024 MiB
> per bundle (see [api-reference.md](./api-reference.md#limits)).

### `watch` / `sync --follow` flags

These are command flags, not env vars, but belong with sync tuning:

| Flag | Meaning |
| --- | --- |
| `--debounce` | Quiet period after file events before syncing. |
| `--max-backoff` | Maximum retry backoff on transient failures. |
| `--max-same-failures` | Exit after this many consecutive identical failures. |

---

## 3. Server and worker environment variables

The API server and workers are configured entirely through environment
variables. Group by concern below.

### Core / networking

| Variable | Meaning | Default / notes |
| --- | --- | --- |
| `DATABASE_URL` | PostgreSQL connection string. | **Required.** |
| `PORT` | HTTP listen port. | server default |
| `LISTEN_ADDR` | Full listen address (overrides `PORT` when set). | — |
| `MIGRATIONS_DIR` | Path to SQL migrations applied on boot. | bundled |
| `FRONTEND_URL` | Web app origin; OAuth redirects land here. | — |
| `COOKIE_DOMAIN` | Registrable domain for the `pf_session` cookie (e.g. `.example.com`) so api/app subdomains share it. | — |

CORS allowed origins are derived from configuration so the browser app can make
credentialed requests; with no origins configured CORS is a no-op (correct for
API-key-only setups).

### Authentication

| Variable | Meaning | Default / notes |
| --- | --- | --- |
| `JWT_SECRET` | HMAC secret for signing/validating session JWTs. | **Required.** Session TTL is 24h. |
| `PUFFERFS_ADMIN_KEY` | Platform admin key (plaintext form). Hashed internally. | optional |
| `PUFFERFS_ADMIN_KEY_HASH` | SHA-256 hash of the admin key (preferred over plaintext). | optional |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | Google OAuth credentials. OAuth is enabled only when these are set. | optional |
| `OAUTH_REDIRECT_URL` | OAuth callback URL, e.g. `https://api.example.com/auth/callback`. | optional |

If neither admin key variable is set, all `/admin/*` routes return `403`.

### Storage (S3-compatible)

| Variable | Meaning |
| --- | --- |
| `AWS_ENDPOINT_URL` | S3-compatible endpoint (omit for AWS S3). |
| `AWS_BUCKET_NAME` | Bucket for source files, bundles, states, sync artifacts, page images. |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | Credentials. |
| `AWS_REGION` / `AWS_DEFAULT_REGION` | Region. |

### Search (Turbopuffer)

| Variable | Meaning | Default |
| --- | --- | --- |
| `TURBOPUFFER_API_KEY` | Turbopuffer API key. | required for search |
| `TURBOPUFFER_API_URL` | Turbopuffer base URL. | provider default |
| `PUFFERFS_TP_NAMESPACE_SHARDS` | Physical namespaces per root, set at root creation. | 1 (max 256) |
| `PUFFERFS_TP_WRITE_BATCH_ROWS` | Rows per Turbopuffer upsert/patch batch. | 512 (max 5000) |

### Modal compute

| Variable | Meaning |
| --- | --- |
| `MODAL_CHUNK_ENDPOINT` | File → chunks endpoint. |
| `MODAL_EMBED_ENDPOINT` | Chunks → embeddings endpoint. |
| `MODAL_QUERY_EMBED_ENDPOINT` | Query text → embedding endpoint. |
| `MODAL_CHUNK_SHARD_ENDPOINT` | Sync shard → chunk artifact (queued pipeline). |
| `MODAL_EMBED_SHARD_ENDPOINT` | Chunk artifact → index-row artifact (queued pipeline). |
| `MODAL_INDEX_SHARD_ENDPOINT` | Index-row artifact → Turbopuffer writes (queued pipeline). |
| `PUFFERFS_EMBEDDING_MODEL_VERSION` | Embedding cache version; must be bumped when the Modal embedding model changes. | — |

### Sync pipeline tuning

| Variable | Meaning | Default |
| --- | --- | --- |
| `PUFFERFS_SYNC_WORKERS` | In-process sync worker concurrency. | 64 (max 64) |
| `PUFFERFS_SYNC_JOB_TIMEOUT` | Max wall-clock per async sync job before it is marked failed. Go duration. | 30m |
| `PUFFERFS_SYNC_MAX_IN_FLIGHT_SHARDS` | Max concurrent in-flight shards in the queued pipeline. | 32 |
| `PUFFERFS_EMBED_BATCH_SIZE` | Chunks per Modal embed batch. | 16 |
| `PUFFERFS_EMBED_BATCH_CONCURRENCY` | Concurrent embed batches. | 4 |
| `PUFFERFS_EMBEDDING_CACHE_QUERY_BATCH_SIZE` | Embedding-cache lookup batch size. | 500 |
| `PUFFERFS_EMBEDDING_CACHE_QUERY_CONCURRENCY` | Concurrent embedding-cache lookups. | 4 |
| `PUFFERFS_CLEANUP_BATCH_SIZE` | Rows per cleanup batch. | 1000 |
| `PUFFERFS_CLEANUP_SYNC_ARTIFACTS` | Whether queued syncs delete transient sync artifacts after commit. | enabled when set |

### Queue (NATS JetStream)

The server uses the in-process object-storage queue unless a NATS queue is
attached. Workers always require NATS.

| Variable | Meaning | Default |
| --- | --- | --- |
| `NATS_URL` | JetStream URL. Enables the queued sync path when set. | unset (in-process) |
| `PUFFERFS_QUEUE_DEDUPE_WINDOW` | JetStream message dedupe window. Go duration. | 24h |
| `PUFFERFS_QUEUE_REPLICAS` | JetStream stream replicas. | 0 |

### Process / runtime selection

| Variable | Meaning |
| --- | --- |
| `PUFFERFS_PROCESS` | `/pufferfs-runtime` execs the worker instead of the server when set to `worker`. |
| `PUFFERFS_WORKER_STAGE` | Selects the worker stage: `chunk`, `embed`, `index`, `commit`, or `cleanup`. Setting it also implies worker mode. |

### Billing (Stripe)

| Variable | Meaning |
| --- | --- |
| `ENABLE_BILLING` | Must be `true` (with Stripe secrets) to enable billing routes; otherwise they 404. |
| `STRIPE_SECRET_KEY` | Stripe API key. |
| `STRIPE_PRICE_ID` | Subscription price ID for checkout. |
| `STRIPE_WEBHOOK_SECRET` | Webhook signing secret (HMAC-SHA256 verification). |

### CLI release manifest (served by `GET /cli/version`)

| Variable | Meaning |
| --- | --- |
| `PUFFERFS_CLI_LATEST_VERSION` | Latest advertised CLI version. |
| `PUFFERFS_CLI_MIN_VERSION` | Minimum supported CLI version. |
| `PUFFERFS_CLI_DOWNLOAD_BASE_URL` | Base URL for release archives. |
| `PUFFERFS_CLI_SHA256_<PLATFORM>` | Per-platform archive checksum, e.g. `PUFFERFS_CLI_SHA256_DARWIN_ARM64`. |

### Web app build-time variables

These are baked into the static web build (Vite), not read at runtime by Go:

| Variable | Meaning |
| --- | --- |
| `VITE_API_URL` | API base URL the web console calls. |
| `VITE_ENABLE_BILLING` | Show the billing route in the web console. |

---

## Notes and gotchas

- **Embedding cache version must be bumped** (`PUFFERFS_EMBEDDING_MODEL_VERSION`)
  whenever the Modal embedding model changes, or cached vectors will be
  inconsistent with new ones.
- **NATS toggles execution mode.** With `NATS_URL` set, sync runs through queued
  workers; without it, the server runs the same pipeline in-process. Workers are
  only meaningful in the NATS configuration.
- **Admin key**: prefer `PUFFERFS_ADMIN_KEY_HASH` over `PUFFERFS_ADMIN_KEY` so
  the plaintext key is never present in the environment.
- For the production AWS/Pulumi deployment and which of these belong in Secrets
  vs. plain env, see [production-deployment.md](./production-deployment.md).
