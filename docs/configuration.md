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
url     = "https://api.pufferfs.com"
api_key = "pfs_sk_..."
```

Advanced direct/self-hosted setups may also provide Turbopuffer and
S3-compatible storage settings:

```toml
[turbopuffer]
api_key = ""
region  = "gcp-us-central1"

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
local user). Server-managed org/user ignore policy is configured through
`pufferfs ignore` or the `/ignore-policy` API and is enforced by the server.
Project-level ignore rules use `.tpfsignore` files placed anywhere in the synced
tree.
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

### `sync --follow` flags

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
| `ENABLE_EMAIL_LOGIN` | Enables email one-time-code login endpoints. Set to `false` to disable. | enabled |
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

| Variable | Meaning | Default |
| --- | --- | --- |
| `MODAL_CHUNK_ENDPOINT` | File → chunks endpoint. | — |
| `MODAL_EMBED_ENDPOINT` | Chunks → embeddings endpoint. | — |
| `MODAL_QUERY_EMBED_ENDPOINT` | Query text → embedding endpoint. | — |
| `MODAL_CHUNK_SHARD_ENDPOINT` | Sync shard → chunk artifact (queued pipeline). | — |
| `MODAL_EMBED_SHARD_ENDPOINT` | Chunk artifact → index-row artifact (queued pipeline). | — |
| `MODAL_INDEX_SHARD_ENDPOINT` | Index-row artifact → Turbopuffer writes (queued pipeline). | — |
| `MODAL_OFFICE_TO_PDF_ENDPOINT` | Optional public Office → PDF conversion endpoint for direct callers. Not used by the API server. | — |
| `MODAL_PDF_TO_PAGE_IMAGES_ENDPOINT` | Optional public PDF → page JPEG endpoint for direct callers. Not used by the API server. | — |
| `PUFFERFS_MODAL_PAGE_IMAGE_DPI` | DPI used when rendering PDF/Office pages to JPEG images for OCR and previews. Lower values reduce S3 upload size and vision-token payloads at some OCR-detail cost. | 160 |
| `PUFFERFS_MODAL_PAGE_IMAGE_JPEG_QUALITY` | JPEG quality for rendered page images. Lower values reduce upload size; valid values are clamped between 30 and 95. | 75 |
| `PUFFERFS_MODAL_PAGE_IMAGE_UPLOAD_CONCURRENCY` | Concurrent S3 page-image uploads per document chunking container. This overlaps with OCR fan-out and is separate from Modal OCR container concurrency. | 512 |
| `PUFFERFS_PDF_RENDERER_INSTALL_URL` | Installer script URL for the `frpdf` binary installed into the Modal chunking image. | `https://raw.githubusercontent.com/suhjohn/frpdf-renderer/main/install.sh` |
| `PUFFERFS_PDF_RENDERER_VERSION` | `frpdf-renderer` release version passed to the installer. Set to `latest` to install the latest release. | `v0.1.1` |
| `PUFFERFS_PDF_RENDERER_PATH` | Runtime path to the installed PDF renderer binary. | `/usr/local/bin/frpdf` |
| `PUFFERFS_PDF_RENDERER_JOBS` | Parallel render jobs passed to `frpdf-renderer --jobs`. Defaults to container CPU count. | CPU count |
| `PUFFERFS_MODAL_PAGE_TEXT_MIN_CONTAINERS` | Warm Modal containers kept for page image-to-text calls. | 4 |
| `PUFFERFS_MODAL_OCR_MAX_CONTAINERS` | Global max Modal containers for OCR/image-to-text calls. This caps OCR fan-out across all documents using the shared `page_image_to_text` function pool. | 100 |
| `PUFFERFS_MODAL_SECRET_NAME` | Single Modal secret name used by all Modal functions. It contains storage, Turbopuffer, and model-provider credentials. | `pufferfs` |
| `PUFFERFS_VLLM_MODELS` | Comma-separated provider-qualified vision model specs for image/page OCR. Entries use `provider/model` and may include weights as `provider/model:weight`; choose weights from usable capacity, for example `min(RPM, TPM / estimated_tokens_per_page)`. `gemini/...` uses the Gemini SDK; other providers use OpenAI-compatible chat completions when `<PROVIDER>_API_KEY` and a base URL are available. Fireworks defaults to `https://api.fireworks.ai/inference/v1`. If Gemini OCR fails, including recitation blocks, OCR falls back to `openai/gpt-5.4-nano`. | `fireworks/accounts/fireworks/models/qwen3p7-plus:5,openai/gpt-5.4-nano:40,openai/gpt-5.4-mini:15,gemini/gemini-3.1-flash-lite:10,gemini/gemini-2.5-flash-lite:30` |
| `PUFFERFS_EMBEDDING_MODEL_VERSION` | Embedding cache version; must be bumped when the Modal embedding model changes. | — |

