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
   API server and workers.

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
session_token     = ""
```

| Key | Meaning |
| --- | --- |
| `server.url` | PufferFS API server base URL. |
| `server.api_key` | Tenant API key (`pfs_sk_...`). |
| `turbopuffer.api_key` / `region` | Turbopuffer credentials (advanced/direct setups). |
| `storage.*` | S3-compatible storage endpoint and credentials. |

Per-root local capture cache (identity, journals, immutable spool objects and heads) lives
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
| `AWS_SESSION_TOKEN` | `storage.session_token` | Required with explicit temporary STS credentials |
| `PUFFERFS_NO_UPDATE_CHECK` | Disable the once-per-day CLI upgrade check when set. | unset |

### CLI capture tuning

The CLI requires only server URL and tenant API key. Source uploads use
short-lived signed S3 URLs issued after authentication; end users never need
operator AWS credentials.

| Variable | Meaning | Default |
| --- | --- | --- |
| `PUFFERFS_CAPTURE_SPOOL_BYTES` | Per-server/root/source spool budget; minimum 8 MiB | 2 GiB |
| `PUFFERFS_SYNC_POLL_TIMEOUT` | Status/wait polling deadline | 35m |

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
| `PUFFERFS_WORKER_DB_MAX_CONNS` | Maximum PostgreSQL connections per Python worker process; 2–16, with no reserved minimum. Idle connections expire after one minute. | `2` |
| `PUFFERFS_DB_MAX_CONNS` | Maximum PostgreSQL connections per Go API process; 1–64. Idle connections expire after one minute. | `4` |
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
| `AWS_SESSION_TOKEN` | Session token when using explicit temporary credentials; included in signed uploads and backend S3 calls. |
| `AWS_REGION` / `AWS_DEFAULT_REGION` | Region. |

Source capture tuning uses plain integer byte values:

| Variable | Meaning | Default |
| --- | --- | --- |
| `PUFFERFS_MULTIPART_PART_BYTES` | Preferred direct-upload part size. Values below S3's 5 MiB minimum use the default; the server raises it when needed to stay within 10,000 parts. | 16 MiB (`16<<20`) |

### Search (Turbopuffer)

| Variable | Meaning | Default |
| --- | --- | --- |
| `TURBOPUFFER_API_KEY` | Turbopuffer API key. | required for search |
| `TURBOPUFFER_API_URL` | Turbopuffer base URL. | provider default |

### Processing workers

Run `python workers/runtime.py ingestion` or `python workers/runtime.py background`.
Both require `DATABASE_URL`, `AWS_BUCKET_NAME`, `GEMINI_API_KEY` and
`TURBOPUFFER_API_KEY`. Use the standard AWS credential chain (ECS task role in
production). `PUFFERFS_WORKER_CONCURRENCY` is 1–64, default 4 file jobs per process.
The background process independently runs provider collection and maintenance.
There are no worker HTTP endpoints or SQS settings.

Turbopuffer provides embeddings internally. Vector-disabled roots support FTS.
`PUFFERFS_EMBEDDING_BATCH_DOCUMENTS` bounds documents per embedding write
(1–256, default 256). Lower values can improve admission under the provider's
token quota; they increase request count and do not raise that quota. Full-text
only writes retain their separate 512-document bound.
Provider extraction settings and optional image fallback are documented below.

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

### CLI release manifest

`GET /cli/version` redirects to `PUFFERFS_CLI_MANIFEST_URL`, default
`https://pufferfs.com/releases/manifest.json`. The installer and CLI read that
same manifest. `scripts/deploy/release-manifest.py` derives downloads and SHA-256
checksums from release artifacts; `PUFFERFS_CLI_MIN_VERSION` is a deployment
input, not API runtime state. Latest version is the selected release tag.

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
- **Admin key**: prefer `PUFFERFS_ADMIN_KEY_HASH` over `PUFFERFS_ADMIN_KEY` so
  the plaintext key is never present in the environment.
- For the production AWS/Pulumi deployment and which of these belong in Secrets
  vs. plain env, see [production-deployment.md](./production-deployment.md).

## Per-file capture

Sync and watch always use capture. They return after durable version acceptance,
without waiting for Gemini or indexing. Use `sync wait` for publication.
There is no mode switch or root-generation fallback.

If another capture has advanced a file's version, registration rejects the whole
batch with HTTP 409 and `code: capture_version_conflict`. Normal sync preserves
the pending capture and stops. Explicit `sync --force` retries that capture first;
on this specific rejection, it moves the entire spool unchanged from `pending/`
to `conflicts/` under the root's `file-capture-*` cache directory, then reads the
latest catalog and captures the **current local files** with a new capture ID.
This can replace newer remote contents with local contents or register deletions
for remotely present files missing locally. It does not merge concurrent edits.
Subset force must select every path in the rejected batch to archive it.

