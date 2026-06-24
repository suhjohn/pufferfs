# PufferFS

PufferFS is a filesystem sync and search service for agent workflows. Sync a
local folder into a hosted hybrid index, then query it from the CLI, web
console, scripts, or agents.

It is built around roots: named filesystem snapshots with access control,
generation tracking, and hybrid BM25/vector retrieval.

Try it at [pufferfs.com](https://pufferfs.com).

## What It Does

- Sync local folders into object storage and a searchable index.
- Query synced files with hybrid, full-text, or vector search.
- Keep folders current with `sync --follow` or an installed user service.
- Issue scoped API keys for agents and automation.
- Separate user-owned roots from organization roots.

## Getting Started

Install the CLI on macOS or Linux:

```sh
curl -fsSL https://pufferfs.com/install.sh | sh
```

Initialize your account:

```sh
pufferfs init
```

Preview a sync before uploading:

```sh
pufferfs sync ./handbook --name handbook --dry-run
```

Sync a directory:

```sh
pufferfs sync ./handbook --name handbook
pufferfs sync --root /Users/me/handbook
```

Sync a subset of a root:

```sh
pufferfs sync --root /Users/me/handbook --include "policies/**" --name handbook
pufferfs sync --root /Users/me/handbook --include "policies/**" --exclude "policies/archive/**" --name handbook
```

Query the latest committed generation:

```sh
pufferfs query "paid time off" --root handbook --top-k 2
```

Read a known file slice:

```sh
pufferfs read docs/policy.pdf --root handbook --pages 10:12 --output-dir ./pages
pufferfs read src/main.go --root repo --lines 200:400
```

Keep a folder current in the foreground:

```sh
pufferfs sync ./handbook --name handbook --follow
```

Or install a supervised background service:

```sh
pufferfs service install ./handbook --name handbook
pufferfs service start handbook
pufferfs service status handbook
```

Useful root and job commands:

```sh
pufferfs sync ./handbook --name handbook --background
pufferfs sync status --root handbook
pufferfs sync jobs --root handbook
pufferfs sync wait --root handbook --job-id <sync-job-id>
pufferfs root current
pufferfs root delete --yes
pufferfs root delete handbook --yes
```

## External Dependencies

PufferFS depends on a few external systems:

- PostgreSQL for users, organizations, roots, jobs, and metadata.
- S3-compatible object storage for temporary source transport, durable state
  snapshots, rendered file artifacts, and sync artifacts.
- Turbopuffer for hybrid search namespaces.
- Modal for chunking, embeddings, OCR/image processing, and shard workers.
- NATS JetStream for the optional queued sync pipeline.
- Email-code and Google OAuth for hosted web login.
- AWS SES for transactional login-code and invite email.
- Stripe for optional billing.

See [Configuration](docs/configuration.md) for environment variables.

## Deployment Options

Use PufferFS in one of two ways:

- **Hosted version**: use the managed service at
  [pufferfs.com](https://pufferfs.com). Install the CLI, run `pufferfs init`,
  and sync/query roots without operating the backend.
- **Self-hosted**: run the Go API server with PostgreSQL, S3-compatible object
  storage, Turbopuffer, Modal endpoints, and optional NATS workers. The web
  console, installer, and workers can be deployed alongside the API when needed.

Self-hosted production setup is documented in
[Production Deployment](docs/production-deployment.md). Architecture details are
in [Architecture and Functionality](docs/architecture-and-functionality.md).

## Core Components

- `cmd/pufferfs`: CLI for sync, query, root management, services, and upgrades.
- `cmd/server`: API server.
- `cmd/worker`: queued sync stage worker.
- `internal/server`: handlers, DB access, sync pipeline, Modal, Turbopuffer,
  billing, and cleanup.
- `modal`: Python Modal app and file chunkers.
- `web`: web console and docs site.
- `infra/pulumi`: AWS production infrastructure.

## File Type Support

| Type | Strategy |
| --- | --- |
| Code | Line-based chunks |
| Markdown / text | Section-based chunks |
| PDF | Page rendering and Gemini vision extraction |
| DOCX / PPTX | Convert to PDF, then page rendering and Gemini vision extraction |
| Images | Captioning/OCR pipeline |
| Email / calendar / contacts | Structured text extraction |
| Audio / video | Overlapping time-window descriptions |

See [File Ingestion and Chunking](docs/file-ingestion-and-chunking.md) for the
full format, extraction, and chunking process.

## Further Reading

- [Developer Guide](docs/developer-guide.md)
- [File Ingestion and Chunking](docs/file-ingestion-and-chunking.md)
- [API Reference](docs/api-reference.md)
- [Security and Data Handling](docs/security-and-data-handling.md)
- [Production Deployment](docs/production-deployment.md)
- [Configuration](docs/configuration.md)

## License

PufferFS is licensed under the MIT License. See [LICENSE](./LICENSE).
