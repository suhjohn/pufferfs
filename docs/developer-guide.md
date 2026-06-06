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
PUFFERFS_VERSION=0.2.0 INSTALL_DIR=~/.local/bin curl -fsSL https://pufferfs.com/install.sh | sh
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
4. Optionally keep the folder current with watch mode or a background service.

A root is the durable unit of sync and access control. The local folder remains
the source of truth. PufferFS stores uploaded copies, extracted chunks,
embeddings, state snapshots, and search index rows so it can answer queries.
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

Create a user-owned root:

```sh
pufferfs sync ./workspace --name workspace --scope user
```

Return machine-readable output:

```sh
pufferfs sync ./workspace --json
```

What to expect:

- The CLI hashes the folder and builds a Merkle tree.
- It compares the current tree to local cache when possible.
- If local cache is stale relative to the server, it fetches remote state and
  diffs against that.
- It uploads only changed content.
- Small files are packed into bundle objects; large and empty files are
  uploaded individually.
- The server creates a sync job and a new generation.
- The index is not visible to queries until the generation commits.
- The CLI polls async sync jobs until completion.
- If the server generation changed during sync, the CLI reloads state,
  recomputes the diff, and retries.

## What Gets Synced

PufferFS honors:

- Built-in ignore rules.
- `.gitignore` in the root.
- `.tpfsignore` in the root.
- Global ignore rules at `~/.tpfs/ignore`.

Common ignored paths include dependency folders, virtual environments, build
outputs, caches, and `.git`.

Likely secret filenames such as `.env`, `.env.*`, private keys, npm/pypi config
files, and credential JSON files are excluded from sync state by default. Treat
this as filename-based protection, not a full content secret scanner.

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
presentations, and images use Modal compute when configured.

Expected extraction behavior:

- Code and config files are split into overlapping text chunks.
- Markdown and text are split by headings and text boundaries where possible.
- PDFs are rendered by page; native PDF text is used when it appears reliable.
- Scanned or image-heavy pages can use vision extraction when available.
- Word and PowerPoint files are converted to PDF first, then processed by page.
- Images can be captioned or text-extracted when vision extraction is available.

Page-based document results may include page numbers and image artifact paths.

## Continuous Sync

Run a foreground watcher:

```sh
pufferfs watch ./workspace --name workspace
```

Or:

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

Delete a root:

```sh
pufferfs root delete workspace
```

Skip confirmation:

```sh
pufferfs root delete workspace --yes
```

What to expect:

- Root deletion removes PufferFS metadata, stored source copies, sync artifacts,
  chunk/page artifacts, and Turbopuffer namespaces.
- Root deletion removes the local PufferFS cache for that root.
- It does not delete local source files.
- Roots with active sync jobs cannot be deleted until jobs finish.

## Permissions and Access

What a caller can do depends on:

- API key scopes.
- Organization role.
- Root scope.
- Root ownership.
- Deny-prefix ACLs.

General expectations:

- Query/read access is enough to list accessible roots and search.
- Sync/write access is needed to create roots, upload files, and sync.
- Org roots require editor-or-higher role to create or write.
- User roots can be written by their owner or org admins.
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

- Object storage is the data plane for uploaded source copies, state refs, sync
  artifacts, and page images.
- PostgreSQL is the control plane plus small durable caches.
- Turbopuffer is the search index.
- Modal is the heavy compute layer for embeddings and document/image
  extraction.

## Troubleshooting

If sync finds no changes:

- Confirm you are syncing the expected path.
- Check ignore rules.
- Check whether the local cache already matches the visible generation.

If query returns no results:

- Confirm the root was synced and the sync job completed.
- Try `--mode hybrid`.
- Remove overly narrow `--glob` filters.
- Confirm the API key has query/read access.
- Confirm the caller can read the root and path.

If watch does not pick up files:

- Confirm the files are not ignored.
- Confirm the watched directory still exists.
- Restart the watcher after large directory moves.

If a sync fails repeatedly:

- Run a normal `pufferfs sync` once to see the direct error.
- Check upload size limits for very large files.
- Check server-side Modal, Turbopuffer, object storage, and queue configuration.
- Check service logs if running as a background service.

## Upgrade Behavior

The CLI can check the server's release manifest and print an upgrade notice at
most once per day.

Direct installs can run:

```sh
pufferfs upgrade
```

What to expect:

- Direct upgrades download a platform archive.
- The archive checksum is verified.
- The current binary is replaced.
- Installed user services can be restarted after upgrade.