Archived conflicts retain their original journals and source bytes, are not marked
accepted, and are not automatically deleted or retried. The CLI prints the
archive location; successful JSON output includes `conflicts_retained` when
nonzero. Authorization failures, generic 409s, server failures and lost responses
never trigger this recovery. If a newly created capture itself conflicts, sync
stops again rather than rebasing in a loop. Dry runs do not archive conflicts.

`sync --dry-run` reads the current catalog and effective ignore
policy, hashes selected local files with bounded reads, and reports per-file
additions/modifications/removals. It requires a configured, reachable server;
policy/catalog failures stop the preview instead of guessing. A new root can be
previewed without creating it. No generation state, local hash cache, proof
update, source upload, or version registration is written. Pending capture
journals are neither executed nor simulated; an actual sync resumes them first.
Renames appear as add/remove operations. Counts describe live source changes,
not exact upload bytes for a later actual capture. Local files may change after
the preview; the actual capture verifies and captures its own bytes.

`sync status` and `sync status --watch` show a per-file summary
from the paginated catalog; `--json` includes up to 20 non-complete examples.
`sync wait` polls publication state, with `--timeout` and cancellation applied
to polling and catalog requests. Root resolution and local hashing still use
the existing helpers. `--include`/`--exclude` rehash selected local files and
wait for matching captured bytes and their latest extraction to be published.
Unrelated failures are ignored in filtered waits. These commands neither upload
nor enqueue work. Root job IDs, `sync jobs`, `--background` and `--detach` are retired.

Local pending/completed capture spools and per-file heads live beneath the root
cache in `~/.tpfs/roots/`, separated by server and source-directory identity.
Accepted source bytes are removed locally only after upload/registration acceptance
and durable installation of per-file heads. New source bytes share packs of up
to 32 MiB. Appends reuse verified remote prefixes and upload only the suffix.
Rewrites and truncations capture new bytes. Up to 64 completed receipts
are retained. Pending/conflicted journals are never evicted automatically.

`PUFFERFS_CAPTURE_SPOOL_BYTES` bounds captured spool data (default 2 GiB, minimum
8 MiB). Captures contain at most 128 files and reserve space for journal metadata.
All new bytes for a file must fit the available spool budget; verified remote
prefixes consume no spool space. If the new bytes do not fit, sync reports
required/available bytes; raise the budget or resolve retained captures and retry.
Earlier accepted captures remain durable. This is not a cap on all CLI metadata.
Multipart upload resumes acknowledged parts after interrupted connections.

### Retained source-storage verification

`workers/source_verify.py` supplies the separate source-integrity check. It is an
operator command, **not** a deployed Modal application or a queued processing
job. From a Python environment with `boto3` and `psycopg[binary]` installed, use
an explicitly selected database, bucket and root:

```bash
python workers/source_verify.py --org-id ORG_ID --root-id ROOT_ID --bucket BUCKET
```

It reads `DATABASE_URL` and AWS credentials from the environment; `--bucket`
defaults to `AWS_BUCKET_NAME`. It does not automatically load `.env`. Prefer a
read-only database credential and S3 access restricted to the selected root's
`sources/ORG_ID/ROOT_ID/` prefix. No API bearer token, SQS, Gemini, Modal or
Turbopuffer credential is needed. This is privileged operator access, not an
alternative to the application ACL/content-proof checks for end users.

The command enumerates **all retained file versions** under the root, including
superseded versions and originals retained behind file-deletion tombstones. It
reads packed `.jsonl` record locators. It
checks each manifest against its stored bytes (an exact range and record checksum
for packed manifests) and its catalog hash/size, verifies extent ownership and completed upload metadata, then hashes
the reconstructed source to EOF. Tombstones are reported without source reads.
Missing manifests/packs, wrong bytes, invalid metadata and failed reads prevent
a successful summary. It does not silently repair, recapture or delete anything.

Metadata pages contain at most 64 versions. Short read-only database transactions
have a 15-second statement timeout. Source ranges stream through bounded memory;
there is no whole-object disk cache. The report includes GET and downloaded-byte
counts, plus verified logical source bytes. No database transaction spans S3 IO.

Output is JSONL: a record for each version followed by a summary. Exit zero
requires the **final** summary's status to be `verified`, a nonempty inventory,
no source failures, and matching version counts/maximum sequence at the start
and end. A concurrent capture invalidates that summary; a missing/deleting root
or interrupted command cannot produce a success summary. SDK/connection errors
are reported by exception type without their potentially sensitive messages.
Protect any
saved report: it contains paths, hashes and artifact locators, not file bodies.

`--timeout` defaults to 900 seconds. It is checked between database operations
and data blocks, not by forcibly interrupting a blocking syscall. S3 calls have
10-second connect/30-second read timeouts and at most three SDK attempts.
Pause writers for cutover validation: this is bounded, live observation of
immutable versions, not an atomic snapshot spanning Postgres and S3. It proves
that the checked source bytes were readable during the run, not that objects
cannot subsequently be deleted. It does not verify extraction chunks, vectors,
mutation artifacts, index rows, or historical paths absent from the per-file
catalog. It does not convert data from previous ingestion formats.

