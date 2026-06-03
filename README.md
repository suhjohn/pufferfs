# PufferFs

Hybrid search for your filesystem that agents can use. Sync a folder, then search it from your CLI.

## Architecture

- **CLI** (Go) — `pufferfs sync`, `pufferfs query`, `pufferfs watch`
- **Server** (Go) — API gateway for sync orchestration and query proxy
- **Compute** (Python/Modal) — file→chunks and chunks→embeddings on GPU
- **Search** (Turbopuffer) — hybrid BM25 + vector search, one namespace per synced root
- **Storage** (S3-compatible) — files, chunk images, and state

## Quick Start

### Prerequisites

- Go 1.23+
- Python 3.12+
- [Modal](https://modal.com) account
- [Turbopuffer](https://turbopuffer.com) API key
- S3-compatible storage (AWS S3, Cloudflare R2, etc.)
- PostgreSQL

### Install CLI

```bash
go install github.com/pufferfs/pufferfs/cmd/pufferfs@latest
```

### Configure

```bash
pufferfs init
# Edit ~/.tpfs/config.toml with your credentials
```

### Sync a directory

```bash
# Dry run first
pufferfs sync ./my-project --dry-run

# Sync
pufferfs sync ./my-project --name my-project
```

### Search

```bash
pufferfs query "how does authentication work"
pufferfs query "login flow" --mode fts
pufferfs query "database schema" --glob "*.sql"
```

### Watch (continuous sync)

```bash
pufferfs watch ./my-project
```

## Server

```bash
# Set environment variables
export DATABASE_URL="postgres://localhost:5432/pufferfs?sslmode=disable"
export PUFFERFS_API_KEY="pfs_sk_..."
export TURBOPUFFER_API_KEY="tbp_..."
export AWS_ACCESS_KEY_ID="..."
export AWS_SECRET_ACCESS_KEY="..."
export AWS_ENDPOINT_URL="https://..."
export AWS_BUCKET_NAME="pufferfs"
export MODAL_CHUNK_ENDPOINT="https://..."
export MODAL_EMBED_ENDPOINT="https://..."
export MODAL_QUERY_EMBED_ENDPOINT="https://..."

go run ./cmd/server
```

## Modal Functions

Deploy the Modal functions:

```bash
cd modal
pip install -r requirements.txt
modal deploy app.py
```

This creates three web endpoints:
- `chunk_file_endpoint` — file → chunks
- `embed_chunks_endpoint` — chunks → embeddings
- `embed_query_endpoint` — query text → embedding vector

## How It Works

1. **Sync**: CLI scans directory → computes diff against previous state → uploads changed files to S3 → server calls Modal to chunk and embed → upserts to Turbopuffer
2. **Query**: CLI sends query to server → server embeds query via Modal → hybrid search (BM25 + vector ANN) on Turbopuffer → reciprocal rank fusion → results returned to CLI
3. **Watch**: OS file watcher (fsnotify) → debounced incremental syncs

## File Type Support

| Type | Strategy |
|------|----------|
| Code (.py, .js, .ts, .go, etc.) | Line-based chunks (~300 lines, 50-line overlap) |
| Markdown / Text | Section/heading-based chunking |
| PDF | Page → image → text extraction |
| DOCX | Paragraph-based chunking |
| PPTX | Slide-based chunking |
| Images | LLM captioning (placeholder) |

## Project Structure

```
├── cmd/
│   ├── pufferfs/      # CLI
│   └── server/        # API server
├── internal/
│   ├── auth/          # API key middleware
│   ├── config/        # Configuration management
│   ├── diff/          # Filesystem diffing
│   ├── ignore/        # .gitignore-style exclusion
│   ├── server/        # Server handlers, Modal/TP clients
│   └── storage/       # S3-compatible storage
├── modal/             # Python Modal functions
│   ├── app.py         # Modal app definition
│   ├── chunkers.py    # File type-specific chunking
│   └── models.py      # Shared data models
└── pkg/
    └── models/        # Shared Go data types
```
