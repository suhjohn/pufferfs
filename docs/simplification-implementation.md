# System simplification implementation

Deployed to production from `535f66c` on September 18, 2026 UTC, retaining source
packing and verified append reuse for upload efficiency. The CLI release remains
v0.8.2; the backend now runs the simplified topology below.

## Target

API, ingestion worker and background worker are separate ECS processes.
Postgres atomically registers and schedules file work. Ingestion prepares
sources; background processing collects provider results, publishes extracted
text, and performs bounded cleanup. Providers are external black boxes.
One work record advances through extraction and publication. S3 keeps canonical
extracted chunks; provider requests are derived from those immutable chunks.

## Implementation checklist

- [x] Replace both SQS queues/consumers and Modal worker endpoints with database claims.
- [x] Consolidate collection, publication and maintenance into one background deployment with independent execution budgets.
- [x] Replace mutation artifacts with versioned canonical chunk publication.
- [x] Consolidate file work records and remove obsolete delivery/progress fields.
- [x] Simplify worker configuration and ECS deployment/cutover.
- [x] Use one namespace per root; reject upgrades that would discard existing shard routing.
- [x] Retain source packing and verified append reuse after measuring request and transfer costs.
- [x] Use one release manifest and one paid E2E gate per shipped revision.
- [x] Retire obsolete runtime compatibility and clearly archive old runbooks.
- [x] Adapt all affected E2Es without dropping behavioral coverage.
- [x] Run real-provider E2Es and synthetic scheduling/upload measurements.

## Verification contract

Existing CLI/API behavior remains the reference: capture acceptance, exact
reads, all search modes, updates, deletion, authorization, restarts, retries,
partial provider results, and durable cleanup. Tests for removed infrastructure
are replaced by tests at the new production process/network boundaries.
Tests never inject state directly into the database. No unit/mock tests.

The production cutover must stop old writers before applying the work-schema
migration; an old worker must not run against the new schema. Retained sources,
publication identities and cleanup records survive the change. No performance
or E2E result is considered verified until recorded below.

## Results

September 16, 2026 UTC, local Docker Compose with real external providers:

All 19 E2E scripts passed, including both vision cases: 20 suite variants.
After restoring source packing and append reuse, all three affected suites were
rerun successfully: capture/recovery, the full corpus, and retention/security.
The release/manual GitHub matrix now contains 15 suites, including base64
redaction through capture, search/read, updates and restarts. These local results preceded the production rollout recorded below. All local PufferFS test
projects were cleaned up after their external-resource cleanup succeeded.

| Verification | Result |
| --- | --- |
| Initial scheduler/publication corpus | Passed all nine original phases, including cleanup |
| Complete corpus after restoring packing | Passed again, all ten phases: native capture/transformation, live follow, 1,024-file capture, multipart recovery, search/read, authorization, outage, resumed and cleanup |
| Live lease renewal failure | Passed: real Postgres outage stopped the next provider batch; the same worker retried all three batches and exact 1,025-line reads passed on two APIs |
| v0.8.2 upgrade | Passed: old processes created published/pending work; production migrations preserved version/head/chunk identities; new workers completed addition, replacement and deletion |
| Installer/shared manifest | Passed: real installer and current CLI self-upgrade verified and installed released v0.8.2 archives through the generated manifest |
| Atomic capture handoff | Passed: replay through two APIs, concurrent duplicate requests, API restarts and two replicas of each worker role |
| Index recovery | Passed: lost write response, SIGKILL, expired claims, database restart, bounded claims, superseded writes, root deletion and late-write cleanup |
| Index batch replay | Passed: a killed worker replayed all canonical batches and exact reads survived both API restarts |
| API access | Passed: two API processes, permissions, keys, groups, membership races, browser sessions, roots, pinned Unicode reads and restart persistence |
| Native embedding batch limit (September 17 UTC) | Passed: 257 chunks written in groups of 256 and 1, SIGKILL recovery, exactly 257 native vectors, all search modes, exact reads and two API restarts. A separate v0.8.2-compatible hotfix passed the same E2E while retaining its original oversized mutation artifact. |
| Capture spool after restoring packing | Passed again: byte limits, shared source packs, verified append reuse, failed upload recovery, replacements, deletion and unchanged sync; empty-only root deletion with vectors enabled and disabled |
| Multipart recovery after restoring packing | Passed again: 32 MiB/two-part interrupted upload, expired session replacement and lost completion response preserved exact journaled bytes |
| Capture batches and metadata manifests | Passed: 128-file batching, concurrent replay, manifest integrity failures/retries, retained extent authorization and real-expiry cleanup |
| Retention/security after restoring packing | Passed again: actual upload expiry, source/artifact cleanup, mixed-pack append reuse, unaccepted uploads, multipart cleanup, old receipt replay, retained source extents and in-flight permission changes |
| Media | Passed all 28 real-provider fixtures and provider upload cleanup |
| Format variants | Passed all 51 fixtures and provider upload cleanup |
| Provider deletion | Passed: root deletion during an accepted provider submission preserved cleanup identities and did not resurrect indexing |
| Provider discovery | Passed: restart resumed the persisted listing cursor, found the original delayed job and indexed both pages without duplicate inference |
| Partial vision fallback | Passed: Gemini/Modal/Gemini/Modal page results survived collector restart, preserved order and passed public read/search |
| Cancelled vision batch | Passed: all four pages used the real Modal endpoint after Gemini cancellation, survived collector restart and passed read/search |
| Provider recovery | Passed the full fresh Gemini-only run: uncommitted preparation, accepted 64-page submission loss plus final page, collector crash, partial results, retry of only failed pages, unchanged successful artifacts and cleanup |
| Go build, Python compilation, shell syntax, Compose configuration | Passed |
| Web build | Passed using installed Node 24 (the initial Node 18 invocation was incompatible with Vite) |
| Pulumi TypeScript build | Passed |
| Production worker Docker image and GoReleaser configuration | Passed |