The Modal secret named by `PUFFERFS_MODAL_SECRET_NAME` should contain:

| Secret variable | Required when |
| --- | --- |
| `AWS_ACCESS_KEY_ID` | Modal reads/writes S3-compatible storage. |
| `AWS_SECRET_ACCESS_KEY` | Modal reads/writes S3-compatible storage. |
| `AWS_ENDPOINT_URL` | Required for non-AWS S3-compatible storage; omit for AWS S3. |
| `AWS_BUCKET_NAME` | Modal reads/writes source files, chunk artifacts, and page images. |
| `TURBOPUFFER_API_KEY` | Modal index shard writes Turbopuffer rows. |
| `MODAL_SECRET_KEY` | Required to authorize direct calls to the public `office_to_pdf` and `pdf_to_page_images` Modal endpoints. |
| `GEMINI_API_KEY` | `PUFFERFS_VLLM_MODELS` includes `gemini/...`, or media OCR is enabled. |
| `OPENAI_API_KEY` | `PUFFERFS_VLLM_MODELS` includes `openai/...`. |
| `FIREWORKS_API_KEY` | `PUFFERFS_VLLM_MODELS` includes `fireworks/...`. |
| `<PROVIDER>_BASE_URL` | Optional for OpenAI-compatible providers; Fireworks defaults to `https://api.fireworks.ai/inference/v1`, OpenAI defaults to `https://api.openai.com/v1`. |

Standalone conversion endpoints accept JSON and are protected by
`MODAL_SECRET_KEY` in the request body:

```json
{
  "secret_key": "shared secret",
  "content_b64": "base64-encoded docx or pptx bytes",
  "file_type": "docx",
  "file_path": "slides.docx"
}
```

`office_to_pdf` responds with `pdf_b64`. `pdf_to_page_images` accepts
`pdf_b64` and responds with one entry per page containing `image_b64`,
`image_bytes`, and `fallback_text`. Page images are rendered by
`frpdf-renderer`; `fallback_text` is extracted separately from the PDF text
layer.

### Transactional email (AWS SES, optional)

Invites work without email: `POST /org/invites` stores a pending invite, and
the invited address accepts it on the next sign-in that proves the invited
email address. Email-code login requires transactional email. Set
`TRANSACTIONAL_EMAIL_FROM` to send login codes and invite notifications through
AWS SES.

| Variable | Meaning |
| --- | --- |
| `TRANSACTIONAL_EMAIL_FROM` | Verified SES sender email address. Enables login-code and invite email when set. |
| `TRANSACTIONAL_EMAIL_FROM_NAME` | Optional display name for the sender. |
| `TRANSACTIONAL_EMAIL_REPLY_TO` | Optional comma-separated reply-to addresses. |
| `TRANSACTIONAL_EMAIL_APP_URL` | Web app URL used for email links. Defaults to `FRONTEND_URL`. |
| `SES_REGION` | SES region. Defaults to `AWS_REGION`, then `AWS_DEFAULT_REGION`, then `us-east-1`. |
| `SES_CONFIGURATION_SET` | Optional SES configuration set. |
| `SES_FROM_IDENTITY_ARN` | Optional SES identity ARN for least-privilege/sending authorization. |
| `SES_FEEDBACK_EMAIL` | Optional bounce/complaint forwarding address. |
| `SES_FEEDBACK_IDENTITY_ARN` | Optional identity ARN for the feedback address. |
| `SES_ENDPOINT_URL` | Optional SES-compatible endpoint override, mainly for local testing. |

The older `INVITE_EMAIL_FROM`, `INVITE_EMAIL_FROM_NAME`,
`INVITE_EMAIL_REPLY_TO`, and `INVITE_EMAIL_APP_URL` variables remain accepted as
compatibility aliases, but new deployments should use the transactional names.

### Sync pipeline tuning

