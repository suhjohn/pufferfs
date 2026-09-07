# PufferFS HTTP API Reference

This reference documents the PufferFS API server's HTTP surface as implemented
in `internal/server/handlers.go`. It is intended for people who integrate with
PufferFS directly — scripts, agents, and services — rather than through the CLI.

The CLI (`pufferfs`) is a client of this same API; anything the CLI does can be
done over HTTP.

## Conventions

### Processing protocol

The server advertises sync protocol 2. Source packs and per-file registration
are the only ingestion protocol. The retired upload, upload-bundle and
root-generation sync/job endpoints return 404. Deploy a matching CLI, API and
consumer release; never retry through a previous ingestion protocol.

### Per-file multipart source packs

These endpoints require root sync permission and `sync` or `write` scope. They
do not use root generation IDs and send no source bytes through the API server.

- `POST /roots/{id}/sources/multipart/init`: `{request_id, size}`. Persist the
  UUID `request_id` before calling. Returns `{object_key, upload_id, part_size,
  part_count, complete}`; identical retries reuse an active session. Size is
  1..128 MiB, with 16 MiB parts. Reusing a request ID with another size is 409.
  Resume checks S3 session status. A confirmed absent session **and** absent
  completed object returns 409 with `code: source_multipart_expired`. Retain
  the captured pack bytes and capture ID, durably assign a new upload request
  UUID, and discard only the expired upload's key/ID/part acknowledgements.
  A temporary status-check failure is 503; keep the existing upload identity.
  If S3 completion succeeded but the API acknowledgement was lost, the original
  frozen part list plus object identity/size can recover `complete: true`.
- `POST /roots/{id}/sources/multipart/part`: `{object_key, part_number}`.
  Returns `{url, headers, size}` for a direct PUT. Part numbers start at 1.
  Retain the returned upload ETag locally. Call again to renew an expired URL
  before completion starts; do not send API credentials to the signed URL.
- `POST /roots/{id}/sources/multipart/complete`: `{object_key, parts}` where
  each ordered part is `{part_number, etag}`. The API persists the exact list
  before calling S3. Retry the identical list after a transient error. A changed
  list is 409; once sealed, new part URL requests are rejected. Successful
  completion returns `{object_key, status: "complete"}`.

Only after completion may captured versions reference this object's extents.
This API is available for migration development. The opt-in capture CLI uses
it for new packs of at least 32 MiB and persists part acknowledgements locally.
The infrastructure aborts incomplete S3 multipart uploads after one day. The
capture CLI recovers from this using the retained immutable spool. API task
credentials need `s3:ListMultipartUploadParts` as well as the existing upload,
object-read and bucket-list permissions. Real AWS/IAM rollout validation remains
required; the expiry/recovery path is exercised using Compose S3.

### General

`POST /roots/{id}/captured-proofs` accepts `{files: [{path, version_id,
content_hash}]}` for 1..128 unique paths. It records the requesting user's hash
proofs for existing current versions without uploading content or creating
extraction/index work. Root sync permission and applicable path ACLs are required.
Any stale/deleted version or hash mismatch rejects the entire batch with 409.
Success returns `{status: "complete", files: <count>}`. Catalog entries from
`GET /roots/{id}/captured-files` include `proof_current` for the requesting user.
This preserves the existing client-reported hash contract, not a cryptographic
proof-of-possession challenge. The opt-in capture CLI hashes uncached or
unproven local files and uses this endpoint when their bytes match the catalog.

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
| `409 Conflict` | File version conflict, retired source identity, or a deleting root. |
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
- `restricted` roots: readable/writable/deletable through explicit root grants
  to an org, user, or group, plus org admin+ override.

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

Returns the authenticated user, org context, and scopes carried by the current
credential. An empty `scopes` array means the credential is unrestricted.

