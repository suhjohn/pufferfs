# Flow optimization implementation

Scope: all eight improvements in the user attachment, reviewed against
`b1a5887`. All eight are implemented and their production-process E2E
checks passed. Deployment uses the coordinated pipeline-version-4 cutover;
the GitHub Actions deployment record identifies the live revision.
The production corpus was not redriven. Test resources were isolated and cleaned up.

| Requirement | Implementation | Verification |
| --- | --- | --- |
| Confirmed index progress and deployment shutdown checkpoints | Implemented for current artifacts | Passed: FTS and native embedding confirmed responses, lost response, crash, SIGTERM, exact reads/search and API restart. |
| Shared request/token budgets and tenant fairness | Implemented shared admission, adaptive token-aware batches, tenant claim rotation and bounded native segment turns | Passed: two worker processes/two APIs, competing tenants, shared token/request windows, natural expiry/restart, six external 429s and bounded segment turns. |
| Summary endpoints, incremental catalog cursors, changed-path tracking | Implemented | Passed: bounded summary, concurrent commits, durable cache crash/reconnect, ACL changes, changed-path scans and actual OS watcher overflow/restart |
| Bounded independently resumable segments and atomic publication | Implemented native source/parser checkpoints, segment artifacts, bounded index turns and pinned publication membership | Passed: large native input, partial failures, retries, pinned reads, multi-batch provider assembly and atomic publication. |
| Reuse unchanged extracted/indexed output on append | Implemented immutable source-prefix verification, shared segment membership, pre-EOF tail restart and segment retirement | Passed: partial lines, UTF-8/base64 boundaries, rewrite/truncate, cleanup, stale/fresh content proofs and exact tail-only replay. |
| Concurrent pack/multipart uploads with durable journals | Implemented | Passed: bounded pack/part concurrency, out-of-order acknowledgments, SIGKILL, exact retained source bytes, replacement publication/read/search and API restart. Legacy spool and multipart recovery also passed. |
| Cleanup page draining, prompt continuation, failure isolation | Implemented | Passed: 1,005 objects, 101 multipart sessions, concurrent root failure, worker restart, source/artifact retention and late-index-write cleanup. |
| Aggregate/per-tenant search admission and retry metrics | Implemented | Passed: two APIs/three tenants, weighted admission, cancellation, deadline and crash lease expiry. Full API suite verified stale-candidate retry/rejection metrics. |

## Invariants

- Preserve source packing and verified append upload reuse.
- Retain immutable source bytes and exact indexed read/location contracts.
- Only publish a complete file version; preserve its predecessor until then.
- Every checkpoint is tied to immutable input and fenced by current ownership.
- A lost external response is ambiguous; never claim exactly-once billing.
- Catalog cursors cannot skip late commits. Watcher overflow requires full repair.
- Shared chunk reuse must remain valid through deletion and late stale writes.
- Keep provider calls real and exercise faults at process/network boundaries.
- Use isolated synthetic E2E resources. Do not redrive the production corpus as
  an implementation test.

## Sequence

Start with confirmed-write progress and graceful shutdown, which are needed by
bounded scheduling. Add shared admission and segmented artifact/execution
state, then build append reuse on that immutable segment identity. Independently
implement incremental client/catalog flow, upload concurrency, cleanup and
search admission. Validate each contract and then run the combined workflow
matrix before shipping. Do not substitute the existing legacy E2Es for coverage
of the new contracts.

## Verification log

- Initial state inspected: Docker is available; production-process Compose
  suites and real-provider network relays exist. Existing checkpoint E2E
  currently expects full replay and must be updated to assert saved work.
- `test-e2e-concurrent-uploads.sh`: passed, isolated project
  `pufferfs-concurrent-uploads-53219`, cleaned up. Two packs honored a shared
  three-transfer bound. A later multipart part was durably acknowledged before
  the first; recovery reused that acknowledgment and replayed only the ambiguous
  part. Retained source bytes and replacement reads/search survived API restart.
