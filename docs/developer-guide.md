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
the source of truth. PufferFS retains captured originals, extracted chunks, embeddings and index rows.
Files become searchable independently after processing.
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

That cache includes root identity, per-file heads and bounded immutable capture spools.

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

Capture returns after durable registration. Inspect or wait for indexing:

```sh
pufferfs sync status --root workspace --json
pufferfs sync wait --root workspace
pufferfs sync wait --root /path/to/workspace --include "docs/**" --exclude "docs/archive/**"
```

The agent discovers files, captures stable bytes into immutable packs, uploads
through signed URLs, then registers versions. Indexing continues asynchronously.
Current captured/indexed heads are separate; unrelated files need not wait for
one another. Subset sync leaves unselected files untouched. Append capture
verifies the old prefix before reusing its remote extents; rewrites/truncations
capture new bytes.

Pending uploads are journaled and resumed. `--force` reindexes unchanged files
and, after an explicit version conflict, preserves the rejected spool while
recapturing current local files. It does not merge concurrent edits. See
[configuration](configuration.md) for exact conflict and retention behavior.

The agent does not produce an atomic snapshot of an actively changing root.
Downstream computation always uses the captured bytes, never a later read of
the live path. Root-generation jobs and background/detach flags are retired.

## What Gets Synced

PufferFS decides which files to sync (and index) by evaluating a layered set of
ignore rules. Anything matched by an ignore rule is excluded from capture, the diff, the upload, and the search index.

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
- Unchanged files.

Moves and renames are captured as a deletion at the old path and a new file
at the destination. Unchanged content can reuse cached embeddings.

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

- Query results are filtered to the root's published file versions.
- In-progress sync data is not returned.
- Results can include file path, absolute path, chunk index, file type, content,
  page number, image path, and score.
- If the current working directory is inside a synced root, the CLI can
  auto-detect the root.

## File Type Behavior

All files are transformed in the independent CPU worker. Text/code/JSONL use
bounded byte-preserving line chunks; spreadsheets preserve cell addresses.
PDF/Word/presentations always render temporary images and use Gemini Batch.
Images use the same vision parsing; media uses temporary audio clips and
best-effort Gemini diarization. Generated images/media are not stored in S3.

See [File Ingestion and Chunking](file-ingestion-and-chunking.md) for every
supported extension, chunk contract and limitation.

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
- Active sync jobs are cancelled automatically. Any queued or already-running
  shard is prevented from committing and repeats root cleanup before it is
  acknowledged.

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

Capture acceptance is not indexing completion. Search/read expose only each
file's published extraction; a replacement leaves its prior publication visible
until the new index mutation finishes. Deletes publish tombstones.

SQS owns execution delivery and retries; Postgres records catalog/ownership and
recovery state. S3 holds immutable sources and derived artifacts. The API never
runs transformation or bulk indexing in-process. See the
[deployment diagram](architecture-and-functionality.md).

## Troubleshooting

If sync finds no changes:

- Confirm you are syncing the expected path.
- For subset sync, confirm `--include`/`--exclude` patterns are root-relative
  and match the paths you expect.
- Check ignore rules.
- Check whether the local files already match captured heads.

If query returns no results:

- Confirm the root was captured and `sync wait` completed.
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

## End-to-end tests

Only Docker Compose end-to-end tests are maintained. The previous Go/Python/
TypeScript unit and in-process integration suites have been retired.

```sh
# Real provider calls incur usage charges. Use dedicated test credentials.
set -a; source .env; set +a
bash scripts/test-e2e.sh
```

See [the E2E runbook](../tests/e2e/README.md) for deployment roles, scenarios,
resource isolation, cleanup and explicit differences from production. Missing
provider credentials fail the suite; they never select mocks or skip scenarios.
