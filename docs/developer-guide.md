# Developer Guide

This guide explains how developers should use PufferFS and what behavior to
expect from sync, search, indexing, and background operation. It is written for
people integrating PufferFS into local workflows, scripts, internal tools, or
AI-agent systems.

## Installation

### macOS / Linux (installer script)

Works on both macOS and Linux (`amd64` and `arm64`). Downloads the latest release, verifies checksums, and installs to `/usr/local/bin`:

```sh
curl -fsSL https://pufferfs.com/install.sh | sh
```

Pin a version or change the install directory:

```sh
curl -fsSL https://pufferfs.com/install.sh | PUFFERFS_VERSION=0.2.1 INSTALL_DIR="$HOME/.local/bin" sh
```

### Docker / CI

Use the installer script in a `Dockerfile` or CI step:

```dockerfile
RUN curl -fsSL https://pufferfs.com/install.sh | sh
```

Or in a GitHub Actions step:

```yaml
- name: Install PufferFS CLI
  run: curl -fsSL https://pufferfs.com/install.sh | sh
```

### From source (development)

```sh
go install github.com/pufferfs/pufferfs/cmd/pufferfs@latest
```

Verify the CLI is available:

```sh
pufferfs --version
```

After installation, configure the CLI with a server URL and API key:

```sh
pufferfs init
```

## Mental Model

PufferFS takes a local folder and turns it into a searchable root.

The usual flow is:

1. Configure the CLI with a PufferFS server URL and API key.
2. Sync a local folder.
3. Query the indexed contents by natural language, keyword, or hybrid search.
4. Optionally keep the folder current with `sync --follow` or a background service.

A root is the durable unit of sync and access control. The local folder remains
the source of truth. PufferFS stores temporary uploaded copies during sync, then
keeps extracted chunks, embeddings, state snapshots, and search index rows so it
can answer queries.
Deleting a root removes PufferFS artifacts and index metadata, not local source
files.

## Configuration

Run:

```sh
pufferfs init
```

The CLI reads `~/.tpfs/config.toml`.

The most common settings are:

```toml
[server]
url = "https://api.example.com"
api_key = "pfs_sk_..."
```

Environment variables override config values:

```sh
export PUFFERFS_SERVER_URL="https://api.example.com"
export PUFFERFS_API_KEY="pfs_sk_..."
```

The CLI also stores per-root local cache under:

```text
~/.tpfs/roots/<root-id>/
```

That cache includes root metadata, flat file state, and a Merkle tree snapshot.

## Syncing a Folder

Dry-run first when trying a new root:

```sh
pufferfs sync ./workspace --dry-run
```

Create or update a root:

```sh
pufferfs sync ./workspace --name workspace
```

Update selected files inside an existing root:

```sh
pufferfs sync --root /Users/me/workspace --include "docs/**" --name workspace
pufferfs sync --root /Users/me/workspace --include "docs/**" --exclude "docs/archive/**" --name workspace
```

Force a re-sync and reindex when committed root state already matches local
files but chunking/index propagation needs repair:

```sh
pufferfs sync --root /Users/me/workspace --force
pufferfs sync --root /Users/me/workspace --include "docs/**" --force
```

Create a user-owned root:

```sh
pufferfs sync ./workspace --name workspace --scope user
```

Return machine-readable output:

```sh
pufferfs sync ./workspace --json
```

Start a sync job without waiting for it to commit:

```sh
pufferfs sync ./workspace --name workspace --background
# --detach is an alias for --background
```

Inspect or wait for a sync job:

```sh
pufferfs sync status --root workspace
pufferfs sync status --root workspace --job-id <sync-job-id> --json
pufferfs sync jobs --root workspace
pufferfs sync wait --root workspace --job-id <sync-job-id>
```

Wait until selected local files are visible in the latest committed generation:

```sh
pufferfs sync wait --root /Users/me/workspace --include "docs/policy.md"
pufferfs sync wait --root /Users/me/workspace --include "docs/**" --exclude "docs/archive/**"
pufferfs sync wait --root workspace --include "docs/**" --json
```

