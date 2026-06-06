# Security and Data Handling

This document describes how PufferFS authenticates callers, enforces access,
stores data, and handles sensitive material. It is aimed at security reviewers,
buyers' security teams, and operators. Behavior described here reflects the
implementation in `internal/auth`, `internal/server`, and `internal/ignore`.

## What PufferFS stores, and where

The local folder you sync remains the source of truth. To answer queries,
PufferFS stores derived copies and metadata across four systems:

| System | Holds | Plane |
| --- | --- | --- |
| Object storage (S3-compatible) | Uploaded source file copies, packed transport bundles, gzipped root state snapshots, sync artifacts, and rendered page/OCR images. | Data |
| PostgreSQL | Orgs, users, API key hashes, roots, ACLs, sync jobs/generations, root state refs, embedding cache, content proofs, subscriptions. | Control |
| Turbopuffer | Search index rows: extracted content, path metadata, file/chunk hashes, file type, generation metadata, and embedding vectors. | Index |
| Modal | Transient compute for chunking, OCR/vision extraction, and embeddings. | Compute |

Object storage object layout (by prefix): `files/<rootID>/...`,
`bundles/<rootID>/...`, `states/<rootID>/...`, `syncs/<generationID>/...`,
`chunks/<rootID>/...`.

> **Implication for reviewers:** extracted document content and a copy of source
> bytes leave the local machine. Treat the server, its object store, Postgres,
> Turbopuffer, and Modal as in-scope for any data-sensitivity assessment.

## Authentication

Three credential types:

1. **Tenant API keys** — `Authorization: Bearer pfs_sk_...`. Stored only as
   SHA-256 hashes; the raw key is shown once at creation and never retrievable
   afterward. Resolved to an org, user, role, and scope set. Newly created
   user keys must include an explicit non-empty scope list.
2. **Session JWTs** — HS256, signed with `JWT_SECRET`, 24-hour TTL. Carried in
   the `Authorization` header or the `pf_session` httpOnly cookie. Issued by the
   Google OAuth flow. OAuth callbacks require signed state bound to a
   short-lived httpOnly state cookie.
3. **Platform admin key** — a separate key for `/admin/*`, compared in constant
   time against `PUFFERFS_ADMIN_KEY_HASH`. If unset, all admin routes return
   `403`.

Unauthenticated routes are limited to health checks, `GET /cli/version`, the
OAuth endpoints, and the Stripe webhook (which is instead verified by signature).

### Session cookie properties

The `pf_session` cookie is `HttpOnly`, `SameSite=Lax`, `Path=/`, with `Domain`
set to the registrable domain (`COOKIE_DOMAIN`) so the app and API subdomains
can share it. `Secure` must be enabled whenever the site is served over HTTPS
(set via the cookie config). CORS allows credentialed requests only from
explicitly configured origins; with no origins set, CORS is a no-op.

## Authorization

Authorization is enforced in layers; a request must pass **all** that apply.

### 1. API key scopes

A scoped key must present the required scope, an accepted alias, or `*`. Keys
with no scopes (and all JWT sessions) are treated as unrestricted. Scopes seen
in the code: `sync`, `query`, `root:create`, `root:delete`, `api_keys:read`,
`api_keys:write`, `acl:read`, `acl:write`, `org:admin`, and coarse aliases
`read` / `write` / `admin` / `delete`.

> New user-created keys reject empty scope lists. Legacy empty-scope keys are
> still treated as unrestricted for compatibility; rotate them to explicit
> least-privilege scopes (e.g. `["query"]` for a read-only agent key).

### 2. Org roles

`owner (4) > admin (3) > editor (2) > viewer (1)`. Role gates membership
changes, ACL management (admin+), org-root writes (editor+), and org-root
deletes (admin+).

### 3. Root scope and ownership

- **`org` roots**: any org member can read; editor+ can write; admin+ can
  delete.
- **`user` roots**: only the owner or an org admin+ can read, write, or delete.

Unreadable roots return `404` (not `403`), so callers cannot probe for the
existence of roots they cannot access.

### 4. Folder ACLs (deny-prefix)

ACLs are modeled with a `permission` field, but the implemented behavior is
**deny-only**: the only accepted value is `none`, which denies a path prefix.

- On **query**, denied prefixes are filtered out of results
  (`/` + `file_path` prefix match).
- On **write/sync**, a change under a denied prefix is rejected.
- With no ACLs configured, all org members can read and editor+ can write,
  subject to the role/scope rules above.

There is currently **no positive/grant ACL** that narrows default access; ACLs
subtract from, not add to, the role/scope baseline.