```json
{ "user": { "id": "...", "email": "...", "name": "..." }, "org_id": "...", "role": "editor", "scopes": ["sync", "query"] }
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

- `scope`: `org` (default), `user`, or `restricted`.
- `org` scope requires editor+.
- `user` scope defaults the owner to the caller; setting another `owner_user_id`
  requires admin+, and the owner must be an org member.
- `restricted` scope requires org admin authority through the normal API and is
  intended for roots exposed by explicit root grants.

Response `201`: the `RootMetadata` object (see [Schemas](#schemas)).

### `GET /roots`

List roots the caller can access. Requires `query` / `sync` / `read` / `write`.
Returns an array of `RootMetadata`.

### `GET /roots/{id}`

Get one root. `404` if it does not exist or the caller cannot read it.

### `DELETE /roots/{id}`

Delete a root and all its PufferFS artifacts. Requires
`root:delete` / `delete` / `write` **and** delete rights on the root
(admin+ for org roots; owner or admin+ for user roots; grant/admin access for
restricted roots).

- Active sync jobs are atomically failed before cleanup. New work for the root
  is rejected, queued work is discarded, and workers that were already running
  repeat full root cleanup after they finish so late writes cannot recreate
  deleted storage objects or index namespaces.
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

### `POST /roots/{id}/read`

Read a deterministic slice from one known file. Requires scope `query` / `read`
and read access to the root/path. This is not search; use it when the caller
already knows the file path and wants a page or line range.

Request:

```json
{
  "path": "docs/manual.pdf",
  "pages": { "start": 10, "end": 12 }
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

- Page reads assemble all indexed chunks with the requested `page_number`.
- Generated image downloads and `include_images` are no longer supported.
- Line reads require chunks indexed with `line_start` / `line_end`; files synced
  before that metadata existed may need to be resynced.
- ACL and user-root content-proof filtering match query behavior. Reads use
  the catalog's published extraction and return 404 for unpublished or deleted
  files.

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
      "content": "...page text..."
    }
  ]
}
```

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

Targets accept `user:<id>`, `role:<role>` and `*`; historical bare user IDs
remain supported. Prefixes are folders and normalized with a trailing slash.

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

- Captured files expose only their published extraction. Capturing or extracting
  a new version leaves the previous publication searchable until the new
  extraction is published.
- Search validates candidate paths against the catalog and retries after excluding
  obsolete/unpublished extractions. Each file's first observed publication is
  pinned for that request; this is not an atomic whole-root snapshot. Single-file
  reads likewise retain one file publication through pagination.
- Sharded roots are queried across all active namespaces concurrently. Multi-root
  queries repeat that process per root, then merge and truncate globally. Both
  hybrid rank lists are validated before rank fusion.
- Denied ACL prefixes are filtered out post-query for each root.
- For `user`-scoped roots, non-admin callers are additionally filtered through
  their stored content proof, so they only receive rows for files they can prove
  they possess.
- Explicitly requested inaccessible roots return `404`. `all_roots` only selects
  roots the caller can access.
- If publication validation exhausts its bounds, the response is `503` with code
  `search_publication_busy` and `Retry-After: 1`, not a successful partial result.
  Retry after indexing/cleanup progresses. Each root's index/database search has
  a 30-second budget; its timeout returns `504`. These limits do not cover the
  earlier query-embedding call or make a multi-root request a 30-second operation.
- ACL lookup failures return an error rather than granting access.

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
      "score": 0.0123
    }
  ]
}
```

`page_number` is present for page-based results. Image storage paths are not returned.
For vector search, `score` is the provider's cosine distance and smaller values
rank first across all roots and shards. For FTS in one shard, `score` is the
provider's BM25 score (higher ranks first). Hybrid search and FTS across shards
use reciprocal rank scores (higher ranks first). Scores across these modes are
not comparable.

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
The same routes can be called from the CLI with `pufferfs admin ...` by passing
`--admin-key` or setting `PUFFERFS_ADMIN_API_KEY`.

