# PufferFS HTTP API Reference

This reference documents the PufferFS API server's HTTP surface as implemented
in `internal/server/handlers.go`. It is intended for people who integrate with
PufferFS directly — scripts, agents, and services — rather than through the CLI.

The CLI (`pufferfs`) is a client of this same API; anything the CLI does can be
done over HTTP.

## Conventions

- Base URL is your server URL, e.g. `https://api.example.com`.
- All request and response bodies are JSON. Responses set
  `Content-Type: application/json`.
- Errors use the shape `{"error": "<message>"}` with an appropriate HTTP status.
  Some errors include extra fields (see [Sync conflicts](#sync-conflicts)).
- Path parameters are written as `{id}`.
- Unless noted, every route requires authentication (see [Authentication](#authentication)).

### Common status codes

| Status | Meaning |
| --- | --- |
| `200 OK` | Success. |
| `201 Created` | Resource created (API keys, roots, ACLs). |
| `202 Accepted` | Async sync accepted; poll for completion. |
| `400 Bad Request` | Malformed JSON, missing required field, or invalid value. |
| `401 Unauthorized` | Missing or invalid credentials. |
| `403 Forbidden` | Authenticated but lacking the required scope or role. |
| `404 Not Found` | Resource missing, or hidden because the caller cannot read it. |
| `409 Conflict` | Stale sync base generation, sync already in progress, or active sync blocks deletion. |
| `500 Internal Server Error` | Unexpected server-side failure. |
| `503 Service Unavailable` | Readiness probe failed (database unreachable). |

> Note on 404 vs 403: for roots, an unreadable root returns `404 Not Found`
> rather than `403`, so callers cannot probe for the existence of roots they
> cannot access.

## Authentication

Normal routes accept either of:

- **API key** — `Authorization: Bearer <pfs_sk_...>`. Resolved to an
  org/user/role/scope set. This is the path used by the CLI and by automation.
- **JWT session** — `Authorization: Bearer <jwt>` or the `pf_session` httpOnly
  cookie set by an interactive login provider such as email-code or Google
  OAuth. Used by the web console.

`/admin/*` routes require a **separate platform admin key** as
`Authorization: Bearer <admin-key>`, compared in constant time against the
configured admin key hash. Normal API keys and JWTs cannot reach admin routes.

### Authorization model

Two independent layers are checked:

1. **API key scopes** — a scoped key must carry the required scope, a documented
   alias, or `*`. JWT sessions and legacy keys with no scopes are treated as
   unrestricted; newly created user keys must send an explicit non-empty scope
   list. Common scopes: `sync`, `query`, `root:delete`,
   `api_keys:read`, `api_keys:write`, `acl:read`, `acl:write`, `org:admin`,
   plus coarse aliases `read`/`write`/`admin`/`delete`.
2. **Org role** — `owner > admin > editor > viewer`. Role gates writes,
   deletes, and member/ACL management regardless of scope.

Root visibility additionally depends on root scope and ownership:

- `org` roots: readable by any org member; writable by editor+, deletable by
  admin+.
- `user` roots: readable/writable/deletable by the owner or an org admin+.

See [security-and-data-handling.md](./security-and-data-handling.md) for the
full model.

### Unauthenticated routes

These skip the auth middleware entirely: `GET /healthz`, `GET /readyz`,
`GET /health`, `GET /cli/version`, login routes (`/auth/providers`,
`/auth/google`, `/auth/callback`, `/auth/email/start`, `/auth/email/resend`,
`/auth/email/verify`, `/auth/logout`), and `POST /billing/webhook` (verified by
Stripe signature instead).

---

## Health and metadata

### `GET /healthz` · `GET /health`

Liveness. Always `200 {"status":"ok"}`. `/health` is a backward-compatible
alias.

### `GET /readyz`

Readiness. Pings the database. `200 {"status":"ready"}` or
`503 {"status":"not ready","error":"database: ..."}`.

### `GET /cli/version`

Returns the CLI release manifest used by `pufferfs upgrade`. No auth.

```json
{
  "latest": "0.3.0",
  "minimum": "0.2.0",
  "protocol_min": 1,
  "protocol_max": 1,
  "downloads": {
    "darwin-arm64": { "url": "https://.../pufferfs_0.3.0_darwin_arm64.tar.gz", "sha256": "..." }
  },
  "notes_url": "https://github.com/suhjohn/pufferfs/releases/download/v0.3.0"
}
```

The values are driven by server env vars (`PUFFERFS_CLI_LATEST_VERSION`,
`PUFFERFS_CLI_MIN_VERSION`, `PUFFERFS_CLI_DOWNLOAD_BASE_URL`,
`PUFFERFS_CLI_SHA256_<PLATFORM>`). `protocol_min`/`protocol_max` are both the
server's `SyncProtocolVersion` (currently `1`).

---

## Auth and identity

### `GET /auth/providers`

Returns which interactive login providers are enabled for this deployment.

```json
{ "email_code": true, "google": true }
```

### `POST /auth/email/start`

Start an email one-time-code login. No authentication required.

Request:

```json
{ "email": "user@example.com", "flow": "web" }
```

For CLI login, send `flow: "cli"` and a loopback `cli_redirect_uri`.

Response `200`:

```json
{ "challenge_id": "elc_...", "expires_in": 600, "resend_after": 30 }
```

### `POST /auth/email/verify`

Verify an email login code. No authentication required. Web flow sets the
httpOnly `pf_session` cookie used by the dashboard. CLI flow returns a scoped
CLI API key.

Request:

```json
{ "challenge_id": "elc_...", "code": "12345678" }
```

Web response `200`:

```json
{ "status": "ok" }
```

CLI response `200`:

```json
{ "status": "ok", "api_key": "pfs_sk_...", "email": "user@example.com" }
```

### `GET /auth/me`

Returns the authenticated user plus org context.

```json
{ "user": { "id": "...", "email": "...", "name": "..." }, "org_id": "...", "role": "editor" }
```

### `POST /auth/api-keys`

Create an API key for the calling user's org. Requires scope
`api_keys:write` / `admin` / `write`.

Request:

```json
{ "name": "CI key", "scopes": ["query"] }
```

Defaults: `name` → `"CLI Key"`. `scopes` must be explicit and non-empty for
new user-created keys; use `["query"]` for read-only search automation and add
broader scopes only when the key needs sync or root management access.

Response `201`:

```json
{ "key": "pfs_sk_..." }
```

The raw key is returned **once** and only stored hashed (SHA-256) — capture it
immediately.

### `GET /auth/api-keys`

List API keys (metadata only, no secrets). Requires
`api_keys:read` / `api_keys:write` / `admin` / `read` / `write`.

### `DELETE /auth/api-keys/{id}`

Revoke an API key. Requires `api_keys:write` / `admin` / `write`.
Returns `{"status":"deleted"}`.

---

## Org management

| Route | Description | Required |
| --- | --- | --- |
| `GET /org` | Get the caller's organization. | authenticated |
| `GET /org/members` | List org members. | authenticated |
| `POST /org/members` | Add/upsert a member. Body: `{"user_id","role"}`. | admin role + `org:admin`/`admin`/`write` |
| `DELETE /org/members/{userId}` | Remove a member. | admin role + `org:admin`/`admin`/`write` |

---

## Ignore Policies

Server-managed ignore policies use gitignore-style pattern text. They are
additive deny rules: if org policy or user policy matches a path, new uploaded
content for that path is rejected during sync finalize. Remove/close operations
for previously indexed ignored paths are allowed so policy changes can remove
existing rows from the visible index.

### `GET /ignore-policy`

Return the effective central policy for the authenticated caller. Requires
`query`, `sync`, `read`, or `write` scope.

```json
{
  "org_patterns": "blocked-org/\n*.secret\n",
  "user_patterns": "scratch/\n*.local\n"
}
```

### `GET /ignore-policy/user`

Return the caller's user-level policy document for the current org. Requires
`query`, `sync`, `read`, or `write` scope.

### `PUT /ignore-policy/user`

Replace the caller's user-level policy document for the current org. Requires
`sync` / `write`.

```json
{ "patterns": "scratch/\n*.local\n" }
```

### `GET /ignore-policy/org`

Return the org-level policy document for the current org. Requires `query`,
`sync`, `read`, `write`, `org:admin`, or `admin` scope.

### `PUT /ignore-policy/org`

Replace the org-level policy document for the current org. Requires admin role
and `org:admin` / `admin` / `write` scope.

```json
{ "patterns": "blocked-org/\n*.secret\n" }
```

Policy update responses:

```json
{
  "org_id": "...",
  "user_id": "...",
  "patterns": "scratch/\n*.local\n",
  "updated_by_user_id": "...",
  "updated_at": "RFC3339"
}
```

---

## Roots

A root is the unit of sync and access control.

### `POST /roots`

Create a root. Requires scope `sync` / `root:create` / `write`.

Request:

```json
{ "name": "workspace", "source_path": "/Users/me/workspace", "scope": "org", "owner_user_id": "" }
```

- `scope`: `org` (default) or `user`.
- `org` scope requires editor+.
- `user` scope defaults the owner to the caller; setting another `owner_user_id`
  requires admin+, and the owner must be an org member.

Response `201`: the `RootMetadata` object (see [Schemas](#schemas)).

### `GET /roots`

List roots the caller can access. Requires `query` / `sync` / `read` / `write`.
Returns an array of `RootMetadata`.

### `GET /roots/{id}`

Get one root. `404` if it does not exist or the caller cannot read it.

### `DELETE /roots/{id}`

Delete a root and all its PufferFS artifacts. Requires
`root:delete` / `delete` / `write` **and** delete rights on the root
(admin+ for org roots; owner or admin+ for user roots).

- `409` if the root has active sync jobs.
- Removes Turbopuffer namespaces and S3 objects under `files/`, `bundles/`,
  `states/`, `chunks/`, and `syncs/` for the root's generations. **Source files
  on the user's machine are not touched.**

Response `200`:

```json
{
  "status": "deleted",
  "root_id": "...",
  "name": "workspace",
  "turbopuffer_ns": "org-...-root-...",
  "turbopuffer_namespaces": ["..."],
  "s3_objects_deleted": 1234
}
```

### `POST /roots/{id}/upload?path=<relpath>[&generation_id=<id>]`

Upload a single source file's bytes (large/empty files). Requires `sync`/`write`
and write-ACL on the path. Body is the raw file. **Max 512 MiB.** With
`generation_id`, stored as temporary transport at
`syncs/<generationID>/sources/files/<path>`. Without `generation_id`, stored at
legacy `files/<rootID>/<path>`. Response `{"key": "..."}`.

### `POST /roots/{id}/upload-bundle?bundle_id=<id>[&generation_id=<id>]`

Upload a packed small-file bundle or a gzip state ref. Requires `sync`/`write`.
Body is the raw bundle. **Max 1024 MiB.** With `generation_id`, stored as
temporary transport at `syncs/<generationID>/sources/bundles/<bundleID>`.
Without `generation_id`, stored at legacy `bundles/<rootID>/<bundleID>`.
Response `{"key": "..."}`.

### `GET /roots/{id}/state`

Return the root's current committed file-state map
(`{ "<path>": { "size", "content_hash", "mtime" } }`). Requires read access.
Used by the CLI to diff against the server when local cache is stale.

### `POST /roots/{id}/read`

Read a deterministic slice from one known file. Requires scope `query` / `read`
and read access to the root/path. This is not search; use it when the caller
already knows the file path and wants a page or line range.

Request:

```json
{
  "path": "docs/manual.pdf",
  "pages": { "start": 10, "end": 12 },
  "include_images": true
}
```

or:

```json
{
  "path": "src/main.go",
  "lines": { "start": 200, "end": 400 }
}
```

Exactly one of `pages` or `lines` is required. Ranges are 1-based inclusive and
may include at most 1000 items.

Behavior:

- Page reads use indexed document chunks with `page_number` / `image_path`.
- When `include_images` is true, page results include an authenticated
  `image_url` for `GET /roots/{id}/assets`.
- Line reads require chunks indexed with `line_start` / `line_end`; files synced
  before that metadata existed may need to be resynced.
- ACL, visible-generation, and user-root content-proof filtering match query
  behavior.

Response:

```json
{
  "root_id": "...",
  "root_name": "handbook",
  "file_path": "docs/manual.pdf",
  "mode": "pages",
  "pages": [
    {
      "page": 10,
      "page_number": 9,
      "chunk_index": 9,
      "content": "...page text...",
      "image_path": "chunks/<root>/docs/manual.pdf.9.jpg",
      "image_url": "/roots/<root>/assets?key=chunks%2F..."
    }
  ]
}
```

### `GET /roots/{id}/assets?key=<storage-key>`

Download a returned page/image asset. Requires scope `query` / `read`. The key
must be an active indexed `image_path` under `chunks/<rootID>/`, and the caller
must be allowed to read the row that references it.

### `POST /roots/{id}/sync`

Submit a sync. See [Sync](#sync) below.

### `POST /roots/{id}/sync/init`

Initialize a sync session. Creates a `SyncJob` and a building `SyncGeneration`
server-side, returning IDs and a manifest prefix the client uses for subsequent
artifact uploads. Requires scope `sync` / `write` and write access to the root.

Request:

```json
{
  "protocol_version": 1,
  "base_generation_id": "<current-visible-generation-or-empty>",
  "base_generation_seq": 7,
  "total_files": 500
}
```

Response `200`:

```json
{
  "root_id": "...",
  "sync_job_id": "...",
  "generation_id": "...",
  "generation_seq": 8,
  "base_generation_id": "...",
  "base_generation_seq": 7,
  "manifest_prefix": "syncs/<generation_id>/manifests/"
}
```

If `base_generation_id`/`seq` is stale, returns `409` with a sync conflict
(same shape as [sync conflicts](#sync-conflicts)).

### `POST /roots/{id}/sync/{generation_id}/upload`

Upload a generation-scoped artifact. Requires scope `sync` / `write`. The
artifact `kind` and `name` are specified as query parameters:

```
POST /roots/{id}/sync/{generation_id}/upload?kind=manifest&name=000000.jsonl
POST /roots/{id}/sync/{generation_id}/upload?kind=proof&name=content-proof.json
POST /roots/{id}/sync/{generation_id}/upload?kind=state&name=state.json.gz
```

Body is the raw artifact bytes (gzipped JSONL for manifests, gzipped JSON for
state/proof). **Max 512 MiB.** Returns `200` with the stored object key:

```json
{ "ref": "syncs/<generation_id>/manifests/000000.jsonl" }
```

### `DELETE /roots/{id}/sync/{generation_id}`

Abort an unsubmitted sync session. Marks the generation as `failed` and cleans
up any artifacts already uploaded under `syncs/<generation_id>/`. Requires scope
`sync` / `write`. Returns `200 {"status":"aborted"}`.

Use this if the client encounters an upload error before calling
`POST /roots/{id}/sync` to finalize.

The server also deletes generation-scoped temporary transport/artifact objects
when finalize is rejected, processing fails, an async job expires incomplete, or
the generation commits successfully.

### `GET /roots/{id}/sync/status[?job_id=<id>]`

Return a sync job's status. Without `job_id`, returns the latest job for the
root. Jobs that exceed the server sync timeout are transitioned to `failed`.
Returns a `SyncJob` object. In-flight statuses include `queued`, `chunking`,
`embedding`, `indexing`/`upserting`, and `committing`; terminal statuses are
`completed` and `failed`.

### `GET /roots/{id}/sync/jobs`

Return up to the 20 most recent `SyncJob`s for the root. Requires read access.

---

## Sync

### `POST /roots/{id}/sync[?async=true]`

Requires scope `sync` / `write`, write access to the root, and write-ACL on
every changed path. Request body is a `SyncRequest`:

```json
{
  "protocol_version": 1,
  "generation_id": "<from-sync-init>",
  "base_generation_id": "<id-or-empty>",
  "base_generation_seq": 7,
  "change_refs": ["syncs/<generation>/manifests/000000.jsonl", "..."],
  "changes": [],
  "state_ref": "syncs/<generation>/state/state.json.gz",
  "simhash": "...",
  "content_proof": { "root_hash": "...", "file_hashes": {}, "dir_hashes": {} },
  "manifest_ref": "..."
}
```

When using the manifest-session flow (recommended for large trees):

1. Call `POST /roots/{id}/sync/init` to get a `generation_id`.
2. Upload manifest shards and state via `POST /roots/{id}/sync/{generation_id}/upload`.
3. Submit the finalize request with `generation_id` and `change_refs` pointing to
   the uploaded manifest shards. `changes` can be empty when `change_refs` is provided.

For backward compatibility, inline `changes` without a `generation_id` still works
for small syncs.

Rules:

- `protocol_version` must equal the server's `SyncProtocolVersion` (`1`), else
  `400` with `{"error","protocol_version","required_version"}`.
- Exactly one of `state` (inline map) or `state_ref` (object key) is required.
  Inline state is gzipped and persisted by the server as a state ref.
- `source_key` values for uploaded content should reference
  `syncs/<generation_id>/sources/files/<path>` or
  `syncs/<generation_id>/sources/bundles/<bundle_id>` for new clients. Legacy
  `files/<root_id>/...` and `bundles/<root_id>/...` refs are still accepted.
- Central org/user ignore policy is enforced during finalize. New content under
  ignored paths returns `400`; removals for ignored paths are allowed so policy
  changes can remove existing indexed rows.
- `base_generation_id`/`seq` must match the root's current visible generation,
  otherwise a [sync conflict](#sync-conflicts) is returned.
- `changes[].status` is one of `ADDED`, `MODIFIED`, `REMOVED`, `MOVED`,
  `RENAMED`, `UNCHANGED` (also defined: `COPIED`, `MOVED_AND_MODIFIED`). For
  moves/renames, `old_path` is required.

**Sync modes:**

- Default (synchronous): runs the pipeline inline and returns `200` with a
  `SyncResponse` only after the generation commits.
- `?async=true`: returns `202 Accepted` immediately with the job/generation IDs;
  poll `GET /roots/{id}/sync/status?job_id=...` until `status` is `completed`
  or `failed`.

Queries read only the latest committed visible generation. While an async sync
job is still in flight, query results continue to come from the previous
committed generation.

`SyncResponse`:

```json
{
  "root_id": "...", "sync_job_id": "...",
  "generation_id": "...", "generation_seq": 8,
  "chunks_added": 12, "chunks_removed": 3, "chunks_moved": 1, "files_processed": 9
}
```

### Sync conflicts

If the client's base generation is stale (someone else committed first), the
server returns `409` with a `SyncConflictResponse`:

```json
{
  "error": "stale sync base generation",
  "client_base_generation_id": "...",
  "client_base_generation_seq": 7,
  "current_generation_id": "...",
  "current_generation_seq": 9
}
```

The expected client behavior is to reload remote state, recompute the diff
against the current generation, and retry. A sync already in progress for the
root also yields `409`.

---

## ACLs

Folder ACLs are **deny-prefix** rules. The only supported `permission` is
`none`, which hides matching path prefixes from search and blocks writes under
them. All ACL routes require **admin role** plus the matching ACL scope.

### `POST /roots/{id}/acls`

```json
{ "path_prefix": "/secret/", "grant_to": "user:<id>|role:<role>|*", "permission": "none" }
```

`permission` defaults to `none`; any other value is rejected with `400`.
Response `201`: the `RootACL`.

### `GET /roots/{id}/acls`

List ACLs for the root. Requires `acl:read`/`acl:write`/`admin`/`read`/`write`.

### `DELETE /roots/{id}/acls/{aclId}`

Delete an ACL. Returns `{"status":"deleted"}`.

---

## Query

### `POST /query`

Search one or more roots. Requires scope `query` / `read` and read access to
every explicitly requested root.

Request (`QueryRequest`):

```json
{ "query": "renewal notice terms", "root_id": "<id>", "mode": "hybrid", "glob": "*.pdf", "top_k": 10 }
```

- `query` is required.
- Exactly one root selector is required:
  - `root_id`: search one root.
  - `root_ids`: search selected roots.
  - `all_roots: true`: search every root the caller can access.
- `mode`: `hybrid` (default), `fts`, or `vector`. Invalid values → `400`.
- `top_k` defaults to `10`. `glob` is optional and filters on `file_path`.

Behavior:

- Results are always constrained to the root's **visible (committed)
  generation**. A root with no committed generation returns no rows. In-flight
  or failed syncs are never exposed.
- Sharded roots are queried across all active namespaces concurrently. Multi-root
  queries repeat that process per root, then merge and truncate globally.
- Denied ACL prefixes are filtered out post-query for each root.
- For `user`-scoped roots, non-admin callers are additionally filtered through
  their stored content proof, so they only receive rows for files they can prove
  they possess.
- Explicitly requested inaccessible roots return `404`. `all_roots` only selects
  roots the caller can access.

Response (`QueryResponse`):

```json
{
  "query": "renewal notice terms",
  "mode": "hybrid",
  "roots_searched": 2,
  "results": [
    {
      "root_id": "<id>",
      "root_name": "contracts",
      "file_path": "contracts/acme.pdf",
      "absolute_path": "/Users/me/workspace/contracts/acme.pdf",
      "chunk_index": 4,
      "content": "...matched text...",
      "file_type": "pdf",
      "page_number": 3,
      "image_path": "chunks/<root>/...png",
      "score": 0.0123
    }
  ]
}
```

`page_number` and `image_path` are present only for page/image-based results.
`score` is the Turbopuffer distance (`$dist`); lower is closer for cosine.

---

## Billing

Active only when Stripe is configured (`ENABLE_BILLING=true` + Stripe secrets);
otherwise these routes return `404`.

| Route | Description |
| --- | --- |
| `GET /billing` | Current org subscription state. |
| `POST /billing/checkout-session` | Create a Stripe checkout session (admins). |
| `POST /billing/webhook` | Stripe webhook receiver (unauthenticated; HMAC-SHA256 verified). |

---

## Platform admin (`/admin/*`)

Require the platform admin key. Used for provisioning, not normal operation.

| Route | Description |
| --- | --- |
| `POST /admin/orgs` | Provision an org. Body: `{id?, name, slug?, external_id?}`. |
| `POST /admin/users` | Provision a user. Body requires `email`. |
| `PUT /admin/orgs/{orgId}/members/{userId}` | Upsert org membership. Body: `{role}`. |
| `POST /admin/orgs/{orgId}/users/{userId}/api-keys` | Create a key for a member. Body: `{name?, scopes?}` (defaults to `["query"]`). |
| `POST /admin/orgs/{orgId}/roots` | Create a root in any org. |
| `DELETE /admin/roots/{id}` | Delete any root (across orgs). |
| `DELETE /admin/orgs/{id}` | Delete an org and all its roots/artifacts. |
| `DELETE /admin/users/{id}` | Delete a user and the roots they own. |

Deletes return `409` while sync jobs are active and report
`turbopuffer_namespaces` and `s3_objects_deleted` on success.

---

## Schemas

### RootMetadata

```json
{
  "id": "string", "org_id": "string", "name": "string",
  "source_path": "string", "scope": "org|user", "owner_user_id": "string?",
  "visible_generation_id": "string", "visible_generation_seq": 0,
  "created_at": "RFC3339", "updated_at": "RFC3339"
}
```

### FileChange (sync)

```json
{
  "path": "string", "absolute_path": "string?",
  "status": "ADDED|MODIFIED|REMOVED|MOVED|RENAMED|UNCHANGED|COPIED|MOVED_AND_MODIFIED",
  "old_path": "string?", "content_hash": "string", "size": 0,
  "source_key": "string?", "source_offset": 0, "source_length": 0
}
```

### SyncJob

```json
{
  "id": "string", "org_id": "string", "root_id": "string", "user_id": "string",
  "status": "running|completed|failed", "total_files": 0, "processed": 0,
  "errors": [ { "error": "..." } ],
  "started_at": "RFC3339", "finished_at": "RFC3339?"
}
```

### RootACL

```json
{
  "id": "string", "org_id": "string", "root_id": "string",
  "path_prefix": "string", "grant_to": "string", "permission": "none",
  "created_at": "RFC3339"
}
```

## Limits

| Limit | Value | Source |
| --- | --- | --- |
| Single file upload | 512 MiB | `handleUpload` |
| Bundle upload | 1024 MiB | `handleUploadBundle` |
| Default `top_k` | 10 | `handleQuery` |
| Sync job timeout | 30 min (configurable via `PUFFERFS_SYNC_JOB_TIMEOUT`) | `syncJobTimeout` |
| Namespace shards per root | 1 default, 256 max | `PUFFERFS_TP_NAMESPACE_SHARDS` |

See [configuration.md](./configuration.md) for tunables.
