# PufferFS

!! This product is experimental. Report any bugs you see 

PufferFS is a filesystem sync and search service for agent workflows. Sync a
local folder into a hosted hybrid index, then query it from the CLI, web
console, scripts, or agents.

It is built around roots: named collections of versioned files with access control,
independent publication, and hybrid BM25/vector retrieval.

Try it at [pufferfs.com](https://pufferfs.com).

## What It Does

- Sync local folders into object storage and a searchable index.
- Query synced files with hybrid, full-text, or vector search.
- Keep folders current with `sync --follow` or an installed user service.
- Issue scoped API keys for agents and automation.
- Separate user-owned, organization-wide, and grant-restricted shared roots.

## Getting Started

Install the CLI on macOS or Linux:

```sh
curl -fsSL https://pufferfs.com/install.sh | sh
```

Initialize your account:

```sh
pufferfs init
pufferfs whoami
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

Sync ignores zero-byte files. If an indexed file becomes empty, its old content
is removed from search; adding content makes it eligible for indexing again.

Sync a subset of a root:

```sh
pufferfs sync --root /Users/me/handbook --include "policies/**" --name handbook
pufferfs sync --root /Users/me/handbook --include "policies/**" --exclude "policies/archive/**" --name handbook
```

Force a re-sync/reindex when committed state exists but index propagation needs
repair:

```sh
pufferfs sync --root /Users/me/handbook --force
pufferfs sync --root /Users/me/handbook --include "policies/**" --force
```

Wait for indexing, then query published files:

```sh
pufferfs sync wait --root handbook
pufferfs query "paid time off" --root handbook --top-k 2
```

Read a known file slice:

```sh
pufferfs read docs/policy.pdf --root handbook --pages 10:12
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

Useful root and status commands:

```sh
pufferfs sync ./handbook --name handbook
pufferfs sync status --root handbook
pufferfs sync wait --root handbook
pufferfs sync wait --root /Users/me/handbook --include "policies/**" --exclude "policies/archive/**"
pufferfs root current
pufferfs root delete --yes
pufferfs root delete handbook --yes
```

## External Dependencies

PufferFS depends on a few external systems:

- PostgreSQL for tenants, permissions, versioned file catalogs and recovery metadata.
- S3 for immutable originals and canonical text chunks.
- Turbopuffer for hybrid search and native embeddings.
- Separate API, ingestion and background container services, with durable Postgres work.
- An optional Modal vision endpoint for image extraction fallback.
- Gemini 3.5 Flash-Lite Batch for document/image parsing and media transcription.
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
  storage, Turbopuffer, Gemini, and ingestion/background workers. The web
  console, installer, and workers can be deployed alongside the API when needed.

Self-hosted production setup is documented in
[Production Deployment](docs/production-deployment.md). Architecture details are
in [Architecture and Functionality](docs/architecture-and-functionality.md).

## Core Components

- `cmd/pufferfs`: CLI for sync, query, root management, services, and upgrades.
- `cmd/server`: API server.
- `internal/server`: handlers, DB access, capture/catalog, Turbopuffer,
  billing, and cleanup.
- `workers`: ingestion and background runtime roles.
- `web`: web console and docs site.
- `infra/pulumi`: AWS production infrastructure.

## File Type Support

| Type | Strategy |
| --- | --- |
| Text/code/JSONL | Bounded line chunks; explicit base64 data-URL payloads redacted, original source retained |
| PDF, Word, presentations | Local page images → Gemini Batch Markdown |
| Spreadsheets | Sheet/cell-preserving extraction |
| Images | Frame/page images → Gemini Batch Markdown |
| Email/calendar/contacts | Structured text extraction |
| Audio/video | Temporary audio clips → Gemini Batch transcript; best-effort speaker labels |

See [File Ingestion and Chunking](docs/file-ingestion-and-chunking.md) for the
full format, extraction, and chunking process.

## Indexing Cost Estimates

These are estimated **backend provider costs**, not hosted-service prices or a
quote. Rates and measurements below were checked on September 18, 2026.

For a measured corpus of approximately 5,400 text/JSONL files, replacing explicit
base64 data-URL payloads with `[base64 image]` reduced 19.0 GB of source data to
10.1 GB of extracted text. A local Qwen tokenizer sample estimated **3.86 billion
tokens**, and local extraction produced approximately **1.92 million chunks**.
Original source files remain intact.

| Rerun scope, with fresh extraction | Native embeddings | Index writes | One successful pass |
| --- | ---: | ---: | ---: |
| Failed files in this corpus only (about 2,700) | ~$253 | ~$40–80 | **~$295–335** |
| Entire measured corpus | ~$270 | ~$44–87 | **~$315–360** |

For the full rerun, **$400–450 is a planning budget with room for retries**, not
an enforced spending cap. The estimate uses Qwen3-Embedding-8B at
[$0.07 per million tokens](https://turbopuffer.com/docs/embedding), 4,096-dimensional
vectors, and standard Turbopuffer [write pricing and batch discounts](https://turbopuffer.com/pricing).
Token counts are sampled estimates; write costs include approximate row metadata.
Actual charges depend on provider billing, retries, discounts, and your plan.

- Re-extract files to apply `[base64 image]`. Retrying old index work reuses its
  existing chunks and can still embed the original base64 payloads.
- The table excludes ongoing infrastructure/storage, AWS request/transfer costs,
  and search queries. Turbopuffer storage for the full resulting index is
  estimated at **~$9/month**, separate from retained source storage.
- This text/JSONL example makes no Gemini or Modal extraction calls. Actual PDFs,
  images, audio, and video can add extraction-provider charges.
- As a size comparison, 3.86 billion tokens equals **7.72 million pages at 500
  tokens/page**, or **3.86 million pages at 1,000 tokens/page**. This is a text-volume
  equivalent, not a PDF processing cost estimate.
- Provider budgets are independent: a Modal spending limit does not cap
  Turbopuffer or AWS charges.

### Measured Indexing Speed

These measurements include capture through searchable publication with native
embeddings. They are examples, not sustained throughput guarantees for the
entire corpus; content, rate limits, retries, and batch size affect speed.

| Measurement | Chunks/minute | Embedding tokens/minute |
| --- | ---: | ---: |
| Production smoke test: 258 chunks in 12.21 seconds | **~1,270** | Not measured |
| Local real-provider benchmark: four index threads, 256 documents/batch (previous default) | **~330** | **~405,000** |
| Local real-provider benchmark: four index threads, 64 documents/batch (new default) | **~1,800** | **~2.22 million** |

The local benchmark rows each indexed 2,048 synthetic chunks. At the illustrative
500 tokens/page assumption, their token rates correspond to approximately **810**
and **4,440 text-equivalent pages/minute**, respectively. These are not PDF
extraction rates, and a chunk is not a page. The production smoke test is too
short to establish sustained speed; the full corpus has not been rerun since
base64 replacement was deployed.

See [benchmark measurements](docs/indexing-performance.md) and
[production verification](docs/simplification-implementation.md#production-verification--september-18-2026-utc)
for conditions and results.

## Further Reading

- [Developer Guide](docs/developer-guide.md)
- [File Ingestion and Chunking](docs/file-ingestion-and-chunking.md)
- [API Reference](docs/api-reference.md)
- [Security and Data Handling](docs/security-and-data-handling.md)
- [Production Deployment](docs/production-deployment.md)
- [Configuration](docs/configuration.md)

## License

PufferFS is licensed under the MIT License. See [LICENSE](./LICENSE).