| Route | Description |
| --- | --- |
| `POST /admin/orgs` | Provision an org. Body: `{id?, name, slug?, external_id?}`. |
| `POST /admin/users` | Provision a user. Body requires `email`. |
| `PUT /admin/orgs/{orgId}/members/{userId}` | Upsert org membership. Body: `{role}`. |
| `POST /admin/orgs/{orgId}/groups` | Create/upsert a group. Body: `{id?, name, external_id?}`. |
| `GET /admin/orgs/{orgId}/groups` | List groups. |
| `GET /admin/orgs/{orgId}/groups/{groupId}/members` | List group members. |
| `PUT /admin/orgs/{orgId}/groups/{groupId}/members/{userId}` | Add a group member. User must already be an org member. |
| `DELETE /admin/orgs/{orgId}/groups/{groupId}/members/{userId}` | Remove a group member. |
| `POST /admin/orgs/{orgId}/users/{userId}/api-keys` | Create a key for a member. Body: `{name?, scopes?}` (defaults to `["query"]`). |
| `POST /admin/orgs/{orgId}/roots` | Create a root in any org, including `restricted` roots. |
| `POST /admin/orgs/{orgId}/roots/{rootId}/grants` | Create/upsert a root grant. Body: `{principal_type:"org|user|group", principal_id, permissions:["read"|"sync"|"delete"|"admin"]}`. |
| `GET /admin/orgs/{orgId}/roots/{rootId}/grants` | List root grants. |
| `DELETE /admin/orgs/{orgId}/roots/{rootId}/grants/{grantId}` | Delete a root grant. |
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
  "source_path": "string", "scope": "org|user|restricted", "owner_user_id": "string?",
  "access": ["read", "sync"], "access_source": "org|owner|role|user|group",
  "created_at": "RFC3339", "updated_at": "RFC3339"
}
```

### Group

```json
{
  "id": "string", "org_id": "string", "name": "string",
  "external_id": "string?", "created_at": "RFC3339", "updated_at": "RFC3339"
}
```

### RootGrant

```json
{
  "id": "string", "org_id": "string", "root_id": "string",
  "principal_type": "org|user|group", "principal_id": "string",
  "permissions": ["read", "sync", "delete", "admin"],
  "created_at": "RFC3339", "updated_at": "RFC3339"
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

| Limit | Value |
| --- | --- |
| Registered files per capture | 128 |
| Source pack | 1–128 MiB |
| Catalog page | 1–1000 files; default 500 |
| Capture request body | 4 MiB |
| Read range | At most 1000 lines or pages per request |
| Read response content | 32 MiB; request smaller ranges above this |
| Default query top_k | 10 |
| Namespace shards per root | 1 default, 256 max |

## Per-file capture catalog

Add `processing=true` to include each file's latest registered extraction status.
The optional `processing` object contains `extraction_id`, `revision`, `stage`,
`status`, `attempt_count`, `acknowledged_batches`, and optional
`mutation_batch_count`. States are `pending`, `running`, `waiting_provider`,
`complete`, `failed`, `superseded`, `missing`, or `inconsistent`. `complete`
requires that exact extraction to be published; matching captured/indexed file
version IDs alone is insufficient when a newer extraction revision is pending.
`missing`/`inconsistent` indicate incomplete catalog/publication metadata, not
successful indexing. Raw worker errors, artifact bodies, and provider payloads
are not exposed. These are live pages, not an atomic whole-root snapshot.

`acknowledged_batches` is durably recorded progress. Current index workers update
it when all mutation batches finish, in the same transaction that completes or
supersedes the work. While running, it remains zero even after the provider
has accepted some batches. A retry replays the full immutable artifact.

The status option uses the same pagination, root sync permission, and path ACLs
as ordinary catalog reads. It reads only Postgres. Omit the option (or use
`processing=false`) for the cheaper capture-bootstrap metadata query.

`GET /roots/{id}/captured-files?limit=500&cursor=<file-id>`

Requires sync/write scope, root sync permission and existing path ACL access.
Returns `files` and optional `next_cursor`. Limit is 1–1000; default 500. Each
entry contains `file_id`, `path`, captured `version_id`/`sequence`,
`indexed_version_id`, `content_hash`, `size`, `deleted` and
`source_manifest_ref`. No source/chunk/vector bodies are returned. Treat this
reference as opaque in clients. It is a packed locator ending in
`.jsonl#OFFSET:LENGTH:RECORD_SHA256` and identifies a checksummed byte range, not an S3 key including the fragment.
Application reads should continue through the read API.

Continue while `next_cursor` is present, including when `files` is empty after
ACL filtering. Pagination is a live catalog scan, not a root-wide snapshot;
version registration must still send each file's `previous_version_id` to
detect concurrent changes. Tombstones remain available for safe path recreation.
Capture acceptance does not imply indexing completion.