What to expect:

- The CLI hashes the folder and builds a Merkle tree.
- It compares the current tree to local cache when possible.
- If local cache is stale relative to the server, it fetches remote state and
  diffs against that.
- It uploads only changed content.
- Small files are packed into bundle objects; large and empty files are
  uploaded individually.
- With `--root <path>` and no subset flags, the CLI syncs that folder as the
  full root. If `--name` is omitted, the root name defaults to the directory
  basename.
- With `--include <glob>` and optional `--exclude <glob>`, the CLI syncs a
  subset. Multiple includes are additive, excludes win, and selected changes are
  patched into the current committed root state so unselected files stay visible
  in existing roots.
- With `--force`, the CLI uploads and reindexes the current files even when the
  committed root state already has matching size/content hashes. For full-root
  sync, all current files are treated as modified and deleted paths are closed.
  For subset sync, only selected files are forced. `--force` is intended for
  one-shot recovery and cannot be combined with `--follow`.
- Uploaded source objects are temporary transport for the sync generation. They
  are removed after the generation commits, is aborted, is rejected, fails, or
  expires incomplete.
- The server creates a sync job and a new generation.
- The index is not visible to queries until the generation commits.
- By default, the CLI polls async sync jobs until completion. With
  `--background`/`--detach`, it prints the `sync_job_id` and exits; use
  `pufferfs sync status`, `pufferfs sync jobs`, or `pufferfs sync wait` to
  inspect completion.
- `pufferfs sync wait --include ...` is different from job wait: it hashes the
  current local files matching the same root-relative include/exclude semantics
  as subset sync, fetches the latest committed root state, and returns only when
  every selected local file has the same size and `sha256:` content hash in the
  visible generation. This works with `sync --follow` and installed services
  because it waits for committed state, not for a particular process.
- If the server generation changed during sync, the CLI reloads state,
  recomputes the diff, and retries.

## What Gets Synced

PufferFS decides which files to sync (and index) by evaluating a layered set of
ignore rules. Anything matched by an ignore rule is excluded from the Merkle
tree, the diff, the upload, and the search index.

Ignore matching combines server-managed policy and local CLI rules. Organization
and user ignore policies are fetched from the server before scanning and are also
enforced by the server during sync finalize. Local `.gitignore`, `.tpfsignore`,
and `~/.tpfs/.tpfsignore` files live on the syncing machine, so direct API
clients must apply equivalent local filtering themselves before calling
`POST /roots/{id}/sync`. The server still enforces authentication, org/user
ignore policy, write ACLs, protocol validation, and upload limits.

For subset updates, use `pufferfs sync --root <root-path> --include <glob>`.
`--include` and `--exclude` are root-relative globs; repeated includes are OR'd
together and excludes win. This mode hashes and uploads only selected files, but
it still sends a complete merged root state to the server; direct API clients
must do the same to preserve unselected files.

### Ignore rule sources (in evaluation order)