- `test-e2e-index-checkpoints.sh`: passed, isolated project
  `pufferfs-index-checkpoints-48065`, cleaned up. For 1,025 chunks, SIGKILL recovery
  made two remaining writes (one ambiguous replay) instead of replaying the
  confirmed first batch. SIGTERM checkpointed both successful batches, released
  the job without consuming a failed attempt, and resumed with one final write.
  Both APIs returned all exact lines before and after restart. Two earlier runs
  exposed test probe serialization issues; neither was reported as a pass.
- `test-e2e-search-admission.sh`: passed, isolated project
  `pufferfs-search-admission-76281`, cleaned up. Configured global capacity of
  three namespace requests and tenant capacity of two held across API replicas.
  Excess requests returned 429/Retry-After before provider IO. Cancellation and
  deadlines released slots; after SIGKILL both restarted APIs respected the
  persisted reservation until its normal 45-second expiry. An earlier run found
  a SQL parameter type conflict, corrected before the passing run.
- `test-e2e-cleanup-pages.sh`: passed, isolated project
  `pufferfs-cleanup-pages-76282`, cleaned up. The first worker pass removed both
  object pages and 100 multipart sessions, left one session immediately due,
  and continued despite a separate root's S3 503s. Restart resumed that final
  session immediately. An earlier fixture incorrectly expected deleting-root
  metadata to return 404; the correct assertion verifies the durable deletion
  flag and refusal of new uploads.
- `test-e2e-embedding-batches.sh`: passed, project
  `pufferfs-embedding-batches-87969`, cleaned up. Native 64-document writes
  checkpointed the confirmed first batch and replayed only the ambiguous second
  response after SIGKILL. All 257 vectors, exact lines, vector/hybrid/FTS queries
  and both API restarts passed.
- `test-e2e-capture-summary.sh`: passed, project
  `pufferfs-capture-summary-19475`, cleaned up. The CLI summarized 1,025 files in
  one request, with selected hashes, missing paths and fresh ACLs. All versions
  published; exact read/search and completed summaries survived API restart.
  Earlier harness runs had a missing optional JSON-field assertion and omitted
  the second API replica during worker startup; those runs are not passes.
- `test-e2e-catalog-changes.sh`: passed, project
  `pufferfs-catalog-changes-33409`, cleaned up. A 521-file cache survived SIGKILL
  between pages; recovery fetched only the uncommitted page and an unchanged
  sync fetched zero records. Twenty concurrent captures were never skipped.
  ACL changes invalidated cursors, while publication/deletion and API restarts
  preserved incremental metadata. Earlier fixtures incorrectly reused a bound
  pack and treated folder ACLs as file ACLs; corrected to public API contracts.
- `test-e2e-follow-changes.sh`: passed, project
  `pufferfs-follow-changes-33519`, cleaned up. An unprivileged
  CLI captured an append while an unrelated directory was unreadable. Nested
  moves, ignore rules, a real inotify queue overflow, SIGKILL/offline rewrite,
  SIGTERM, the existing pending-index append/rewrite/truncate/delete scenario,
  retained bytes, exact reads/search and API restart passed. An initial fixture
  bounded its queue workload below Docker's configured OS queue capacity; the
  corrected fixture uses alternating writes to two temporary files.
- `test-e2e-embedding-capacity.sh`: initial implementation passed, project
  `pufferfs-embedding-capacity-39078`, cleaned up. Native writes and two API
  replicas shared configured token/request debits; restart preserved them and
  natural expiry freed them. Six external 429s shared a cooldown with queries,
  then the job completed with attempt_count=1. This run exposed avoidable idle
  time from conservative reservations. Adaptive smaller batches and faster
  token-capacity rechecks were added afterward and verified in the second run
  below with one worker thread per background replica.
- The updated `test-e2e-embedding-capacity.sh` passed in project
  `pufferfs-embedding-capacity-68111`, cleaned up. One thread in each of two
  background replicas shared the same token window with both APIs. Batches
  shrank to fit remaining capacity; all 16 vectors/chunks published with exact
  source/read results. Shared request limits, restart, expiry, FTS availability
  and six external 429s passed again.
