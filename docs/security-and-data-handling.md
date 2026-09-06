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
| Object storage (S3-compatible) | Immutable originals and source manifests, extracted text chunks, vector packs and replayable mutations | Data |
| PostgreSQL | Tenants, credentials, permissions, file versions, extraction/work metadata, source extents, provider submissions and cleanup records | Control |
| Turbopuffer | Searchable text, file/chunk/location metadata and optional vectors | Index |
| Transformation/index/query workers | Temporary captured data, rendered images, audio clips and inference buffers | Compute |
| Gemini Batch | Temporary image/audio inputs and parsing/transcription outputs | External provider |

Source and extraction keys are scoped by organization and root. Signed uploads
authorize one immutable object, not a bucket or a tenant prefix. Multipart
completion verifies upload identity and size. Registration checks uploader,
capture, path permissions and exact reusable source extents under the root lock;
knowledge of another file's packed-object key is insufficient.

Generated images and converted media never enter S3. Original images are
ordinary retained input bytes. Gemini uploads have explicit cleanup tracking;
a missing provider file is not reported as a confirmed deletion without evidence.

Originals and derived data leave the device. Include S3, Postgres, worker
hosting, Gemini and Turbopuffer in the data-sensitivity assessment. Current heads,
retries and append dependencies prevent premature retention cleanup.

## Authentication

Three credential types:

1. **Tenant API keys** — `Authorization: Bearer pfs_sk_...`. Stored only as
   SHA-256 hashes; the raw key is shown once at creation and never retrievable
   afterward. Resolved to an org, user, role, and scope set. Newly created
   user keys must include an explicit non-empty scope list.
2. **Session JWTs** — HS256, signed with `JWT_SECRET`, 24-hour TTL. Carried in
   the `Authorization` header or the `pf_session` httpOnly cookie. Issued by
   login providers such as email-code and Google OAuth. OAuth callbacks require
   signed state bound to a short-lived httpOnly state cookie; email-code
   challenges are short-lived, attempt-limited, and stored as HMAC hashes.
3. **Platform admin key** — a separate key for `/admin/*`, compared in constant
   time against `PUFFERFS_ADMIN_KEY_HASH`. If unset, all admin routes return
   `403`.

Unauthenticated routes are limited to health checks, `GET /cli/version`, the
login endpoints, and the Stripe webhook (which is instead verified by
signature).

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
- **`restricted` roots**: only explicit root grants can read/write/delete, plus
  org admin+ override.

Unreadable roots return `404` (not `403`), so callers cannot probe for the
existence of roots they cannot access.

Root grants assign root-level `read`, `sync`, `delete`, or `admin` permissions
to an org, user, or group. They are evaluated before folder ACL deny-prefix
rules.

### 4. Folder ACLs (deny-prefix)

ACLs are modeled with a `permission` field, but the implemented behavior is
**deny-only**: the only accepted value is `none`, which denies a path prefix.

- On **query**, denied prefixes are filtered out of results
  (`/` + `file_path` prefix match).
- On **write/sync**, a change under a denied prefix is rejected.
- With no ACLs configured, all org members can read and editor+ can write,
  subject to the role/scope rules above.

There is currently **no positive folder ACL** that narrows default access; ACLs
subtract from, not add to, the role/scope/root-grant baseline.

### 5. Content-proof filtering (user roots)

For `user`-scoped roots, non-admin callers' query results are additionally
filtered by per-file path/hash proof records. Missing, deleted or mismatched
proofs do not fall back to old root-wide proofs. This is a client-reported hash
check, not a cryptographic proof-of-possession challenge; all tenant/root/path
authorization still applies. Org roots skip this additional hash filter.

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
> guarantee, and pair it with server-managed org/user ignore policies,
> `.gitignore`/`.tpfsignore`/global-ignore rules, and ACL deny prefixes for
> anything sensitive.

PufferFS also honors built-in ignores, server-managed org/user ignore policies,
`.gitignore`, `.tpfsignore` (root), and `~/.tpfs/.tpfsignore` (global). Org/user
policies are enforced by the server during capture registration; local ignore files are
CLI-side filtering for the syncing machine.

## Query-result correctness and isolation

Search validates ranked candidates against per-file catalog publications before
returning results; rejected extraction identities are excluded and re-queried.
Reads pin one publication across pagination. Pending/superseded mutations are
not exposed. There is no root-generation or mixed-schema fallback.

Tenant/root permissions, deny-prefix ACLs and user-root hash proofs still apply
to every result, including multi-root retrieval. Permission or publication
lookup failures fail closed.

## Data lifecycle and deletion

Root deletion marks the root unavailable and preserves cleanup targets before
removing catalog rows. Late worker writes are handled by repeated scheduled
cleanup of source/extraction/mutation prefixes and index namespaces. Historical
prefixes remain on the cleanup list for upgrades. Local source files are never
deleted by PufferFS root deletion.

Original packs, derived artifacts and vector caches have separate retention
policies. Current heads, append dependencies, retry state, active leases and
provider submissions protect reachable data from premature cleanup.
Postgres stores vector locators, not vector bodies.

S3 versioning, backups, provider operational records and already-exported logs
need their own retention policy; API deletion alone is not evidence of physical
erasure from those systems. See [configuration](configuration.md) and
[E2E limitations](../tests/e2e/README.md).

## Vulnerability disclosure

Report security issues to `security@pufferfs.com`. Include affected routes or
commands, reproduction steps, impact, and any relevant logs or request IDs.

Good-faith testing is in scope when it avoids data destruction, service
disruption, spam, social engineering, and access to other users' data. Do not
exfiltrate data beyond what is necessary to prove the issue.

## Known caveats and hardening notes

These are real, in-code limitations to account for in a deployment:

- **Folder ACLs are deny-only.** Do not rely on folder ACLs to grant root
  access; they can only subtract prefixes. Use root grants for root-level
  org/user/group sharing.
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
- [ ] Restrict object-store, Postgres and Turbopuffer access to authorized
      API/worker roles and scoped signed uploads; they hold originals,
      extracted content, catalog records and vectors.
- [ ] Use ACL deny prefixes and ignore files for sensitive subtrees; do not rely
      on filename-based secret filtering alone.
- [x] Harden OAuth state before exposing Google login publicly.