The final corpus run ID is `e8b4acb6aa554103a11587f8f0cc7065`.
Evidence is in sanitized `tests/e2e/artifacts/` logs/results. Targeted suites
verified the later single-namespace routing, non-root workers, cleanup and
replica changes. The final packed corpus run is `0fdf5d72960d49a1bbda13886bb264fc`;
all ten phases passed, including cleanup.

The retry test initially inherited the optional Modal fallback from `.env`.
Modal correctly completed the damaged Gemini requests, violating that test's
Gemini-only expectation. The harness now enables fallback only in the vision
overlay. A complete fresh recovery run passed with Gemini-only configuration;
the partial-failure and cancellation vision suites also passed separately.

## Measured tradeoffs and cutover limits

- The local 1,024-file capture phase took 7.24 seconds after restoring packing,
  compared with 35.43 seconds with one object per file (an earlier packed run
  took 6.30 seconds). Read-only inspection found nine source objects totaling
  36,265,229 bytes for the 1,024-file capture. These are local observations, not
  a production throughput promise. Source packing and verified append reuse avoid extra
  requests and full-file append uploads. Only new bytes count against the spool
  payload allowance.
- One worker row replaces transform/index rows. The phase's attempt count resets
  when extraction hands off to publication. Saved chunk content plus row format
  determine replay; no new mutation/cleanup artifact is written.
- The migration refuses active multi-namespace roots. The production preflight
  found zero incompatible roots; migrations 048–050 then completed successfully.
- New worker deployments retain bounded concurrency, renewable ownership and
  version-fenced publication. Removing them would permit stale/partial content
  to become visible. Original source extents/journals remain compatible.
- S3 lifecycle configuration retires legacy `mutations/` and
  `maintenance/root-deletions/` objects after 30 days of object age. An already
  old object can expire after deployment. Back up referenced legacy artifacts
  as well as Postgres if an old-version rollback must remain possible.
- Old writers were stopped before migrations 048–050. This deployment did not
  publish a CLI release or change provider spend limits.
- The cutover script checked other IAM roles before removing the stack-owned
  Modal OIDC provider. The AWS infrastructure update and ECS rollout succeeded.
- The default deployment keeps one ingestion and one background ECS task
  running, each with 2 vCPUs and 4 GiB memory. They are billed by AWS separately
  from the existing $100/month Modal workspace limit. Concurrency and task
  sizing are configurable; no cloud cost or throughput benchmark was run.

## Production verification — September 18, 2026 UTC

[Full deployment](https://github.com/suhjohn/pufferfs/actions/runs/35312124279)
and [CI](https://github.com/suhjohn/pufferfs/actions/runs/35309905229) passed
for `535f66c`. Both API tasks and the ingestion/background tasks use that
revision. Worker health checks passed, old ECS consumers are inactive, and
Postgres reports migration 50.

An isolated private synthetic root containing a 257-chunk text file and one
embedded image data URL completed capture and indexing in 12.21 seconds. Both
files published on their first attempt. All 257 lines matched indexed reads;
the image payload became `[base64 image]`; FTS, vector and hybrid search passed.
The root, its namespace and four storage objects were deleted afterward. This
is a smoke test, not a throughput estimate for an existing corpus.

The final local marker E2E (`543f14d277f5434893b00dc08fd3bc6d`) also passed
capture, restart, update/delete and cleanup against real providers. The entire
paid E2E matrix was not rerun for this final marker-only change.

The production preflight found 2,731 pending index jobs already at five attempts.
The new scheduler preserves their attempt counts and reports exhausted jobs as
failed; deployment does not reset or re-extract them. Existing extracted text
needs a new extraction to receive the base64 marker. No full-corpus reindex was
launched during deployment.