- `test-e2e-base64-redaction.sh` passed in project
  `pufferfs-base64-redaction-65787`, cleaned up. The new checkpointable parser
  preserved base64 replacement text and source/line bounds through capture,
  process restarts and updates. This verifies the ordinary streaming path;
  persisted parser/digest restoration still requires the segment E2Es.

## Current local client contracts

- `/roots/{id}/capture-summary` returns counts and at most 20 examples. POST
  accepts at most 1,000 selected path/hash/size tuples. Global status still
  aggregates current database rows; it does not enumerate them over the network.
- `/roots/{id}/catalog-changes` assigns revisions only to committed outbox
  events, so a late commit cannot disappear behind a cursor. Cursors bind the
  root, organization, user and deny rules. An ACL change returns 409/reset.
- `remote-catalog.db` is a local bbolt metadata cache. Each transaction saves a
  page and cursor together. Pending changed paths remain until capture succeeds.
  Source bytes and existing durable upload journals retain their own lifecycle.
- The follower drains filesystem events while capture performs network IO.
  Events select paths/subtrees; overflow, restart, policy changes and the
  configurable `--reconcile-interval` (15 minutes by default) trigger a full
  scan. A 30-second metadata poll detects remote catalog/central-policy changes.

## Shared embedding capacity configuration

`PUFFERFS_EMBEDDING_REQUESTS_PER_MINUTE` (default 1,024) and
`PUFFERFS_EMBEDDING_TOKENS_PER_MINUTE` (default 2,000,000) must match in every
API/background replica. These are configured account/model budgets, not quota
discovery. Their initial values are persisted on first admission; disagreeing
replicas fail closed. To change a running budget, drain embedding producers,
update `provider_capacity` for the model to the intended limits, then roll out
matching configuration. Existing 60-second request debits remain in force.

Each actual attempt reserves one request plus UTF-8 bytes and prompt overhead
as a conservative token estimate. Successful `performance.embedding_tokens`
measurements replace the estimate; missing metrics or ambiguous responses keep
it until expiry. 429 responses share a cooldown, retain the request debit and
release rejected token estimates. Index work defers without consuming failed
attempts. Vector/hybrid queries return 429 with Retry-After; FTS does not consume
embedding capacity. Postgres serializes admission only, never provider IO.
When a full batch does not fit, admission reports remaining token capacity.
The worker builds a smaller prefix and atomically reserves it again before
calling the provider. Only the actual confirmed prefix advances its checkpoint.

## Segment implementation (pipeline version 4)

The checkpointable parser, digest helper, source-range reader, migration 056,
immutable segment/checkpoint IO, fenced preparation/turn transitions, append
seeding, publication/read/search membership and shared segment cleanup are now
connected to the runtime and verified by the E2Es below. A first
fresh-database run exposed an unsupported infinite
timestamp decoded by the Python database driver; the migration default was
corrected, that run was stopped and cleaned up, and fresh runs were started.

- Keep the existing ingestion/background deployment roles and `file_work`
  queue. A file can yield bounded transform and index turns through that queue;
  the current attempt token fences every state change. Tenant rotation happens
  at each turn, so one large file cannot keep a slot for its entire lifetime.
- Persist immutable chunk segments plus extraction-to-segment membership.
  An extraction has a growing prepared prefix, a confirmed index cursor, and
  an immutable transform checkpoint. Native text checkpoints include parser
  buffers, redaction state, positions, unfinished segment records and SHA-256
  state. A pre-EOF checkpoint retains the unfinished final chunk for appends.
- `cmd/pufferfs-source-digest` uses Go's standard SHA-256 binary state format
  through bounded pipes. This avoids custom cryptography and lets a resumed
  worker continue the full-file digest without downloading an unchanged prefix.
  It is packaged in worker images and used by native transformation.