| Source | Scope | Format |
| --- | --- | --- |
| **Always ignored** | All projects | Hard-coded (`.git`) |
| **Built-in defaults** | All projects | Hard-coded list (see below) |
| **Secret-file patterns** | All projects | Filename glob (see below) |
| **Org ignore policy** | Every root in the org | Gitignore syntax, server-managed |
| **User ignore policy** | Syncs by the current user in the org | Gitignore syntax, server-managed |
| **`~/.tpfs/.tpfsignore`** | All projects for the current local machine user | gitignore syntax |
| **`.gitignore`** | Directory where the file lives (recursive) | [gitignore syntax](https://git-scm.com/docs/gitignore) |
| **`.tpfsignore`** | Directory where the file lives (recursive) | gitignore syntax |

A file is excluded if **any** source matches it. There is currently no negation
or override mechanism across sources (though negation patterns such as `!keep`
work within a single gitignore-syntax file).

### User-defined ignore files

#### Server-managed org and user policies

Use server-managed policies for rules that should apply before upload and should
also be enforced against direct API clients.

```sh
pufferfs ignore get --level effective
pufferfs ignore get --level user
pufferfs ignore set --level user --file ~/.tpfs/user.tpfsignore
pufferfs ignore edit --level user

pufferfs ignore get --level org
pufferfs ignore set --level org --file org.tpfsignore
pufferfs ignore edit --level org
```

Org policy applies to every root in the organization and requires org admin
permission to change. User policy applies to syncs performed by the current user
inside that organization. These policies are additive: a lower layer cannot
un-ignore a path ignored by org or user policy.

#### `.tpfsignore` (project-level)

Place a `.tpfsignore` file in any directory of your project. Patterns in that
file apply to the directory tree rooted at the file's location, using standard
[gitignore pattern syntax](https://git-scm.com/docs/gitignore#_pattern_format).

```text
# my-project/.tpfsignore
# Ignore all CSV data files in this directory tree
*.csv

# Ignore the local scratch folder
scratch/

# Ignore generated API client
generated/client/
```

You can place `.tpfsignore` files in subdirectories for scoped rules:

```text
# my-project/data/.tpfsignore
# Only applies inside my-project/data/
*.parquet
*.arrow
```

#### `~/.tpfs/.tpfsignore` (global)

The global ignore file at `~/.tpfs/.tpfsignore` applies to **every** project synced
by the current user. Use it for machine-specific patterns that should never be
synced regardless of project.

```text
# ~/.tpfs/.tpfsignore
# Editor swap/backup files
*.swp
*.swo
*~

# OS metadata
.Spotlight-V100
.Trashes
```

#### `.gitignore` (project-level, also respected)

PufferFS loads `.gitignore` files from every directory in the tree. Patterns are
scoped to the directory where the file lives, same as Git. If your project
already has a comprehensive `.gitignore`, PufferFS inherits those rules
automatically — no extra configuration needed.

### Built-in default ignores

These paths are always excluded without any configuration:

| Pattern | Reason |
| --- | --- |
| `.git` | Version control internals (always hard-excluded) |
| `node_modules` | JavaScript dependencies |
| `__pycache__` | Python bytecode cache |
| `.venv` / `venv` | Python virtual environments |
| `dist` / `build` | Build outputs |
| `.tpfs` | PufferFS local state |
| `.next` / `.nuxt` | Framework build caches |
| `.cache` | Generic cache directory |
| `.DS_Store` / `Thumbs.db` | OS metadata |
| `*.pyc` / `*.pyo` | Python compiled files |
| `*.o` / `*.so` / `*.dylib` | Native object files / shared libraries |
| `*.class` | Java class files |

### Secret-file patterns

These filenames are unconditionally excluded to prevent accidental sync of
credentials. This is a guardrail, not a full secret scanner — secrets embedded
inside non-matching files (e.g. a token in `config.yaml`) will still sync.

| Pattern | Examples matched |
| --- | --- |
| `.env` / `.env.*` | `.env`, `.env.local`, `.env.production` |
| `*.pem` / `*.key` | `server.pem`, `private.key` |
| `*_rsa` / `id_rsa` / `id_ed25519` / `id_ecdsa` | SSH private keys |
| `credentials.json` | GCP service account |
| `service-account*.json` | `service-account-prod.json` |
| `*.p12` / `*.pfx` | Certificate bundles |
| `.npmrc` / `.pypirc` | Package-manager auth configs |

### Verifying ignored patterns

Use `--dry-run` to preview what would be synced and see which patterns are
active:

```sh
pufferfs sync --dry-run .
```

The dry-run output lists detected secret files and active ignore patterns
without uploading anything.

### Interaction With `sync --follow`

`sync --follow` applies the same ignore matcher when setting up filesystem
watchers. Directories matching ignore rules are not watched, reducing system
resource usage and eliminating noise from dependency installs or build outputs.

## File Changes

The sync model understands:

- Added files.
- Modified files.
- Removed files.
- Moved files.
- Renamed files.
- Unchanged files.

Move and rename detection is content-hash based. For moved files, PufferFS can
reuse existing indexed row metadata when safe, which avoids unnecessary
re-chunking and re-embedding.

## Search

Basic query:

```sh
pufferfs query "renewal notice terms"
```

Search a specific root by name or ID:

```sh
pufferfs query "pricing assumptions" --root workspace
```

Search selected roots or every accessible root:

```sh
pufferfs query "renewal notice" --root contracts --root handbook
pufferfs query "renewal notice" --all-roots
```

Choose search mode:

```sh
pufferfs query "customer SSO notes" --mode hybrid
pufferfs query "exact function or phrase" --mode fts
pufferfs query "documents about onboarding risk" --mode vector
```

Filter by file path:

```sh
pufferfs query "retention policy" --glob "*.pdf"
pufferfs query "database migration" --glob "*.sql"
```

Return JSON:

```sh
pufferfs query "contract renewal" --json
```

Search modes:

- `fts`: full-text BM25 search over extracted content.
- `vector`: semantic search using query embeddings.
- `hybrid`: runs vector and full-text search, then merges results with
  reciprocal rank fusion.

What to expect:

- Query results are filtered to the root's latest committed generation.
- In-progress sync data is not returned.
- Results can include file path, absolute path, chunk index, file type, content,
  page number, image path, and score.
- If the current working directory is inside a synced root, the CLI can
  auto-detect the root.

## File Type Behavior

Text-like files can be chunked locally by the Go server. PDFs, Office files,
presentations, images, structured files, and media files use Modal compute when
configured. The full extraction and chunking process is documented in
[File Ingestion and Chunking](file-ingestion-and-chunking.md).

Expected extraction behavior:

- Code and config files are split into overlapping text chunks.
- Markdown and text are split by headings and text boundaries where possible.
- PDFs are rendered by page and sent through vision extraction by default.
- Native PDF text is retained only as a no-vision fallback.
- Word and PowerPoint files are converted to PDF first, then processed by page.
- Images can be captioned or text-extracted when vision extraction is available.
- Email, calendar, and contact files are parsed into searchable text records.
- Audio and video are split into overlapping time windows and described for
  semantic search.

Page-based document results may include page numbers and image artifact paths.

## Continuous Sync

Run a foreground watcher:

```sh
pufferfs sync ./workspace --name workspace --follow
```

Useful options:

```sh
pufferfs sync ./workspace --follow --debounce 3s
pufferfs sync ./workspace --follow --max-backoff 2m
pufferfs sync ./workspace --follow --max-same-failures 10
```

What to expect:

- PufferFS runs an initial sync.
- It watches filesystem events with a debounce timer.
- It retries transient sync failures with backoff.
- It exits on repeated identical failures after the configured threshold.
- New directories are added to the watcher when created.

## Background Services

Install a supervised user service:

```sh
pufferfs service install ./workspace --name workspace
pufferfs service start workspace
```

Manage it:

```sh
pufferfs service status workspace
pufferfs service logs workspace
pufferfs service restart workspace
pufferfs service stop workspace
pufferfs service uninstall workspace
```

What to expect:

- macOS uses `launchd`.
- Linux uses `systemd --user`.
- The service runs `pufferfs sync <path> --follow`.
- Logs are captured by the operating system service manager.

## Root Management

Show the root for your current directory:

```sh
pufferfs root current
```

Delete the root for your current directory:

```sh
pufferfs root delete
```

Delete a specific root by name or ID:

```sh
pufferfs root delete workspace
```

Skip confirmation. With no root argument this deletes the current directory's
root; with an argument it deletes that specific root:

```sh
pufferfs root delete --yes
pufferfs root delete workspace --yes
```

What to expect:

- When no root is supplied, the CLI detects the root containing the current
  working directory from the local `.tpfs` metadata.
- Without `--yes`, the confirmation prompt requires the root ID even when the
  root was detected from the current directory.
- Root deletion removes PufferFS metadata, any remaining transport/sync
  artifacts, durable state, chunk/page artifacts, and Turbopuffer namespaces.
- Root deletion removes the local PufferFS cache for that root.
- It does not delete local source files.
- Roots with active sync jobs cannot be deleted until jobs finish.

## Permissions and Access

What a caller can do depends on:

- API key scopes.
- Organization role.
- Root scope.
- Root ownership.
- Root grants.
- Deny-prefix ACLs.

General expectations:

- Query/read access is enough to list accessible roots and search.
- Sync/write access is needed to create roots, upload files, and sync.
- Org roots require editor-or-higher role to create or write.
- User roots can be written by their owner or org admins.
- Restricted roots are visible only through explicit root grants, plus org
  admin override.
- Root grants can give `read`, `sync`, `delete`, or `admin` access to an org,
  user, or group.
- Org roots require admin-or-higher role to delete.
- User roots can be deleted by their owner or org admins.
- Denied path prefixes are filtered out of search and block writes.

## Using PufferFS With Agents

PufferFS works well as a retrieval layer for agents because it returns focused
chunks instead of entire folders.

Recommended pattern:

1. Keep a folder synced.
2. Let the agent issue targeted queries.
3. Feed only the relevant results into the agent's reasoning or tool workflow.
4. Re-query when the task changes instead of preloading a large corpus.

Good agent queries look like:

```text
where is the customer onboarding checklist
pricing assumptions for enterprise renewal
incident runbook for failed imports
contract language about termination notice
```

Prefer `hybrid` mode as a default. Use `fts` for exact terms, identifiers, or
known phrases. Use `vector` when users are describing concepts and likely do
not know the source wording.

## Operational Expectations

Sync jobs are generation-based:

- A sync builds a new generation.
- Turbopuffer rows may be written before commit.
- Queries only see the latest visible generation.
- Failed or partial generations are not exposed in normal query results.

Queued deployments may use worker stages:

- Chunk.
- Embed.
- Index.
- Commit.
- Cleanup.

Without queued workers, the server can run the same pipeline in-process.

Storage expectations:

- Object storage is the data plane for temporary source transport, durable state
  refs, sync artifacts, and page images.
- PostgreSQL is the control plane plus small durable caches.
- Turbopuffer is the search index.
- Modal is the heavy compute layer for embeddings and document/image
  extraction.

## Troubleshooting

If sync finds no changes:

- Confirm you are syncing the expected path.
- For subset sync, confirm `--include`/`--exclude` patterns are root-relative
  and match the paths you expect.
- Check ignore rules.
- Check whether the local cache already matches the visible generation.

If query returns no results:

- Confirm the root was synced and the sync job completed.
- Try `--mode hybrid`.
- Remove overly narrow `--glob` filters.
- Confirm the API key has query/read access.
- Confirm the caller can read the root and path.

If `sync --follow` does not pick up files:

- Confirm the files are not ignored.
- Confirm the followed directory still exists.
- Use `pufferfs sync wait --root <root-path> --include <path-or-glob>` to check
  whether the latest local file content has reached the committed server state.
- Restart `sync --follow` after large directory moves.

If a sync fails repeatedly:

- Run a normal `pufferfs sync` once to see the direct error.
- Check upload size limits for very large files.
- Check server-side Modal, Turbopuffer, object storage, and queue configuration.
- If Modal embedding returns 500s for text/code chunks, verify the server is not
  sending chunk metadata fields unsupported by the deployed Modal embed schema;
  the server should preserve line metadata in index rows but omit it from the
  embed request payload.
- Check service logs if running as a background service.

## Upgrade Behavior

The CLI can check the configured server's release manifest and print an upgrade
notice at most once per day.

Direct installs can run:

```sh
pufferfs upgrade
```

What to expect:

- Direct upgrades use the public `https://api.pufferfs.com/cli/version`
  manifest unless `--manifest-url` is provided.
- Direct upgrades download a platform archive.
- The archive checksum is verified.
- The current binary is replaced.
- Installed user services can be restarted after upgrade.