### 5. Content-proof filtering (user roots)

For `user`-scoped roots, non-admin callers' query results are additionally
filtered through a stored **content proof** (a Merkle map of file paths →
hashes). A row is returned only if its `file_path`/`file_hash` appears in the
caller's proof. This means that even with a shared or cloned index, a user only
sees results for files they can prove they currently possess. Org roots
intentionally skip this filter (results are shared by membership + ACL).

## Secret-file handling

Before sync state is built, the CLI's ignore matcher excludes likely secret
files by **filename pattern**:

```
.env            .env.*          *.pem           *.key
*_rsa           id_rsa          id_ed25519      id_ecdsa
credentials.json  service-account*.json
*.p12           *.pfx           .npmrc          .pypirc
```

> **This is filename-based protection, not a content secret scanner.** A secret
> embedded inside a non-matching file (e.g. a hard-coded token in
> `config.yaml`) will be synced and indexed. Treat it as a guardrail, not a
> guarantee, and pair it with `.gitignore`/`.tpfsignore`/global-ignore rules and
> ACL deny prefixes for anything sensitive.

PufferFS also honors built-in ignores plus `.gitignore`, `.tpfsignore` (root),
and `~/.tpfs/ignore` (global) — useful for keeping caches, build outputs, and
dependency folders out of the index and reducing exposure surface.

## Query-result correctness and isolation

- Every query is constrained to the root's **visible (committed) generation**.
  If the visible generation cannot be resolved, the query **fails closed**
  (returns an error) rather than serving unfiltered rows. A root with no
  committed generation matches no rows, so in-flight or failed syncs are never
  exposed.
- Tenancy is enforced by org scoping on every root lookup
  (`id.OrgID == root.OrgID`); cross-org reads are not possible through the normal
  API.
- **Any new query path must reapply the visible-generation filter and the ACL /
  content-proof filters** — these are the load-bearing isolation controls.

## Data lifecycle and deletion

- **Root deletion** (`DELETE /roots/{id}` or admin equivalent) removes the
  root's Turbopuffer namespaces and all S3 objects under `files/`, `bundles/`,
  `states/`, `chunks/`, and `syncs/` for its generations, plus the local
  `~/.tpfs/roots/<id>/` cache. **It does not delete source files on the user's
  machine.**
- Deletion is **blocked (`409`) while sync jobs are active**; org/user deletes
  cascade to owned roots under the same rule.
- Deletion does not imply deletion of already exported logs, provider billing
  records, or vendor-side operational records unless those are covered by the
  deployment's separate retention process.
- Failed or partial generations are cleaned up and never become visible to
  queries.
- The embedding cache is keyed by org + model version + content hash; bumping
  `PUFFERFS_EMBEDDING_MODEL_VERSION` effectively invalidates stale cached
  vectors.

## Vulnerability disclosure

Report security issues to `security@pufferfs.com`. Include affected routes or
commands, reproduction steps, impact, and any relevant logs or request IDs.

Good-faith testing is in scope when it avoids data destruction, service
disruption, spam, social engineering, and access to other users' data. Do not
exfiltrate data beyond what is necessary to prove the issue.

## Known caveats and hardening notes

These are real, in-code limitations to account for in a deployment:

- **ACLs are deny-only.** Do not rely on ACLs to *grant* narrower-than-default
  access; they can only subtract prefixes.
- **Legacy empty-scope API keys are unrestricted.** New user-created keys
  require explicit scopes, but old empty-scope keys should be rotated.
- **Secret filtering is filename-based** (see above).
- **JWT secret and admin key are the crown jewels.** Compromise of `JWT_SECRET`
  forges any session; compromise of the admin key grants cross-org
  provisioning/deletion. Store both in a secrets manager, rotate on exposure,
  and prefer the hashed admin-key form.

## Operator checklist

- [ ] Serve over HTTPS and ensure the session cookie is marked `Secure`.
- [ ] Set a strong, unique `JWT_SECRET` from a secrets manager.
- [ ] Configure the admin key via `PUFFERFS_ADMIN_KEY_HASH` (not plaintext), or
      leave it unset to disable admin routes entirely.
- [ ] Restrict CORS origins to your known web app origin(s).
- [ ] Issue API keys with explicit least-privilege scopes; rotate regularly.
- [ ] Lock down the object store, Postgres, and Turbopuffer to the server's
      network; they hold source copies, extracted content, and vectors.
- [ ] Use ACL deny prefixes and ignore files for sensitive subtrees; do not rely
      on filename-based secret filtering alone.
- [ ] Harden OAuth state before exposing Google login publicly.