- Read only authenticated immutable source extents; before append reuse, prove
  the previous version's extents are the exact new prefix and the predecessor
  extraction completed source verification. On rewrite/truncate or changed
  extraction contracts, start a new stream. Never trust an unfinished previous
  version as a verified prefix.
- Stable index row identities belong to immutable segments, allowing append
  versions to share them without resending text to the embedding provider.
  Publication still waits for full source hash verification and every segment's
  confirmed index progress. Previous publication stays visible until then.
- Search validates candidate segment membership against a pinned published
  extraction, with bounded candidate lookups rather than loading all segment IDs
  for a large file. Read requests use bounded segment lookup/paging using line,
  page or ordinal ranges. Content proofs use that publication's file hash;
  a reused row's original file hash cannot authorize a new version.
- Cleanup retires unreferenced segments under a database lock before any
  external delete. New memberships cannot pin a retiring segment. Current and
  pending extractions protect their shared segments; root deletion still owns
  the entire root prefix. Existing version-cutoff deletion and owner-extraction
  prefix deletion preserve shared rows/artifacts.
- Preserve legacy row_format=1 artifacts/publications during rollout. Native,
  structured and provider-produced chunk streams all use bounded indexing;
  document conversion remains subject to its parser/provider's whole-input
  contract. No old corpus redrive is authorized or required for migration.

Native source turns read at most 4 MiB. Index turns process up to one 64-chunk
segment with vectors or eight segments without vectors. Whole-input decoders
still parse their source/container before indexing; they persist bounded segment
artifacts during decoding and validate saved output when recovering. They do
not create a monolithic final chunk artifact.
Provider collection prepares one completed batch (at most 64 requests and a
64 MiB decoded assembly window) per turn, persists its request/output cursors,
and hands the same file job to indexing. Once that prefix is confirmed, the job
returns to collection. Only the final batch permits whole-file publication.
Preparation validates the complete request range once; collection seeks its next
batch and checks an indexed failure lookup instead of rescanning the whole file.

Read and search requests have a 30-second lifetime. Segment retirement waits at
least one minute after a catalog change, allowing pinned requests to finish.
Metadata fallback reads the first and last segments, preserving exact line
bounds without loading every segment or downloading content.

The full `test-e2e-api-access.sh` passed in project
`pufferfs-api-access-94816`, cleaned up. It verified API permissions, source
retention, exact reads/search, root routing, stale-candidate retries and metrics,
key revocation, groups, concurrency and process restarts before segmented
publication was enabled. The later combined runs below also passed.

- Segmented `test-e2e-base64-redaction.sh` passed in project
  `pufferfs-base64-redaction-49916`, cleaned up. Capture, real native vectors,
  parser/digest restoration across the 11 MiB data URL, exact source/location
  preservation, FTS/vector/hybrid search, restart, append, deletion and forced
  extraction passed. This also covered segmented CSV output.
- `test-e2e-capture-spool.sh` passed in project
  `pufferfs-capture-spool-66397`, cleaned up. Byte/spool limits, packing, upload
  failures, conflicting retries, retained journals, append reuse, replacements,
  deletion, empty files with/without vectors, authorization, process restarts
  and multipart recovery passed against segmented extraction.
- The first segmented API suite reached permission and stale-publication checks,
  then a clean-query call-count assertion encountered still-retained obsolete
  segments. The harness now waits for real scheduled segment retirement before
  measuring a clean single-pass query, while separately asserting the retry
  behavior. Its read assertions also observe bounded segment filters instead of
  the legacy extraction filter. This failed run is not a pass.
- `test-e2e-segments.sh` passed in project `pufferfs-segments-49917`, cleaned up:
  4 MiB source turns, SIGTERM checkpointing, exact whole-file source download
  count after restart, stable-prefix append reuse, SIGKILL/normal lease recovery,
  shared-segment cleanup, rewrite/truncation, deletion and both API restarts.
  An expanded run adds explicit stale/fresh content-proof checks on the reused
  prefix and exact tail-write observations.
