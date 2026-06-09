# PufferFS

PufferFS is a filesystem sync and search service for agent workflows. Sync a
local folder into a hosted hybrid index, then query it from the CLI, web
console, scripts, or agents.

It is built around roots: named filesystem snapshots with access control,
generation tracking, and hybrid BM25/vector retrieval.

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
```

Query the latest committed generation:

```sh
pufferfs query "paid time off" --root handbook --top-k 2
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
pufferfs root delete handbook --yes
```

## External Dependencies

PufferFS depends on a few external systems:

- PostgreSQL for users, organizations, roots, jobs, and metadata.
- S3-compatible object storage for uploaded files, bundles, states, and sync
  artifacts.
- Turbopuffer for hybrid search namespaces.
- Modal for chunking, embeddings, OCR/image processing, and shard workers.
- NATS JetStream for the optional queued sync pipeline.
- Google OAuth for hosted web login.
- AWS SES for optional email invites.
- Stripe for optional billing.

See [Configuration](docs/configuration.md) for environment variables.

## Deployment Options

PufferFS can run in a few shapes:

- **Hosted production stack**: AWS ECS/Fargate, ALB, S3, CloudFront, EFS-backed
  NATS, Pulumi, and GitHub Actions.
- **Self-hosted API**: run the Go server with PostgreSQL, object storage,
  Turbopuffer, and Modal endpoints.
- **Queued workers**: add NATS JetStream and run stage workers for chunk, embed,
  index, commit, and cleanup.
- **Static web console**: build `web/` and serve the generated static assets.
- **CLI-only clients**: distribute `pufferfs` plus a server URL and scoped API
  key.

Production setup is documented in
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
| PDF | Page rendering and OCR/text extraction |
| DOCX | Paragraph-based chunks |
| PPTX | Slide-based chunks |
| Images | Captioning/OCR pipeline |

## Further Reading

- [Developer Guide](docs/developer-guide.md)
- [API Reference](docs/api-reference.md)
- [Security and Data Handling](docs/security-and-data-handling.md)
- [Production Deployment](docs/production-deployment.md)
- [Configuration](docs/configuration.md)

## License

PufferFS is licensed under the MIT License. See [LICENSE](./LICENSE).
