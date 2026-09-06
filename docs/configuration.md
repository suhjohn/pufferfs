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
session_token     = ""
```

| Key | Meaning |
| --- | --- |
| `server.url` | PufferFS API server base URL. |
| `server.api_key` | Tenant API key (`pfs_sk_...`). |
| `turbopuffer.api_key` / `region` | Turbopuffer credentials (advanced/direct setups). |
| `storage.*` | S3-compatible storage endpoint and credentials. |

Per-root local capture cache (identity, journals, immutable spool packs and heads) lives
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
| `PUFFERFS_SOURCE_RANGE_BYTES` | Target bytes in each line-aware local text/code execution range. Every completed range is assigned its own worker shard. Values below 1 MiB use the default; values above the default are capped at the execution-shard budget. | 16,318,456 bytes (about 15.6 MiB) |

### Search (Turbopuffer)

| Variable | Meaning | Default |
| --- | --- | --- |
| `TURBOPUFFER_API_KEY` | Turbopuffer API key. | required for search |
| `TURBOPUFFER_API_URL` | Turbopuffer base URL. | provider default |
| `PUFFERFS_TP_NAMESPACE_SHARDS` | Physical namespaces per root, set at root creation. | 1 (max 256) |
| `PUFFERFS_TP_WRITE_BATCH_ROWS` | Rows per Turbopuffer upsert request. | 512 (max 512) |
| `PUFFERFS_TP_WRITE_BATCH_BYTES` | Approximate maximum serialized Turbopuffer request size; row buffers reserve 64 KiB for the envelope/schema. | 8 MiB (max 8 MiB) |

### Modal compute

| Variable | Role |
| --- | --- |
| `MODAL_TRANSFORM_ENDPOINT` | Consumer → CPU transformation worker |
| `MODAL_FILE_INDEX_ENDPOINT` | Consumer → GPU index worker |
| `MODAL_FILE_CPU_INDEX_ENDPOINT` | Consumer → no-vector index worker |
| `MODAL_QUERY_EMBED_ENDPOINT` | API → separate query embedder |
| `MODAL_SECRET_KEY` | API/consumer caller authentication |
| `PUFFERFS_MODAL_ENDPOINT_SECRET_NAME` | Modal auth secret; default `pufferfs-endpoint-auth` |
| `PUFFERFS_WORKER_SECRET_NAME` | Modal worker credentials; default `pufferfs-workers` |
| `PUFFERFS_MODAL_EMBED_GPU` | Bulk GPU; default L4 |
| `PUFFERFS_MODAL_INDEX_MAX_CONTAINERS` | Bulk pool limit; default 16 |
| `PUFFERFS_MODAL_QUERY_EMBED_GPU` | Query GPU; default L4 |
| `PUFFERFS_MODAL_QUERY_EMBED_MIN_CONTAINERS` | Warm query workers; default 1 |
| `PUFFERFS_MODAL_QUERY_EMBED_MAX_CONTAINERS` | Query pool limit; default 2 |

Worker credentials include `DATABASE_URL`, S3 credentials/bucket, both SQS
URLs, `GEMINI_API_KEY` and `TURBOPUFFER_API_KEY`. Include `AWS_SESSION_TOKEN`
with temporary credentials. The query app receives only the endpoint-auth
secret containing `PUFFERFS_MODAL_ENDPOINT_AUTH_KEY`, equal to the caller's
`MODAL_SECRET_KEY`.

Pinned Nomic model/code revisions live in `modal/nomic_model.py`. Workers
persist vector bodies in S3; Postgres stores locators. Documents/media use
Gemini 3.5 Flash-Lite Batch without provider roulette or native-PDF-text bypass.

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

### Queue and process roles

Both `PUFFERFS_SQS_TRANSFORM_QUEUE_URL` and
`PUFFERFS_SQS_INDEX_QUEUE_URL` are required at API/consumer startup.

`PUFFERFS_PROCESS=worker` starts a consumer; `PUFFERFS_WORKER_STAGE` selects
`transform` or `index`. Each process accepts
`PUFFERFS_WORKER_CONCURRENCY` (default 4, maximum 64). Pulumi defaults to
16 concurrent jobs per consumer service.

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
- **SQS is required.** Missing queue configuration fails startup.
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

Archived conflicts retain their original journals and pack bytes, are not marked
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
not exact upload bytes after append reuse/packing. Local files may change after
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
Accepted pack bytes are removed only after upload/registration acceptance and
durable installation of the per-file heads. Append reuse reads those heads'
remote extent mappings, not old local packs. Up to 64 completed journal receipts
are retained per cache. Pending/conflicted journals and their bytes are never
evicted; incomplete directories with no published journal are discarded on the
next sync under its exclusive lock. These were never submit-ready captures.

`PUFFERFS_CAPTURE_SPOOL_BYTES` bounds captured spool data per server/root/source
cache (default 2 GiB, minimum 8 MiB). Pending, conflicted and completed metadata
count toward available capacity; new captures reserve 4 MiB for their journal.
An oversized capture fails before exceeding its pack-byte budget. Raise the
limit explicitly for larger files/batches. Cleanup does not erase originals in
S3 and never treats an upload error as acceptance. Head metadata scales with
tracked paths; the limit is not a cap on all CLI disk usage.

### Historical recapture audit

Catalog-only `sync status` cannot detect a historical file that has never been
registered in the new catalog. Before retiring the old index, run this read-only
audit on the source host against a prepared **non-production validation**
deployment with per-file processing enabled:

```bash
pufferfs sync audit ROOT_ID --source /path/to/root --json
```

Use the actual root ID and a credential with legacy-state read access and sync
access to the entire root. The command reads the old `/state` inventory and the
paginated captured catalog, then hashes their union of paths locally using
bounded reads. It neither trusts local hash caches nor writes proofs. The source
directory must be explicit; it is not taken from a remote path. Historical paths
remain in the audit even when current ignore rules exclude them from sync.

- `needs_capture`: local bytes have no captured version or differ from it.
- `missing_original`: the legacy inventory names a path with neither a captured
  version nor an available local original. Restore it or resolve its intended
  deletion explicitly; the audit does not authorize or register a deletion.
- `missing_source_reference`: the catalog has no usable source metadata. This
  is a repair requirement, not a claim that an S3 object was checked.
- `pending`, `running`, `waiting_provider`, `failed`, etc.: capture exists, but
  its latest extraction is not successfully published.
- `complete` / `deleted`: the observed catalog reports publication of the
  current captured version, including an explicit deletion when applicable.

The report includes every audited path, whether its local bytes still match the
legacy hash, the observed legacy generation, and `source_storage_verified:
false`. A changed legacy hash is informational: capturing the current filesystem
does not recover an older version whose original bytes no longer exist.
Only `catalog_covered` exits successfully; incomplete and empty inventories exit
nonzero. JSON remains parseable on an incomplete result; the error goes to stderr.
Missing/unreadable inventory, an advancing legacy generation, unsafe local paths,
nonregular files and detected changes while hashing fail the audit. `--timeout`
(default 15 minutes) and cancellation apply to requests and local hashing.

Recapture uses the existing sync command, not a second uploader:

```bash
pufferfs sync /path/to/root --id ROOT_ID --dry-run
pufferfs sync /path/to/root --id ROOT_ID
pufferfs sync wait ROOT_ID
pufferfs sync audit ROOT_ID --source /path/to/root --json
```

Review the preview and effective ignore rules first. Normal sync may register
deletions for **already-captured** paths absent locally and resumes pending
journals before scanning. It does not synthesize tombstones for legacy-only
paths. Matching legacy hashes do not suppress initial capture: those paths still
need immutable source packs and per-file registration. A repeated accepted sync
reuses its local heads; it does not recapture merely because indexing is pending.

This audit is one migration check, **not a cutover gate on its own**. It does not
scan local paths absent from both inventories, download or hash S3 objects,
inspect actual index rows/search results, recover already-lost legacy inventory,
or bypass path ACLs. Catalog pages and local reads are not an atomic root snapshot;
pause writers and repeat validation for a cutover. Full source-storage integrity,
read/search authorization, live worker/provider delivery, and production rollout
still require independent verification and normal deployment approval.

### Retained source-storage verification

`modal/source_verify.py` supplies the separate source-integrity check. It is an
operator command, **not** a deployed Modal application or a queued processing
job. From a Python environment with `boto3` and `psycopg[binary]` installed, use
an explicitly selected database, bucket and root:

```bash
python modal/source_verify.py --org-id ORG_ID --root-id ROOT_ID --bucket BUCKET
```

It reads `DATABASE_URL` and AWS credentials from the environment; `--bucket`
defaults to `AWS_BUCKET_NAME`. It does not automatically load `.env`. Prefer a
read-only database credential and S3 access restricted to the selected root's
`sources/ORG_ID/ROOT_ID/` prefix. No API bearer token, SQS, Gemini, Modal or
Turbopuffer credential is needed. This is privileged operator access, not an
alternative to the application ACL/content-proof checks for end users.

The command enumerates **all retained file versions** under the root, including
superseded versions and originals retained behind file-deletion tombstones. It
checks each content-addressed manifest against its stored bytes and its catalog
hash/size, verifies extent ownership and completed upload metadata, then hashes
the reconstructed source to EOF. Tombstones are reported without source reads.
Missing manifests/packs, wrong bytes, invalid metadata and failed reads prevent
a successful summary. It does not silently repair, recapture or delete anything.

Metadata pages contain at most 64 versions. Short explicit read-only database
transactions have a 15-second statement timeout; no database transaction is
held while downloading S3 content. Pack downloads use 64 KiB buffers and a
private temporary disk cache capped at 256 MiB, with a 128 MiB individual-pack
limit matching the capture API. Files/versions sharing cached packs reuse their
GETs. Eviction can require downloading a pack again; manifests are still read
per version. Whole-pack downloads trade extra bytes for fewer per-extent calls.
The report includes pack GET call counts (SDK retries can add requests),
downloaded pack bytes and verified logical source bytes so that cost is visible.

Output is JSONL: a record for each version followed by a summary. Exit zero
requires the **final** summary's status to be `verified`, a nonempty inventory,
no source failures, and matching version counts/maximum sequence at the start
and end. A concurrent capture invalidates that summary; a missing/deleting root
or interrupted command cannot produce a success summary. SDK/connection errors
are reported by exception type without their potentially sensitive messages.
Temporary packs are removed on normal exit and handled errors. Protect any
saved report: it contains paths, hashes and artifact locators, not file bodies.

`--timeout` defaults to 900 seconds. It is checked between database operations
and data blocks, not by forcibly interrupting a blocking syscall. S3 calls have
10-second connect/30-second read timeouts and at most three SDK attempts.
Pause writers for cutover validation: this is bounded, live observation of
immutable versions, not an atomic snapshot spanning Postgres and S3. It proves
that the checked source bytes were readable during the run, not that objects
cannot subsequently be deleted. It does not verify extraction chunks, vectors,
mutation artifacts, index rows, or historical paths absent from the per-file
catalog; use the preceding local audit for that last coverage check. Neither
command alone authorizes production cutover or legacy deletion.

Transformation and expired-provider-input refresh now use the same catalog-bound
manifest reader: the manifest reference must belong to its root and match its
SHA-256 filename, its declared file hash/size must match the catalog, and every
extent must point to that root's pack/multipart prefix. A manifest's own checksum
is no longer sufficient evidence that it represents the requested file version.

### Per-file index maintenance

The scheduled `pufferfs-reconciliation` deployment now requires `AWS_BUCKET_NAME`,
S3 read/write access to cleanup mutation artifacts, and `TURBOPUFFER_API_KEY`
in addition to its existing database/SQS configuration. It shares the index
worker's `TURBOPUFFER_REGION` / optional `TURBOPUFFER_API_URL` settings. Apply
migrations through 034 before deploying the updated worker roles. Drain old
workers before this rollout: they do not enforce extraction-revision ordering.

For Python workers, use either a region with the default/`{region}`-templated
provider URL, or a fixed `TURBOPUFFER_API_URL` with `TURBOPUFFER_REGION` unset.
The SDK reads that environment variable even when the application omits the
region argument and rejects it alongside a fixed URL. Compose resolves one
explicit regional URL for both Go and Python and does not forward the region
variable separately.

After handoff recovery and bounded deleted-root cleanup, it selects at most
1,000 published files and removes index rows below each published version/revision
cutoff. New cleanup records share one S3 pack per organization/root in that sweep;
existing records are replayed from their saved packs. Each referenced pack is read
once per sweep, retaining only the selected files' records, not unrelated history.
Metadata reservations, locator installation and success checkpoints use bulk
updates. At most eight index requests run concurrently, without holding database
connections. This reduces serial waiting and metadata round trips, not the number
of per-file Turbopuffer delete requests.
Success is rechecked after one day; partial/failed operations become eligible
after five minutes. Backlog can extend these intervals. The role retains its
180-second timeout and a 90-second soft budget for the entire per-file maintenance
sweep. At the budget, it stops starting index requests, waits for started requests
and checkpoints their successes; provider timeouts still bound individual network
operations. No new queue or always-running service is introduced.
Historical rows without revision order and
namespace remapping cleanup remain cutover gaps. This does not delete source
packs, extracted chunks, vectors, or local capture spools for live roots.

### Root deletion recovery and retained sources

Migration 034 records permanent root-deletion targets in Postgres before root
or organization cascades discard their namespace/generation identities. It also
records targets when a root is marked `deleting_at`, so an interrupted delete
remains recoverable. These are maintenance tombstones, not the SQS work queue.
Root IDs and organization ownership are immutable; deleted IDs cannot be reused.
A newly created root with the same name/path receives a new ID as before.

The same `pufferfs-reconciliation` deployment sweeps up to 25 due deletion targets
per invocation, with a separate 30-second soft budget. Index deletion records
are packed once in `maintenance/root-deletions/` in S3 and reused on retries.
Each index delete filters the recorded namespace by the deleted `root_id`, never
deleting another root's rows even if the namespace was subsequently remapped.
Sources, extraction outputs, mutations and legacy root/generation prefixes are
cleared in pages of up to 1,000 objects, followed by up to ten multipart aborts.
Checks between requests stop further work after the soft deadline; the existing
180-second deployment timeout remains the hard limit.

Partial/failed targets are eligible again in five minutes. Successful targets
become eligible again after one day to catch late writes/uploads; backlog can
extend both intervals. Tombstones and their small S3 erasure records are retained
indefinitely. Successful cleanup does not prove that every old network request
has finished. Root metadata left behind by a failed API delete still requires
that API call to be retried; the sweeper handles external artifacts, not API
response replay or final metadata deletion.

The Modal reconciliation AWS principal additionally needs `s3:DeleteObject` and
`s3:AbortMultipartUpload` on those exact root/generation prefixes,
`s3:ListBucket` and `s3:ListBucketMultipartUploads` on the bucket, and S3 get/put
access to `maintenance/root-deletions/`. Pulumi does not provision the external
Modal principal; its credentials/policy must be verified separately before
deployment. No new permission is needed by the ECS consumers for this sweep.

Migration 039 adds source-pack provenance and reachability metadata; migration
040 defers the source-object foreign-key check until both root-deletion cascade
paths complete. The
reconciler uses `PUFFERFS_SOURCE_RETENTION_SECONDS` (default 30 days, minimum
60 seconds). Captured/indexed heads and nonterminal work pin their source
versions. Only obsolete versions whose extraction/work lifetimes have ended
and aged out retire. A pack remains whole while any retained version references
any byte range in it; append reuse does not extend or copy those bytes.

Unreferenced packs can retire only after their retention interval and every
recorded upload authorization have expired. Actual signed URL deadlines are
persisted before URLs are exposed. Capture registration and retirement share
the root lock, and retired keys can never be reauthorized. Up to 100 versions
and packs are considered per scheduled pass, with a 15-second loop budget;
S3 objects are deleted in a batch, and exact-key abandoned multipart sessions
are aborted. Failed/partial targets retry after five minutes; successful
tombstones are checked daily for late writes. Individual network timeouts can
extend the loop budget. Source manifests, version records and extent edges
remain audit receipts. This is not physical erasure of noncurrent S3 versions;
versioned-bucket lifecycle policy remains a deployment gate.

New upload keys belong to their authenticated uploader. On first registration
they are bound to one capture transaction; a later capture may reuse only ranges
present in that same file's prior version. Neither a sibling's pack location nor
an old capture ID authorizes grafting its bytes into another path. A definitive
`source_pack_reupload_required` response lets the CLI replace upload identities using its
unchanged local spool, capture ID and digests. Other errors never trigger that
reset, and remote-only append extents cannot be silently replaced.

Catalog acceptance rechecks current organization membership, role, API-key
validity/scopes, root grants, group memberships and folder denies after the
manifest write. It holds the root and the exact authorizing rows only until the
short catalog transaction commits. A revocation that committed during upload
therefore prevents catalog/proof acceptance; restoring access permits retry of
the same captured bytes. Existing presigned upload URLs remain capabilities
until their recorded expiry; this fence does not revoke a URL already issued or
claim instantaneous revocation of unrelated read requests.

Legacy manifests are backfilled in bounded scheduled passes. Pack GC fails
closed for any root with unknown version extents; corrupt/missing manifests
remain visible as backfill failures. Legacy uploads lack uploader identity:
existing authorized same-file ranges remain reusable after backfill. Unaccepted
pre-migration uploads require re-upload from the retained local spool using the
new CLI; knowledge of an old key alone never authorizes a new binding.
Fresh-schema source-GC assertions passed, including real upload expiry and
pending-spool recovery. The initial root-cleanup failure passed after the 040
API migration. A latest-code full rerun, populated pre-039 upgrade, paused-reader
and real AWS permission validation are still required. Explicit root deletion
continues to erase that root's current objects and abandoned multipart uploads.

Migration 038 tracks retired extraction artifacts. The scheduled reconciler
uses `PUFFERFS_OBSOLETE_ARTIFACT_RETENTION_SECONDS` (default 30 days, minimum 60
seconds). A version is protected while either captured or indexed; pending,
running, failed, or provider-waiting work is protected regardless of age. Only
terminal obsolete extractions whose extraction/work timestamps have aged out
and whose referenced provider batches are terminal can retire. Cleanup targets
only their exact `extractions/{org}/{root}/{extraction}/` and
`mutations/{org}/{root}/{extraction}/` prefixes, including abandoned multipart
uploads; it never sweeps source packs/manifests or shared index-cleanup cutoff
artifacts. Each pass handles at most five extractions, one object page and ten
multipart uploads per prefix, with a 15-second loop budget (individual network
timeouts may extend it). Partial/error passes retry after five minutes;
successful tombstones repeat daily for late writes. Metadata remains for audit,
and root deletion takes over cleanup if the catalog is removed. The fresh
Compose retention suite passed before migration 039; race coverage with paused work/retries and
populated-database migration remain verification gates.

Migration 037 tracks organization-shared embedding-cache packs independently of
tenant rows. The scheduled reconciler retires at most 100 cold packs per pass,
using `PUFFERFS_EMBEDDING_CACHE_RETENTION_SECONDS` (default 30 days, minimum 60
seconds). Any cache hit refreshes the whole pack. Readers and writers lock the
pack row through bounded S3 IO; retirement atomically removes its lookup entries
and permanently fences that identity before a batched S3 deletion. New uploads
are registered before S3 IO, use single-use identities, and are collectible even
if upload/locator publication is interrupted. Tombstones survive tenant deletion
and repeat acknowledged deletes daily to catch late writes; they are not a
claim of physical erasure in versioned buckets. A cold cache miss is re-encoded.
Published mutation artifacts contain their vectors and remain replayable without
the cache. Failed batch-delete entries retry after five minutes. This requires
the reconciler's S3 DeleteObject permission for the `embeddings/` prefix, not
new permissions for clients. The pre-039 Compose retention suite passed cache
reuse, actual scheduled expiry, preserved search/mutations, and re-encoding after
eviction. It does not yet verify deletion races with paused cache readers/writers.

Migration 035 tracks temporary
Google upload IDs separately from root/request rows. The collector releases exact
tracked page/clip and batch-input uploads after all active transform work,
batches and unfinished requests release them. For a deleted root it first
cancels the provider job and waits for terminal status. An acknowledged delete
or provider 404 sets `deleted_at`; a 403 never does. Migration 036 records the
provider's returned `expirationTime` as `expires_at`, falling back conservatively
to 48 hours after registration for uploaded Files without that metadata. Retries
never extend this deadline. After it passes, bounded maintenance sets the
separate `expired_at` and stops pointless delete retries without removing any
source or batch recovery mappings. This records the provider retention deadline,
not independently verified physical erasure. The [Files reference](https://ai.google.dev/api/files#File)
defines the returned expiry timestamp; [uploaded Files retention](https://ai.google.dev/gemini-api/docs/files#delete-uploaded-files)
is distinct from Google's six-week generated batch-result retention. Generated
results are not registered as deletable uploads: deleting their batch did not
make result bytes unavailable in the real-provider diagnostic. Unrecorded upload
responses still require orphan/expiry handling. These provider cleanup paths
are implemented. The complete lost-response/partial-retry real-provider E2E
passed; the elapsed-expiry check remains a verification gate.
No full physical-erasure
claim is made for them. Roots hard-deleted before migration 034 have no retained
cleanup identity and need separate historical reconciliation.

Migration 032 assigns immutable extraction registration sequences. Since the
historical order was not recorded, it conservatively ranks each already-published
extraction above its pre-migration alternatives. To replace that result, register
a genuinely new extraction revision; replaying an old registration cannot
promote its priority. Existing mutation artifacts lacking `extraction_sequence`
can still replay, but same-version cleanup deliberately retains those rows.