Transformation and expired-provider-input refresh now use the same catalog-bound
manifest reader: the manifest reference must belong to its root and match its
SHA-256 filename, its declared file hash/size must match the catalog, and every
extent must point to that root's pack/multipart prefix. A manifest's own checksum
is no longer sufficient evidence that it represents the requested file version.

### Per-file index maintenance

The background worker performs bounded cleanup with database and S3 access plus
the search provider key. It derives obsolete-row filters from the published
version/extraction in Postgres and rechecks that head before acknowledging
cleanup. No cleanup mutation files are written. Successful cleanups are checked
again later, so late writes from expired attempts can be removed.

For Python workers, use a provider region with the default/templated URL, or a
fixed `TURBOPUFFER_API_URL` with `TURBOPUFFER_REGION` unset. Compose resolves one
explicit URL for both Go and Python.

### Root deletion recovery and retained sources

Migration 034 records permanent root-deletion targets in Postgres before root
or organization cascades discard their namespace identities. It also
records targets when a root is marked `deleting_at`, so an interrupted delete
remains recoverable. These are maintenance tombstones, independent of the file work queue.
Root IDs and organization ownership are immutable; deleted IDs cannot be reused.
A newly created root with the same name/path receives a new ID as before.

The background maintenance loop sweeps up to 25 due deletion targets
per invocation, with a separate 30-second soft budget. Index delete requests are derived from durable Postgres cleanup targets.
Each index delete filters the recorded namespace by the deleted `root_id`, never
deleting another root's rows even if the namespace was subsequently remapped.
Sources, extraction outputs and mutation prefixes are
cleared in pages of up to 1,000 objects, followed by up to ten multipart aborts.
Checks between requests stop further work after the soft deadline; individual provider timeouts bound in-flight requests.

Partial/failed targets are eligible again in five minutes. Successful targets
become eligible again after one day to catch late writes/uploads; backlog can
extend both intervals. Tombstones and their small S3 erasure records are retained
indefinitely. Successful cleanup does not prove that every old network request
has finished. Root metadata left behind by a failed API delete still requires
that API call to be retried; the sweeper handles external artifacts, not API
response replay or final metadata deletion.

ECS task IAM authorizes reads/writes/deletes and multipart cleanup on the artifact
bucket. No separate Modal CPU principal remains.

`PUFFERFS_SOURCE_RETENTION_SECONDS` and
`PUFFERFS_OBSOLETE_ARTIFACT_RETENTION_SECONDS` default to 30 days (minimum 60
seconds). Current captured/published versions, unfinished work and live provider
submissions pin their sources. Historical shared objects remain until no
retained version references them. Retention also removes legacy mutation
artifacts, but new processing stores only canonical extraction chunks.

### Image extraction fallback

Set `PUFFERFS_VISION_BASE_URL` to an authenticated Modal Shared Endpoint's HTTPS
OpenAI-compatible base URL (including `/v1`) to enable image fallback. Set
`MODAL_PROXY_TOKEN` to the combined `wk-<id>.ws-<secret>` proxy credential and optionally
`PUFFERFS_VISION_MODEL` (default `deepseek-ai/DeepSeek-V4.1-Flash`). These are
collector runtime settings; the deploy workflow copies the URL/model repository
variables and `MODAL_PROXY_TOKEN` repository secret into the worker secret.
An empty base URL leaves fallback disabled.

After a Gemini batch reaches a terminal state, the collector regenerates only
failed image inputs from the verified retained source and sends inline image
bytes to Modal. Completed Gemini pages are preserved. Modal results retain the
same page/frame anchors and record provider/model provenance in the result
manifest. Audio stays on Gemini. This does not bypass an unresolved Gemini
submission or a failure to retrieve its status/results: those retain the
existing recovery path.

Each collector allows four simultaneous fallback requests, with 60-second HTTP
timeouts and a five-minute scheduling budget. Unfinished/failed pages retain
the normal three-attempt batch retry policy. A crash before result publication
can repeat synchronous Modal inference; there is no exactly-once billing claim.
The dedicated E2E suite is `scripts/test-e2e-vision-fallback.sh`; it needs real
Gemini, Modal and Turbopuffer credentials and uses synthetic fixtures only.
The default case corrupts two images at the upload network boundary; setting
`PUFFERFS_E2E_VISION_CASE=cancel` instead cancels the run-owned Google batch and
checks whole-batch fallback. This variable is used only by the E2E driver.

Extraction registration assigns immutable sequences. Index artifacts must carry
their exact extraction sequence. Retry regenerates all text writes from canonical chunks;
there is no partial-checkpoint or missing-sequence compatibility path.

Migration 046 removes the retired generation-sync tables and root metadata,
and limits deletion targets to the current namespace/source/artifact contracts.
`sync audit` and the historical `/roots/{id}/state` endpoint are removed.