| Variable | Meaning | Default |
| --- | --- | --- |
| `PUFFERFS_SYNC_WORKERS` | In-process sync worker concurrency. | 64 (max 64) |
| `PUFFERFS_SYNC_JOB_TIMEOUT` | Max time without persisted job progress before an async sync job is marked failed. Go duration. | 30m |
| `PUFFERFS_SYNC_JOB_WATCHDOG_INTERVAL` | How often the cleanup worker reconciles stalled or inconsistent jobs. Go duration. | 1m |
| `PUFFERFS_SYNC_MAX_IN_FLIGHT_SHARDS` | Max concurrent in-flight shards in the queued pipeline. | 32 |
| `PUFFERFS_EMBED_BATCH_SIZE` | Chunks per Modal embed batch. | 16 |
| `PUFFERFS_EMBED_BATCH_CONCURRENCY` | Concurrent embed batches. | 4 |
| `PUFFERFS_EMBEDDING_CACHE_QUERY_BATCH_SIZE` | Embedding-cache lookup batch size. | 500 |
| `PUFFERFS_EMBEDDING_CACHE_QUERY_CONCURRENCY` | Concurrent embedding-cache lookups. | 4 |
| `PUFFERFS_CLEANUP_BATCH_SIZE` | Rows per cleanup batch. | 1000 |
| `PUFFERFS_CLEANUP_SYNC_ARTIFACTS` | Whether terminal syncs delete transient source/sync artifacts. Set `0`, `false`, `no`, or `off` to disable. | enabled |

### Queue (Amazon SQS FIFO or NATS JetStream)

The server uses the in-process object-storage path unless a durable backend is
selected. Workers require either SQS or NATS. Production uses one SQS FIFO queue
per stage so failures and capacity are isolated.

| Variable | Meaning | Default |
| --- | --- | --- |
| `PUFFERFS_QUEUE_BACKEND` | Durable queue backend: `sqs` or `nats`. | unset |
| `PUFFERFS_SQS_CHUNK_QUEUE_URL` | SQS FIFO URL for chunk jobs. Required for SQS. | unset |
| `PUFFERFS_SQS_EMBED_QUEUE_URL` | SQS FIFO URL for embed jobs. Required for SQS. | unset |
| `PUFFERFS_SQS_INDEX_QUEUE_URL` | SQS FIFO URL for index jobs. Required for SQS. | unset |
| `PUFFERFS_SQS_COMMIT_QUEUE_URL` | SQS FIFO URL for commit jobs. Required for SQS. | unset |
| `PUFFERFS_SQS_CLEANUP_QUEUE_URL` | SQS FIFO URL for cleanup jobs. Required for SQS. | unset |
| `NATS_URL` | JetStream URL. Selects NATS when the backend is unset. | unset (in-process) |
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

### Product analytics (PostHog)

| Variable | Meaning |
| --- | --- |
| `POSTHOG_ENABLED` | Set to `true` to emit backend product events. |
| `POSTHOG_KEY` | PostHog project token used by the backend capture API. This can match `VITE_POSTHOG_KEY` when web and backend events should land in one PostHog project. |
| `POSTHOG_HOST` | Optional PostHog ingestion host. Defaults to `https://us.i.posthog.com`. |

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
| `VITE_POSTHOG_KEY` | Optional browser-safe PostHog project token for web analytics and frontend product events. |
| `VITE_POSTHOG_HOST` | Optional PostHog ingestion host. Defaults to `https://us.i.posthog.com`. |

---

## Notes and gotchas

- **Local `.env` is for development and integration runs.** From the repo root,
  use `set -a; source .env; set +a` before commands that need Modal,
  Turbopuffer, AWS, or other service credentials. Never print or commit secret
  values; report variable names and presence only.
- **Embedding cache version must be bumped** (`PUFFERFS_EMBEDDING_MODEL_VERSION`)
  whenever the Modal embedding model changes, or cached vectors will be
  inconsistent with new ones.
- **The durable backend toggles execution mode.** With SQS selected or
  `NATS_URL` set, sync runs through queued workers; without either, the server
  runs the same pipeline in-process.
- **Admin key**: prefer `PUFFERFS_ADMIN_KEY_HASH` over `PUFFERFS_ADMIN_KEY` so
  the plaintext key is never present in the environment.
- For the production AWS/Pulumi deployment and which of these belong in Secrets
  vs. plain env, see [production-deployment.md](./production-deployment.md).