- Segmented `test-e2e-embedding-batches.sh` passed in project
  `pufferfs-embedding-batches-58179`, cleaned up. Confirmed segment writes were
  not repeated after SIGKILL; the ambiguous second batch was replayed once.
  All 257 native vectors, exact reads and search modes survived API restart.
- Segmented `test-e2e-api-access.sh` passed in project
  `pufferfs-api-access-70606`, cleaned up. This includes pinned segment reads,
  current/stale proofs, in-flight ACL revocation, membership/routing call counts,
  stale-publication retry metrics and API restart. Later read-deadline and exact
  metadata-bound changes passed in project `pufferfs-api-access-7672` below.
- `test-e2e-formats.sh` passed in project `pufferfs-formats-local-66398`, cleaned
  up. Native spreadsheet/structured formats and real provider-backed variants
  produced segmented artifacts with retained source hashes, expected content,
  locations and public search. The later bounded provider-assembly handoff is
  verified by the provider recovery suite below.
- `test-e2e-upgrade.sh` passed in project `pufferfs-upgrade-3456`, cleaned
  up. Data created by the v0.8.2 CLI/API/workers remained readable after the real
  migrations, pending legacy work finished, and unchanged files were not
  re-extracted. The old phase uses its matching old CLI; the new phase uses the
  new catalog cursor API.
- The latest `test-e2e-api-access.sh` passed in project
  `pufferfs-api-access-7672`, cleaned up. In addition to the segmented API
  coverage above, this run verified the read deadline and bounded metadata
  fallback, then repeated public reads/search after both API processes restarted.
- Combined `test-e2e-embedding-capacity.sh` passed in project
  `pufferfs-embedding-capacity-20995`, cleaned up. Shared request/token budgets,
  adaptive batches, tenant scheduling, external throttling and natural expiry
  remained correct with segmented extraction enabled.
- An expanded append run reached recovery but its fixture reused the denied
  user as the newly authorized proof reader. A separate synthetic user now owns
  the proof checks; the failed run was cleaned up and is not a pass.
- The summary regression's 1,025 real single-file publications exceeded its
  former five-minute harness wait while progressing without failed work. It now
  uses the suite's ordinary configurable E2E deadline; bounded response and
  query-count assertions are unchanged. The fresh run passed, as recorded below.
- Review of the bounded-turn handoff found a lease-heartbeat edge case. A
  confirmed provider response is now checkpointed before checking for heartbeat
  failure, and that check precedes ownership release. The database-outage E2E
  asserts the stopped attempt and saved cursor before recovery, rather than
  assuming every interrupted attempt must replay a confirmed response.
- Combined checkpoint, search-admission and cleanup-page suites passed in
  projects `pufferfs-index-checkpoints-7675`, `pufferfs-search-admission-37167`
  and `pufferfs-cleanup-pages-52853`; all were cleaned up. Confirmed writes,
  graceful shutdown, aggregate/per-tenant query limits, crash expiry, multiple
  object pages, multipart cleanup and independent cleanup failures passed.
- One summary rerun stopped during Compose readiness: `up --wait` treated the
  successful one-shot readiness container's exit as a failure. The script now
  waits for long-running services and runs the readiness command separately.
  No user workflow or provider resource had been created in that failed run.
- The expanded `test-e2e-segments.sh` passed in project
  `pufferfs-segments-42366`, cleaned up. The >8 MiB Unicode fixture resumed
  without source rereads; append downloaded only its suffix and reused more
  than 2,048 stable chunks. Only the ambiguous tail write was repeated. Stale
  content proofs could not read/search the new publication's reused rows; fresh
  proofs worked. True end-of-file metadata, shared-prefix cleanup, rewrite,
  truncate, deletion and both API restarts also passed.
- `test-e2e-retention.sh` passed in project
  `pufferfs-retention-local-84689`, cleaned up. Real source authorization expiry,
  orphan-pack and multipart cleanup, obsolete extraction artifacts, retained
  append extents, receipt bounds, forged source references and in-flight ACL,
  membership, grant and API-key revocations passed.
