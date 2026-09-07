# Current-format ingestion audit

This audit covers the API server, transformation workers, collector workers,
index workers, scheduled reconciler, and local CLI. Their hosting, triggers,
queues and handoffs are shown in the [role diagram](provider-batch-manifests.md#roles-and-deployment).
These changes are local until a deployment is explicitly recorded below.

The requested contract is a fresh schema with S3 batches, without conversion
from the previous ingestion implementation. Ordinary crash recovery, ownership,
retention and integrity checks still apply to current-format work.

## Removed paths

| Area | Previous compatibility behavior | Current behavior |
| --- | --- | --- |
| Provider schema, 045 | Refused replacement while any old batch/upload row existed | Replaces the old ledger without an emptiness gate or conversion |
| Embedding schema, 041 | Backfilled offsets into pack directories, retained `embedding_locations`, reconstructed locators on downgrade | Replaces both old cache tables with one pack-directory table; cache misses encode normally |
| Embedding cleanup | Deleted old locator rows and accepted both old and current S3 key layouts | Clears one pack directory and accepts only current single-use keys |
| Source reader | Accepted standalone `.json` and packed `.jsonl` manifests | Requires an owned, bounded `.jsonl` range with a record checksum |
| Capture replay | Normalized packed records into standalone object identities | Compares owned record digests independently of pack membership; rejects invalid locators |
| Source provenance | Scheduled backfill, extra version flags/index, pending-catalog errors and root-wide cleanup gates | Extents are committed with each accepted version; no backfill state or polling |
| Source upload ownership | Treated old uploads with no uploader as re-upload candidates | Requires an uploader identity in the schema and exact authenticated ownership in the API |
| Index publication | Resumed old per-write checkpoints and accepted rows without extraction sequences | Replays the whole immutable artifact; requires exact version/extraction identity and sequence; publishes acknowledgment atomically |
| Index deletion/cleanup | Additional deletion branch for rows without file identity; retained unsequenced revisions | Uses current file/version/extraction identities and ordered cutoffs |
| Media preparation | Interpreted old extraction revisions with five-minute clips | Supports the current extraction revision and its one-minute clip contract |
| Generation inventory | CLI `sync audit`, historical root-state endpoint, generation joins and metadata fields | Current capture catalog and status/wait only |
| Deletion | Queried/cancelled retired generation jobs; collected historical prefixes and guessed namespaces | Marks current roots before IO, retains registered namespace/current-prefix targets, then deletes; applies to root/user/org entrypoints |
| Retired tables, 046 | Kept generation jobs/shards, root states, root-wide proofs and old embedding cache | Drops those six tables, generation metadata, and source-backfill columns |
| Email wiring | Unused invite-only aliases and wrapper constructors | One transactional-email interface and constructors |
| E2E setup | Built old APIs/workers to exercise conversion and partial checkpoints | Runs current roles on fresh schema; preserves corruption, full replay, retry, deletion, authorization and restart scenarios |

`provider_batches` remains one row per bounded input range. Input/result/cleanup
detail is stored in immutable S3 manifests. `embedding_packs` remains one row per
bounded vector pack. The core file/version/work catalog and source-provenance
edges remain because current workflows use them.

## Checks intentionally retained

- **Submission markers and discovery cursors** prevent duplicate paid jobs after
  a lost response. This is current-run recovery, not a version migration.
- **Batch/work leases, attempt tokens and current-head checks** prevent stale
  workers from publishing after retries, replacement or deletion.
- **S3 owner/range/checksum/size checks** bind bytes and mutation targets to the
  authorized current file. Packed replay still works when a capture is reordered
  or retried as a subset.
- **Root deletion tombstones and upload deletion/expiry receipts** survive
  catalog removal and late network writes. They do not preserve old schemas.
- **Immutable extraction revisions and model IDs** describe current content
  contracts; they are not removed merely because their names contain a version.
- **Read pagination's metadata query** resolves an empty range against the same
  pinned publication. It does not fall back to a previous artifact format.
- **CLI minimum-version checks, supported login flows, API scopes, health URLs
  and configured email environment names** remain active product contracts.
  They do not migrate ingestion state or add old storage readers.
- **Historical SQL files** remain the ordered schema-construction history.
  Later definitions replace/drop the old structures. There is no runtime
  schema detection, dual read/write, background conversion, or backfill worker.
  Irreversible schema changes reject downgrade because deleted records cannot
  be reconstructed; a successful but incomplete rollback is not reported.

## Verification and deployment

Static review covered runtime SQL, artifact readers/writers, checkpoint replay,
cleanup target generation, CLI/API inventory, schema changes, test runners and
current documentation. The older throughput/implementation ledgers retain their
dated measurements; their migration results describe earlier snapshots.

Fresh-schema E2E results are recorded here after the runs complete. They use
separate production processes, real Postgres and Turbopuffer/Gemini, and
LocalStack S3/SQS. They do not verify Modal autoscaling, AWS IAM, a production
cutover, or million-file throughput.

A read-only deployment preflight found schema 40 with one root, 5,340 captured files,
10,071 embedding packs, 10 completed provider batches, 21 provider upload records,
and 3,645 pending/running work items. A follow-up confirmed all 5,340 source versions use standalone `.json`
locators, none use the current packed format, and two work leases were live.
Counts are a point-in-time observation.
No deployment or production data reset was performed by that preflight.
The previous source/artifact formats are intentionally unsupported; removing
migration support does not establish authorization to erase existing captures.

### Completed runs

- `5b8f85cdf08a4d729ccab495b461b5c9`: current-worker index crash/replay,
  all phases passed, exit 0 and resources cleaned. Replayed all three mutation
  batches after the normal lease; exact artifact identity and all 1,025 source
  lines/FTS passed through two APIs and after API restart. Recovery: 298.71 s.
- `cf86e9bed603457e8baf74d3d26e8c43`: root deletion during an accepted paid
  job, passed in 92.62 s; three acknowledged upload deletions; cleanup passed,
  exit 0 and resources cleaned. No legacy namespace/prefix targets were needed.

The first full/retention runs were deliberately stopped before vector creation
when review found the new embedding table lacked the empty-directory default
needed for pre-upload reservations. Cleanup passed (1.61 s and 8.17 s), and
fresh reruns use the corrected schema. No full-pass claim applies to those
stopped runs. The text-only suites do not exercise embedding reservations.

- API suite `ffd8c40f8f5e45cea912450821783249`: all phases passed, exit 0
  and resources cleaned (cleanup 5.82 s). Two API processes verified current
  membership/key/grant authorization, multi-root search, Unicode paged reads,
  concurrent member/group changes, browser sessions, root/directory creation,
  CLI publication, and durable behavior after API restart. Its fresh schema
  had non-null source upload ownership and no extent-backfill columns.

Final fresh-schema inspection found 28 public tables, no stored function
references to retired provider/vector/generation tables or backfill, the empty
vector-directory reservation default, and non-null source uploader identity.

- Manifest/capture suite `pufferfs-manifest-packs-77320`: all phases passed,
  exit 0 and resources cleaned. Verified one manifest PUT for 128 files,
  reordered/subset replay, checksum/truncation rejection and queue recovery,
  bounded capture SQL, concurrent two-API captures, provenance and revocations,
  actual source authorization expiry, retained-byte re-upload and API restarts.
  Final capture run `df7aab0eac29406da9098b4922b7443f` cleanup: 7.81 s.
- Retention/security `d7a3081efa4443d68c3222b1e0bd7415`: all phases
  passed, exit 0 and resources cleaned. Security/retention: 930.79 s; cleanup:
  8.43 s. Verified current vector-pack reservation/reuse/expiry/re-encoding,
  source provenance and authorization revocations, real source expiry,
  multipart cleanup, immutable receipt replay and retained-byte re-upload.
- Full corpus `94e825fd99f84b3ba8e54aff1af11466`: all phases passed,
  exit 0 and resources cleaned. Mixed-format extraction, source/artifact checks,
  FTS/vector/hybrid search and authorization: 882.78 s. Outage recovery after
  API/consumer restart: 58.79 s. Mixed malformed/valid SQS delivery passed;
  23 provider uploads had acknowledged deletions, with none awaiting expiry.
  Cleanup: 7.43 s. No PufferFS E2E containers remained after these runs.

Go build, web build, Python compilation, shell syntax and diff whitespace
checks passed. No unit or mock-based tests were used.
