# Architecture and functionality

This describes the simplified code in this checkout. Production remains on the
v0.8.2 topology until the coordinated deployment in
[production-deployment.md](production-deployment.md). Verification is recorded in
[simplification-implementation.md](simplification-implementation.md).

## Deployment topology

One repository builds a Go API image, a Python worker image, a CLI, and a static
web app. The worker image runs as two separately deployable ECS/Fargate services.
Each service may have multiple replicas; threads within a worker process share a
bounded database pool. External providers below are black boxes.

```text
 USER COMPUTER                         AWS / HOSTED BACKEND
 +------------------+                  +-------------------+
 | CLI / local agent|--capture/status-->| API server        |
 | journal + watcher|--search/read----->| auth + catalog    |
 +--------+---------+                  +---+------+--------+
          | signed source upload           |      | authorized search/read
          v                                v      v
 +------------------+              +------------+  +----------------------+
 | S3               |              | Postgres   |  | Turbopuffer          |
 | originals        |              | catalog   |  | search + embeddings  |
 | canonical chunks |              | file_work |  | [external black box] |
 +---+----------+---+              | leases     |  +----------^-----------+
     |          ^                 +--+------+--+             |
     | read     | chunks              |      |               | write text
     v          |             claim   |      | claim         |
 +--------------+------+ <------------+      +--> +----------+-----------+
 | Ingestion worker    |                         | Background worker    |
 | extract / submit    |--advance file_work------>| publish / collect    |
 +----------+----------+       (Postgres)         | cleanup              |
            |                                    +-----+------+---------+
            | submit                                   |      |
            v                                          |      +--publish head--> Postgres
 +-----------------------+ <------poll results---------+
 | Gemini                |                             |
 | [external black box]  |                             +--fallback image request--+
 +-----------------------+                                                        v
                                                        +--------------------------+
                                                        | Vision endpoint on Modal |
                                                        | [external black box]     |
                                                        +--------------------------+

 Background worker --bounded cleanup--> S3 / Turbopuffer / provider uploads
 Web console --------authenticated HTTP------------------> API server
 Installer / CLI ----release manifest + archives---------> S3 / CloudFront
```

| Role | Hosting and trigger | Consumes → produces → handoff |
| --- | --- | --- |
| API server | ECS, HTTP requests | Authenticated capture → version and work in one Postgres transaction; authorized read/search → provider results filtered by published catalog |
| Ingestion worker | ECS, claims due `transform` work from Postgres | Original S3 source → canonical chunks or provider submission; advances the same work row or waits for provider results |
| Background worker | ECS, claims due `index` work; independent collection and maintenance loops | Chunks → text writes → published catalog head; provider results → chunks; durable cleanup targets → bounded deletions |
| Local agent | User computer, explicit sync or filesystem events | Stable file bytes → immutable upload and durable capture journal → API registration |
| Web console | Static S3/CloudFront app, browser interaction | User actions → API calls |

**The queue is the Postgres `file_work` table.** Capture registration enqueues
work atomically. Workers claim due rows with `FOR UPDATE SKIP LOCKED`, which
lets concurrent workers take different jobs without waiting on each other.
ECS starts worker containers; no consumer starts a worker over HTTP. The normal
lease is five minutes, renewed every minute. A lease is temporary ownership of
a job; expired ownership lets another process retry it. Attempts are bounded
and failures are visible through file status and CloudWatch summaries.

The background deployment has separate execution capacity for publication,
provider collection and cleanup. A provider poll does not occupy a publication
slot. There is no SQS queue, Go delivery consumer, Modal CPU app, worker HTTP
endpoint, embedding service, or saved index mutation file.

## Workflows

### Capture and publication

```text
local change
  --> verify prior prefix for append reuse
  --> combine new source bytes into immutable journaled packs (up to 32 MiB)
  --> signed S3 PUT or resumable multipart upload
  --> API atomically registers version + extraction + one work row
  --> ingestion claims work and extracts text / submits provider request
  --> canonical chunks saved to S3
  --> same work row advances to publication
  --> background worker writes text to Turbopuffer [black box]
  --> all writes succeed and lease/version are still current
  --> publish catalog head in Postgres
  --> search/read expose the file
```

Small files share source packs to reduce upload requests. Appends reuse the
previous remote byte ranges only after hashing and verifying the local prefix;
only the new suffix is uploaded. Rewrites and truncations capture new bytes.
Multipart upload and local journals preserve crash recovery.

### Provider extraction and fallback

```text
provider submission [black box]
  --> persist submission identity and request metadata
  --> background collection polls results
  --> validate and save successful results
  --> retry missing/retryable items
  --> terminal image failure with fallback configured
      --> vision endpoint [black box] --> validated text
  --> assemble ordered chunks --> ordinary publication
```

### Search and read

```text
CLI / web --> API authenticates and resolves permitted roots
          --> Turbopuffer [black box], one namespace per root
          --> enforce published version + per-path access
          --> return ranked results / requested lines or pages
```

### Update, deletion and recovery

```text
update --> new version --> ordinary capture/publication
                       --> prior publication stays visible until replacement succeeds
file delete --> durable tombstone --> delete index rows --> publish tombstone
root delete --> persist cleanup targets --> remove root --> repeat bounded cleanup
worker crash --> lease expires --> reclaim same work --> regenerate writes from chunks
late/stale write --> cannot publish newer head --> cleanup removes obsolete rows
```

Postgres publication is still necessary: external writes can succeed partially,
and a newer version may arrive during a write. Native embeddings remove model
orchestration, but do not make the catalog and provider one transaction.