- Full `test-e2e.sh` passed in project `pufferfs-e2e-local-1-20988`, cleaned up.
  Native capture, pending-index follower changes, the mixed real-provider
  corpus, multipart recovery, public reads/search, authorization, service
  outage, API restart and resumed work all passed with segmented extraction.
- The partial-failure vision E2E passed in project
  `pufferfs-vision-local-42754`, cleaned up. Real Gemini successes and Modal
  fallback results matched the supplied four-page expectations, survived
  collector SIGKILL and passed public page reads/search without resubmitting
  successful Gemini pages.
- Combined summary and incremental-catalog suites passed in projects
  `pufferfs-capture-summary-55079` and `pufferfs-catalog-changes-13951`, cleaned
  up. All 1,025 summary files published and exact reads/search survived restart.
  The 521-file cache retained page/cursor atomicity through SIGKILL, concurrent
  commits were not skipped, unchanged sync returned no records, and ACL changes,
  publication and tombstones preserved the catalog cursor contract.
- Combined changed-path follower and concurrent-upload suites passed in
  `pufferfs-follow-changes-19307` and `pufferfs-concurrent-uploads-28628`, cleaned
  up. Actual watcher overflow, offline edits, nested moves, ignore changes,
  interrupted journals and out-of-order acknowledgments all preserved captured
  bytes and user-visible reads/search.
- Combined `test-e2e-index-recovery.sh` passed in
  `pufferfs-index-recovery-local-51407`, cleaned up. The real provider accepted
  ambiguous and stale writes, while current publication and authorization
  remained correct. Postgres restart recovered pooled connections; scheduled
  cleanup removed late writes after root deletion.
- The cancellation vision E2E passed in `pufferfs-vision-local-93920`, cleaned
  up. A real cancelled Gemini job used Modal for all four supplied page
  expectations, survived collector restart, and preserved public reads/search
  and provider upload cleanup.
- The final lease-renewal E2E passed in `pufferfs-index-renewal-55219`, cleaned
  up. Postgres stayed down through the real heartbeat and pool deadlines while
  a provider response was held. The worker checkpointed both confirmed batches,
  stopped the attempt before a third write, and the same process retried only
  the remaining chunk. An earlier harness attempt omitted `error` from its
  inspection query and is recorded as a failed run, not a production failure.
- Media E2E passed in `pufferfs-media-local-39016`, cleaned up, using real
  provider results and public source, read, search and location assertions with
  segmented artifacts.
- Final capture-handoff E2E passed in `pufferfs-capture-handoff-74085`, cleaned
  up. Concurrent retries across two API processes created one durable job per
  file; two ingestion and two background processes published the expected
  sources exactly once per file and preserved read/search results.
- Final provider-recovery E2E passed in
  `pufferfs-provider-recovery-local-99736`, cleaned up. Lost preparation and
  submission responses preserved one paid job per accepted batch. A 65-page
  file indexed its first 64-page batch in a separate turn while staying hidden,
  then published all pages after the final batch. After a collector crash,
  partial-result recovery regenerated only the two failed pages, preserved the
  successful result objects and verified source order and public reads/search.

## Final checks and scope

- Passed: `go build ./...`, infrastructure TypeScript build, Python compilation,
  shell syntax checks, Go formatting check and `git diff --check`.
- Full corpus and targeted workflow/recovery E2Es used real external providers,
  production roles, Postgres and S3-compatible storage over real network
  boundaries. No unit tests, mocks or production fault hooks were added.
- Installer E2E was not rerun; installer code and release versions are unchanged.
  These runs establish correctness, not a production throughput benchmark.
- Whole-input document/container decoders still require their input parse;
  their artifacts and index turns are segmented. Native text additionally
  resumes its source/parser state and reuses stable append output.
- Deployment requires the documented coordinated migration 056 cutover, then
  a matching CLI release. See [production deployment](production-deployment.md).
