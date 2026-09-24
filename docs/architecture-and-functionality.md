# Architecture and functionality

This describes the production topology deployed from `535f66c` on September 18,
2026 UTC. The CLI release remains v0.8.2. Deployment instructions are in
[production-deployment.md](production-deployment.md); verification is recorded in
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

## Pipeline version 4

The optimization work keeps the same separately deployed roles and Postgres
queue. The flow below describes pipeline version 4 after migration 056; the
earlier section records the September 18 deployment. Verification is tracked in
[flow-optimization-implementation.md](flow-optimization-implementation.md).

```text
Local agent --changed paths + cached catalog cursor--> API server
Local agent --bounded concurrent pack/part uploads---> S3
API server  --atomic capture + job-------------------> Postgres file_work

Ingestion worker --claim a tenant's next transform turn--> Postgres
                 --read up to 4 MiB of native source-----> S3
                 --save parser/digest + chunk segments---> S3
                 --checkpoint + yield job for indexing---> Postgres file_work

Background worker --claim a tenant's next index turn-----> Postgres
                  --read selected immutable segments----> S3
                  --reserve shared embedding capacity----> Postgres
                  --write bounded text batch-------------> Turbopuffer [black box]
                  --checkpoint confirmed prefix----------> Postgres
                  --more native input? yield transform---> Postgres file_work
                  --more provider output? yield collector-> Postgres file_work
                  --all input verified and indexed?
                      publish whole-file catalog head----> Postgres

API server --shared search admission + published-segment checks--> Postgres
           --search / bounded line-page reads-------------------> Turbopuffer [black box]
```

Provider collection now hands off one completed provider batch at a time;
native transformation resumes its byte stream. Whole-input document decoders
still need their source container, but persist bounded output segments and use
the same bounded index turns. Worker processes yield between turns so a large
file does not retain an index slot until completion.

On append, verified immutable source extents prove an unchanged prefix. The new
extraction references its predecessor's fully indexed segments and resumes a
pre-EOF parser checkpoint for the tail. Search/read validate segment membership
against the published file and use that publication's hash for content proofs.
Cleanup retires unreferenced segments before deletion; shared segments keep
their owning artifacts alive. No unchanged prefix is sent for embedding again.

Status returns a summary; catalog cursors return committed changes; the local
follower watches changed paths with periodic full reconciliation. Upload packing
and durable journals remain. The deployment runbook describes the required
coordinated cutover and shared capacity settings.
