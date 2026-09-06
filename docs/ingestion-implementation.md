# Ingestion replacement implementation ledger

This tracks implementation of the approved ingestion replacement specification.
The target is SQS-backed, independently operated transformation/collector/index
roles, immutable S3 sources and outputs, and per-file publication. Postgres is
the catalog and recovery ledger, not the queue. Nomic and the isolated query
embedding pool remain. This ledger is progress tracking, not a reduced scope.

## Current implementation (2026-09-05)

The repository now uses per-file capture and publication throughout the CLI,
API, SQS consumers and independently deployed Modal roles. The previous
generation pipeline, NATS backend, upload transports, Go chunking/indexing,
Postgres vector cache, monolithic Modal app and their configuration/UI have
been removed. Historical migrations, read-only recapture audits and cleanup
of previously stored artifacts remain.

Gemini 3.5 Flash-Lite Batch remains the media provider. Speaker labels are
best effort, as accepted by the user; evaluating another model is no longer
an open decision. Earlier diarization failures remain recorded below.

Only Docker Compose end-to-end tests are maintained. The retirement build
passes for CLI/API/consumer images, web and Pulumi; TypeScript checking,
Python compilation, GoReleaser configuration and whitespace checks pass.
Current verification is recorded below; earlier format, retention and
real-cloud results predate this cleanup.

Production upgrade still requires historical recapture validation, matching
CLI/API/worker deployment, worker secrets/queue configuration and verification
of the deployed principals' permissions. Existing queues must be drained and
Pulumi resource removals reviewed before applying the infrastructure changes.

## Verification history

The entries below record their original code and test versions.

### Legacy-code retirement (2026-09-05)

- Final-code native run `a9d80bf0de68498998a7b0a97a0b0eb3`, project
  `pufferfs-retirement-native`, exited 0. Capture passed in 0.46 seconds,
  transformation in 2.12, publication/read/FTS in 10.46 and cleanup in 1.07.
  Thirteen native files produced 394 verified chunks through separate CLI,
  API, SQS consumer and worker processes, with real Turbopuffer. Assertions
  also verified eight-field SQS messages, absent retired endpoints, no new
  generation/job rows, and local root identity when its name differs from
  the source directory. All project containers and volumes were removed.
- Index-recovery run `9c57a1088ca34d5c816bcad77c81ad1b`, project
  `pufferfs-index-recovery-local-20622`, exited 0. It verified HTTP role
  authentication, durable vector/mutation replay after a killed worker and
  lost response, stale writes after newer publication, root deletion during
  a held write, and scheduled cleanup of the late rows. Cleanup passed and
  all project containers and volumes were removed.
- Corpus run `9714e4bdabf24a7fb1569e5569fb65ec` passed capture, multipart
  recovery and verification of 1,024 files across 100 directories, all ten
  Gemini batches, retained sources, chunks, publication, search/read and ACLs.
  Its restart/malformed-delivery phases remain in progress. Its image predates
  the final unused diff/cache/interface removal; the index-recovery image
  includes it. Both predate the CLI root-name correction covered above.
- Shipping preflight confirmed the local database configuration matches the
  deployed production API. It contains 189 roots with historical publications
  and has no per-file catalog tables yet. A reader cutover therefore requires
  recapture/migration or explicit acceptance of temporary search unavailability.
  The new worker secret and production transform/index endpoints also remain
  unconfigured. No production resources were changed by this preflight.
- Media release assertions now follow the accepted best-effort speaker-label
  contract. Exact voice separation is no longer a gate; transcript content,
  utterance timestamps, silence, request scopes, retained originals and public
  retrieval remain mandatory. A fresh expanded media run is in progress.

### Expanded formats and real-cloud fixes (2026-09-05)

- Format run `4277353a0ef24336b16385338656678c`, project
  `pufferfs-formats-local-98876`, passed all **51 variants** in **1,189.31
  seconds**, cleanup in **7.37 seconds**, with unchanged application images.
  This includes real binary XLSB; Unicode/ANSI MSG; Word, presentation and
  spreadsheet templates/macro-capable containers; HEIF/AVIF/JPEG 2000; and
  multi-frame GIF/APNG/TIFF. Every original hash, extracted content/location,
  exact spreadsheet cell address, durable mutation and public FTS assertion
  passed; Office page reads passed too. All 39 real Gemini jobs completed and
  generated images were absent from S3. The fixture writer preserves Office
  package namespaces and writes actual XLSB bytes rather than relabeling XLSX.
  No active VBA or advanced JPX-compositing coverage is claimed. Subsequent
  driver-only changes validate completed siblings immediately instead of hiding
  their results behind the slowest provider job; that scheduling change was
  not part of this recorded pass.
- Actual cloud validation reproduced three deployment defects hidden by the
  local emulator/runtime adapter: binary libpq missed the system CA store,
  explicit S3 credentials discarded `AWS_SESSION_TOKEN`, and the Modal GPU
  container lacked its imported image-build helper. Worker images now install
  and select the CA bundle without disabling TLS verification, S3 config carries
  the session token, and both shared image recipes include their helper and
  requirements files. The real bulk role has initialized on CUDA/float16 and
  completed actual indexing; a full cloud scenario result is recorded separately.
- `tests/e2e/cloud_index.py` provisions only a temporary database/login, bucket,
  FIFO queues and Modal secrets/apps. It passes resource-scoped two-hour STS
  credentials to runtime roles, never the host's provisioning credentials.
  Provisioning supports an explicit database-router login suffix and checks the
  dedicated login before creating AWS resources. The failed session-token run
  `02b81ab80f144f92af0a3f94784b5cf7` also failed initial root cleanup; rebuilding
  only its API with the fix made cleanup pass in **3.31 seconds**. Its exact
  cloud resources, secrets and protected recovery file were then removed.
  No production IAM policy, endpoint or historical filesystem was changed.
- Media run `f4c875f9dcf840a7b0fe1af4c2e57d35` timed out awaiting Gemini after
  **1,205.27 seconds**; cleanup passed in **6.89 seconds**. Before timeout,
  read-only inspection of its published long recording still showed two voices
  merged into `speaker_0` in the first request, despite accurate starts near
  0, 15.02, 285.097 and 305.099 seconds. A stronger voice-inventory prompt did
  not solve that problem and was removed, not retained as extra unproven
  complexity. The two-voice assertions remain unchanged. After owned jobs were
  terminal, two remaining tracked uploads were explicitly deleted; three other
  IDs were unavailable without a fresh deletion acknowledgement, which does
  not prove physical erasure. This is not a media-suite pass or a provider
  retention-expiry test. A decision on best-effort Flash-Lite diarization versus
  evaluating a dedicated model remains open.
- Evidence is under `/private/tmp/pufferfs-retention-readiness.KqooX0/`:
  `format-variants-namespace-fixed.log`, `media-voice-inventory.log`, and the
  `cloud-index*.log` files. Failed and deliberately interrupted runs remain
  failed; later recovery does not retroactively make them full passes.

### Actual AWS / Modal GPU E2E passed (2026-09-05)

- Run `eb6c0db4478041579561c8a4eb15ddfc`, project
  `pufferfs-cloud-d2c5e78dc575`, passed in **183.14 seconds**, API cleanup in
  **1.84 seconds**, and exited 0. The actual bulk and independent query apps
  both logged `device=cuda, dtype=torch.float16`. CLI/API/native transform and
  SQS consumers ran as separate Compose processes; Postgres, S3/SQS, Modal GPU
  and Turbopuffer were real cloud services. No production endpoints changed.
- Assertions verified denial of account-wide bucket enumeration; 130 distinct
  vectors in at least three S3 packs; finite, normalized 768-dimensional floats;
  persisted acknowledged mutation bodies; FTS/vector/hybrid search; and a new
  captured version whose append reused all 130 prior vector locators and bytes.
  Public read returned the appended record. Public multipart APIs passed init,
  resume (real ListParts), signed part upload, completion and source-byte checks.
  Root deletion hid/removed its data, and the actual scheduled reconciler aborted
  its abandoned multipart session and recorded a successful prefix-cleanup pass.
- All this run's temporary database/login, bucket, four queues, two Modal
  secrets/apps, Compose resources and protected recovery credentials were
  removed. Earlier cloud runs also completed cleanup, including the explicit
  session-token recovery documented above. Bulk app:
  `ap-gzHqpkMmdKbFwodgUTVK1D`; query app: `ap-s7uXoMo5sfepCTXyYu8TWp`.
  Evidence: `cloud-index-scheduled-cleanup.log` in the directory above.
- This proves the isolated scoped runtime credentials, not the production ECS
  task role or default production Modal worker secret. It does not prove a
  populated schema upgrade, versioned-bucket physical erasure, production
  autoscaling, historical recapture/cutover or the latest full combined corpus/
  recovery/retention rerun. The successful format suite used earlier frozen
  application images; neither run retroactively validates another run's image.

### Capture authority at commit (2026-09-05)

- Catalog registration now checks current organization membership and role,
  API-key identity/expiry/scopes, root grants and group memberships after S3
  IO, as well as folder denies. The root lock and share locks on the exact
  authorizing rows last only through the metadata transaction. Group IDs are
  taken from the locked snapshot, not a second unlocked membership query.
  Ordinary reads and capture commits share the same pure root-permission policy.
- Fresh-image Compose project `pufferfs-permissions-local-20260905`, run
  `8edecb6117ea47bdbdb67a3742c7ce22`, passed all nine
  `capture-permission-races` cases in **40.26 seconds**, cleanup in **4.17
  seconds**, and exited 0. Each held a real S3 manifest response while revoking
  user/org/group grants, downgrading a grant, removing group membership,
  downgrading an editor, activating a role-dependent folder deny, removing
  organization membership, or deleting the API key. Every held request returned
  403 without catalog/proof/pack binding. Restored access allowed the same
  capture to complete through transform, real Turbopuffer publication and read.
  The dedicated phase used CPU/native files; it does not prove media or GPU
  behavior. All isolated project resources were removed.
- This closes the tested capture-commit revocation boundary, not every read or
  presigning race. Previously issued S3 URLs remain usable until expiry. The
  complete retention/corpus/provider/index suites have not been rerun with these
  latest authorization changes. No production resources or IAM were changed.
  Evidence: `/private/tmp/pufferfs-retention-readiness.KqooX0/capture-permissions.log`
  and `capture-permissions-services.log`; current Go builds, Python compilation
  and `git diff --check` pass.

### Source retention, capture fences and media accuracy (2026-09-05)

- Migration 039 adds version-to-pack byte-range edges, upload ownership,
  authorization deadlines and retirement tombstones. New registration validates
  ranges under the root lock; GC uses the same lock and only retires obsolete
  versions with terminal, aged work. Current heads, outstanding retries/provider
  work and unknown legacy extent mappings prevent deletion. Mixed live/dead
  packs stay whole. Unreferenced packs are deleted in bounded S3 batches after
  both retention and upload deadlines; exact-key multipart sessions are aborted.
- The local CLI can reset a definitively unusable upload identity and re-upload
  its immutable spool without changing bytes, digests, capture ID or base
  versions. It cannot fabricate missing remote append bytes. Legacy unaccepted
  uploads without provenance require a fresh upload with the new CLI; retained
  same-file ranges become reusable after bounded manifest backfill. A populated
  migration is not yet verified. Manifests and small audit metadata are retained.
- A real manifest-PUT response fault reproduced an ACL race: run
  `6ad983e941ec47ab9f00e296d02b18f5` accepted HTTP 202 after a folder deny committed.
  Rechecking denies in the catalog transaction and serializing deny insertion
  with the root lock made fresh run `b86d1f65109d484c909a3f8622c9a5ea` pass in
  2.15 seconds (cleanup 0.69 seconds). This does not prove other revocation races.
- Fresh migration-039 run `e71ea8adf31c4eb288f51d4abf858301`, project
  `pufferfs-retention-local-60802`, has passed spool/conflict/receipt checks,
  obsolete artifact cleanup, cross-tenant uploads, the ACL race, packed sibling
  and capture-ID graft rejection, another uploader's unbound-pack rejection,
  legitimate second-writer same-file reuse, and embedding-cache expiry/reuse.
  Source-GC assertions then passed after the actual 15-minute authorization
  expiry: obsolete/unaccepted packs and abandoned multipart sessions were
  removed, mixed-pack append reuse and old receipt replay remained valid, and
  the original pending bytes were re-uploaded despite the changed live file.
  The assertion phase passed in **932.49 seconds**. Initial cleanup failed on
  the 039 FK in 0.75 seconds; the same API-image upgrade through 040 fixed it,
  and cleanup passed in 4.43 seconds. All isolated project resources were
  removed. Later legacy-reupload handling, all-expired-pack error aggregation,
  and revision-pinning changes are not verified by that run's original images.
- Expanded media run `2fc7113e402a43a09cb5997ad20160c6` indexed all 28 files but
  failed the long-clip speaker assertion (332.38 seconds). The WAV was correctly
  bounded to 10,080,078 bytes; independent FFmpeg inspection confirmed speech
  at 0/15/285/305 seconds. Gemini reported the 285-second utterance near 44 seconds.
  A more explicit elapsed-time/voice-label prompt still failed in run
  `0f8dcb4aaba44cccbfee1dd7aad110f3` (510.60 seconds). Both cleaned up successfully;
  their transcripts remain diagnostic evidence, not successful suite results.
- New v2 captures use 60-second media clips, still Gemini 3.5 Flash Lite Batch.
  Original v1 extractions retain 300-second boundaries during regeneration.
  Capture replay keeps the originally recorded extraction revision rather than
  scheduling new paid work after an upgrade. E2E expectations now check silence,
  utterance timestamps within three seconds and voice-label equivalence within
  each request. Run `f34a62278bd64fd7a58d9fd320dcc1e8`, project
  `pufferfs-media-local-73285`, indexed all 28 files and 33 provider requests but
  failed the two-voice assertion again (277.61 seconds). The 285-second speech
  was now located at 285.454 seconds; the first clip still used only one speaker.
  No fixture expectation was weakened or provider changed. Diarization quality
  remains a release gate; this is not a passing expanded-media suite.
- That run's API cleanup exposed a migration-039 cascade ordering error:
  deleting a root checked the pack-reference FK before both cascade paths had
  finished. Migration 040 makes that constraint initially deferred; pack GC
  still retains its metadata tombstone. A fresh API image applied 040 to the
  populated test database, and the identical public cleanup passed in 4.72
  seconds. The project was then removed normally. The first failed cleanup is
  retained in the results. The separate source-retention run required and
  passed the same 040 API upgrade for cleanup after its assertions completed.
- Read-only cloud audit found the configured `pufferfs-workers` Modal secret
  absent from `main`. Separately auditing the existing legacy `pufferfs` secret
  in temporary sandbox `sb-eBafBY0fUs4PoQltY4sZB2` confirmed STS identity
  `arn:aws:iam::940827433648:user/root` (an IAM user, not the AWS account-root
  principal) and successful real S3 object/multipart listings of an empty random
  audit prefix. Both new SQS URL variables were missing. No source bytes were
  read, queue messages received, credentials changed or IAM policies applied.
  This is not a configured new-worker, write-permission or bulk-GPU E2E pass.
  An earlier probe failed because boto3 clients need explicit closing; that
  diagnostic bug was fixed before the successful read-only calls.

### Retention, deployment separation and security follow-up (2026-09-05)

- Accepted local source-pack bytes now become eligible for removal only after
  durable registration acceptance and installation of all local heads. Retained
  heads refer to S3 extents for append reuse. The cache keeps at most 64 receipts;
  incomplete unpublished spools are discarded under the sync lock. A configurable
  per-cache spool limit defaults to 2 GiB, reserves journal space, and never evicts
  pending/conflicted captures. This is local retention, not cloud artifact GC.
- `query_app.py` is an independent Modal app with only endpoint authentication,
  not the worker/provider/database secret. Query and bulk share pinned Nomic
  loading code. Legacy `app.py` no longer defines the query class; deploy the new
  app and switch API URLs before redeploying the old app. No production rollout
  was performed. The final query default is explicit CUDA; Compose selects CPU.
- Corpus rerun `bb7f701193c649ddb4c325e0e99998ff` is a **failure**, not a pass.
  All corpus/provider transformations completed, but concurrent calls in the
  Compose adapter corrupted Nomic's position-cache dimensions (17 versus 16).
  The astronomy index work exhausted five attempts and reached the real emulator
  DLQ. The driver was stopped after that terminal-work evidence; the script
  exited 137 and cleanup passed in 7.55 seconds. Application images were not
  changed during the run. Its logs are retained alongside the new validation
  artifacts under `/private/tmp/pufferfs-retention-readiness.KqooX0/` and
  `/private/tmp/pufferfs-query-auth.6btlRw/corpus.log`.
- The Compose HTTP adapter now serializes invocations per container, matching
  Modal's default rather than adding unsupported shared-model concurrency.
  Current Dockerfiles built successfully, including fresh vector dependency and
  model-cache layers. Run `41029430e9d54e1ea2f4889b4db8e7ce`, project
  `pufferfs-retention-local-73230`, passed retention/security in 15.78 seconds,
  then cleanup in 1.10 seconds, exiting 0 with no project containers/volumes.
  It checked spool limits, incomplete cleanup, removal of accepted packs,
  original hashes, append extent reuse, public reads/FTS, malformed auth across
  all four HTTP roles, concurrent Nomic query requests and cross-tenant source
  signing/renewal/completion/reference forgery. The final explicit query-device
  setting and startup log were added afterward and require their rerun.
- The previously completed query-auth index suite
  `d0a6facc96ec4379a2220a71a923e7b9` passed all crash/replay, stale-write and root
  deletion phases with exit 0; all four endpoints rejected malformed credentials.
  Its evidence remains `/private/tmp/pufferfs-query-auth.6btlRw/index-evidence.tar.gz`.
- GitHub's `e2e` environment was created with the authenticated repository owner
  as required reviewer (manual self-approval allowed). Both provider secrets
  were populated securely from the existing local configuration and their names
  verified. These are not newly issued dedicated CI provider keys. No workflow
  was dispatched/approved. A generic configuration script preserves existing
  approval rules and refuses to install secrets into an unprotected environment.
- The isolated Modal app `ap-Prl805xfyOXZfX0PjZl4CE` built its actual query image,
  rejected invalid credentials, returned normalized 768-dimensional vectors to
  eight concurrent HTTP requests, and stopped normally. No production app/API
  URL changed. This does not verify bulk GPU indexing or deployed AWS access.
- Read-only AWS inspection confirms public-access blocking and AES256 encryption
  on the deployed artifacts bucket. It has no bucket policy. IAM simulation
  allows source get/put but denies `s3:ListMultipartUploadParts` and
  `s3:ListBucketMultipartUploads` for the deployed ECS role. Infrastructure source
  now includes both permissions and an HTTPS-only bucket policy; these changes
  are **not applied**. No claim of deployed least-privilege/tenant IAM isolation.
- Still required: cloud source/chunk/vector/provider-input retention, pending and
  conflicted spool pressure/receipt pruning E2E, provider lost-response/partial
  retry tests, long media and expanded-format scenarios, packed-file/ACL race
  security coverage, refreshed full suites, bulk GPU validation and authorized
  AWS changes. This work does not reduce the original implementation objective.

### Explicit CUDA verification and provider cleanup implementation (2026-09-05)

- Final query configuration passed another fresh-image retention/security run:
  `3785c8039c2348d2ba7888a71fca6027`, project `pufferfs-retention-local-84604`.
  Verification took 15.62 seconds, cleanup 1.03 seconds; the command exited 0.
  The isolated Modal rerun `ap-ktm5zxUZIprESi4pPlF6Sk` logged
  `device=cuda, dtype=torch.float16`, rejected four invalid credentials, and
  returned normalized vectors for eight concurrent HTTP queries. It exited 0
  and stopped the temporary app. This proves the query role on real GPU, not
  the bulk index role or cloud autoscaling/load behavior.
- Migration 035 and `provider_cleanup.py` track exact temporary Google upload IDs
  and their batch associations independently of root/request-row lifetimes.
  Page/clip uploads and batch JSONL inputs are registered without
  saving media bodies in Postgres or S3. Batch file mappings are inserted in one
  short transaction, not one connection per request.
- Scheduled collection now deletes tracked files only after transform work,
  all referencing batches and unfinished extraction requests release them.
  Deleted-root batches are cancelled, but files stay pinned until the provider
  reports a terminal job. Cleanup is bounded, retries failed deletes, accepts
  only provider 404 as already absent, and never lists/deletes unrelated account
  files. Upload responses lost before their exact IDs can be recorded remain an
  orphan/expiry gap; ordinary S3 source/chunk/vector GC is still unfinished.
- Full corpus verification now checks real provider upload disappearance through
  the scheduled production collector after durable extraction. Fresh full run
  `pufferfs-e2e-local-1-90387`, run `abc26e90a792428bac9059ecf2adde14`,
  **failed** at provider cleanup after passing extraction, original-source,
  search, Nomic and ACL assertions. Cleanup incorrectly included Google's
  generated batch-result files, whose deletion returns HTTP 400. The command
  exited 1, and external cleanup passed. Log:
  `/private/tmp/pufferfs-retention-readiness.KqooX0/full-corpus.log`.
  Existing retention/security and cloud-query passes predate provider cleanup.

### Provider lifecycle correction and additional E2E scenarios (2026-09-05)

- The real-provider reproduction `pufferfs-media-local-72013` established that
  Files.delete rejects generated batch-result IDs with HTTP 400 (its 40-character
  ID limit). Removing one exact, terminal synthetic batch made batches.get and
  files.get return 404, but **files.download still returned the result bytes**.
  This is diagnostic evidence, not a passing E2E or proof of erasure. The run
  was stopped after diagnosis; its remaining live batch was cancelled by scoped
  cleanup. Cleanup was rerun with idempotent handling of already-absent jobs.
- The upload ledger now records only our page/clip and JSONL uploads. Generated
  results are not silently marked deleted or subjected to endless invalid
  deletion requests. Google's [Batch documentation](https://ai.google.dev/gemini-api/docs/batch-api#retrieving-results)
  documents six-week retention for results, distinct from the uploaded Files
  API's 48-hour expiry. Immediate generated-result erasure is not promised.
- Added a separate provider network-relay Compose topology and suite. It forwards
  unchanged requests to real Gemini, can hold one submission request/response,
  and exposes only durable IDs, hashes and transport state. The suite kills the
  actual transform process after provider acceptance, then checks exact job
  reconciliation and absence of a second paid create. Its partial-batch scenario
  deletes alternate run-owned uploads after the submission envelope is in
  flight; real Gemini, not a stub, must produce the per-request failures.
  Retry assertions check unchanged successful-page artifacts and ordered chunks.
  These scenarios are implemented; execution results are recorded separately.
- Expanded the focused media suite to 26 alternate-container/codec fixtures plus
  a 315-second, two-voice recording. Expected clip boundaries, terms and speaker
  counts live in fixture data. The verifier checks timestamp offsets and distinct
  request-scoped speaker labels through real S3 chunks and public search. AMR
  remains uncovered because the runner has a decoder but not an encoder.
- CI uses five isolated suite jobs (maximum two concurrently) with distinct
  artifacts and per-suite timeouts. The protected environment still gates real
  provider credentials. No GitHub-hosted execution is claimed.
- Both fresh-image runs were interrupted when the host ran out of disk space
  and OrbStack/Docker stopped at 2026-09-05 17:47 PDT. These are **not passes**.
  Provider project `pufferfs-provider-recovery-local-90663`, synthetic run
  `200e215473274d5a9ddd8ce8a19cf9c7`, passed the accepted-but-held response
  assertion and the actual transform-process kill/release. Postgres subsequently
  recorded the same remote job through reconciliation, but final publication
  and partial-retry phases did not complete. Media project
  `pufferfs-media-local-2968` reached driver startup; no media result is verified.
  Both scripts exited 1 and could not run external cleanup because Docker's
  socket disappeared. Preserve both projects' volumes for cleanup after Docker
  is restored; do not prune them. Logs are `provider-recovery.log` and
  `expanded-media.log` in the retention-readiness scratch directory above.
  An exact-job cancellation was requested through Google's HTTPS API after the
  host failure; the subsequent status was terminal `BATCH_STATE_SUCCEEDED`
  (completion raced cancellation). No successful cancellation or file erasure
  is claimed. Tenant/upload cleanup still needs the retained Compose state.
- After the interruption, Python compilation, provider-recovery Compose config
  validation, CI YAML parsing and `git diff --check` passed. No further builds
  or E2Es were started on the full disk. Only tiny task-owned scratch artifacts
  were found outside Docker; unrelated files, caches and Docker data were not
  deleted to make space. Restore disk space and Docker before resuming the two
  retained projects or making any deployment-readiness claim.
- Disk space recovered to approximately 2.4 GiB after the daemon stopped.
  OrbStack was restored for cleanup only, with no additional build/test run.
  Both interrupted tenants were removed through their actual API cleanup
  workflow. In the provider project, the ordinary scheduled collector then
  observed the removed root and terminal provider job, acknowledged deletion of
  its two tracked uploads, and persisted their deletion timestamps. Subsequent
  SDK and HTTPS reads/deletes returned 403, not 404: Gemini masks absent upload
  IDs as "no permission or may not exist". Both Compose projects were removed
  after cleanup; no unrelated Docker images, volumes or personal files were
  pruned. This cleanup observation does not turn interrupted suites into passes.
- The verifier now first requires the collector's acknowledged deletion record
  and only then accepts 403/404 as post-deletion inaccessibility. Production
  cleanup still treats 403 as failure, never evidence of erasure. Input refresh
  may regenerate an unusable 403/404 input from authorized S3 sources, with the
  replacement upload still subject to the same provider credentials. That
  refresh change requires a fresh E2E run. Known remaining gate: an externally
  deleted upload or lost delete response can produce ambiguous 403 indefinitely;
  an explicit provider-expiry/retry policy is still needed. The new partial-batch
  test must not be declared passing until this lifecycle is handled truthfully.

### Explicit provider expiry and lower-churn builds (2026-09-05)

- Migration 036 separates acknowledged upload deletion from passage of the
  provider's expiry deadline. Upload registration persists the returned
  `expirationTime`; missing metadata/legacy rows conservatively use 48 hours
  after registration under the uploaded Files retention policy. Reusing an
  upload in another batch never extends its deadline. A bounded scheduled
  expiry scan records `expired_at` without calling DELETE or removing any source,
  request mapping or batch recovery state. Ambiguous 403s remain errors until
  deletion is acknowledged or the expiry deadline passes; they never prove erasure.
- The partial-retry test now captures the real expiry timestamps before its
  explicit input-deletion fault. It checks that any ambiguous cleanup outcome
  retains that exact deadline without claiming premature deletion/expiry.
  Confirmed deletions, elapsed deadlines and pending external deletions are
  reported separately. This change is unverified by a complete provider E2E;
  real elapsed expiry is not simulated or claimed as covered by a short run.
- Added a 70-capture CLI scenario requiring exactly 64 retained receipts, absent
  accepted local pack bytes, continuing reuse of the oldest remote extent, and
  a correct final public read. It uses ordinary captures/appends, not injected
  journals or database fixtures. Its fresh retention run is tracked separately.
- Both Go Docker stages now copy only Go sources and embedded migrations, so
  documentation/Python/test edits do not invalidate Go compilation. E2E fixture
  dependencies now precede application/test source copies. Read-only BuildKit
  inspection found six 155.9 MB full-source snapshots and twelve compile-cache
  records belonging to the three immediately preceding runs. Exact-ID pruning
  removed only those 18 unused, private cache records (2.208 GB reported), which
  can be regenerated from the repository. No image, volume, evidence archive,
  unrelated cache or personal data was deleted. Free host space reached 3.9 GiB.
- Fresh retention run `53d912786fa847d48aa340c5052b0423` passed in 23.45 seconds
  (excluding build/startup), with cleanup passing in 1.55 seconds. Project
  `pufferfs-retention-local-39622` was removed; sanitized log:
  `/private/tmp/pufferfs-retention-readiness.KqooX0/expiry-retention.log`.
  This includes the 70-capture receipt/append scenario, not provider expiry or
  the subsequently added pending/conflicted-spool pressure scenario.

### Cold embedding-pack retention and unaccepted-spool coverage (2026-09-05)

- Migration 037 adds a pack-level cache lifecycle. New pack identities are
  single-use and registered before upload, so an interrupted upload can be
  collected. Cache hits refresh/lock one pack through its bounded S3 range read;
  uploads hold that same row lock through locator publication. Scheduled cleanup
  skips locked packs, atomically retires cold packs/removes their cache locators,
  then sends one S3 batch delete for at most 100 keys. A retired identity is never
  reused. Failed/unacknowledged deletes retry after five minutes; retained
  tombstones repeat successful deletions daily to catch late network writes.
- The ordinary configurable cache TTL defaults to 30 days, minimum 60 seconds.
  Retirement does not delete source/chunk/publication/work state. Replayable
  mutations already contain their vectors, while a subsequent cache miss may
  legitimately require fresh encoding. This is embedding-cache GC, not yet
  obsolete source-pack/chunk GC, physical erasure in versioned buckets, or a
  bound on tombstone metadata growth.
- Added black-box spool pressure coverage: disable the actual S3 upload proxy,
  retain a 3 MiB pending capture, register a competing file through the public
  upload/API workflow, then retry and explicitly force archive. An 8 MiB budget
  must reject a second large capture without evicting conflict bytes, while a
  smaller replacement can publish and be read. No journal/DB fixture injection.
- The expanded retention script sets the ordinary cache TTL to 120 seconds and
  waits for the unchanged real-time maintenance schedule. Two Nomic publications
  must reuse one pack; expiry must remove it while search and vector-containing
  mutation artifacts remain intact; a later cache miss must use a new pack.
  These additions pass syntax/diff checks but still require the fresh Compose
  run. The earlier receipt/retention pass does not verify this new behavior.

### Provider recovery passed; obsolete extraction retention added (2026-09-05)

- Run `83424dee6afb4978a9843e52bd0f0e18`, project
  `pufferfs-provider-recovery-local-45433`, completed every real-provider phase
  and exited 0. The lost accepted response recovered the original job in 273.59
  seconds, with one submission and unchanged upload identity. The partial batch
  returned two actual successes/two actual input failures; the retry published
  all four pages in order in 333.00 seconds without rewriting successful result
  objects. Successful pages remained at attempt one, failed pages reached attempt
  two with new uploads. Read/source/search assertions passed. Cleanup separately
  reported eight acknowledged upload deletions and two explicitly test-deleted
  inputs retaining their actual future expiry deadlines after ambiguous 403s.
  No elapsed 48-hour expiry or generated-result physical erasure was claimed.
  API cleanup passed in 1.68 seconds and the isolated project was removed.
- Migration 038 adds an obsolete-extraction artifact tombstone. Retirement
  requires a version that is neither captured nor indexed, terminal extraction
  status, no unfinished/failed work, no live referenced provider batch, and the
  configured age on extraction/work state. The default is 30 days (minimum 60
  seconds). Current versions, pending retries, sources, catalog/proofs, and
  index-cleanup cutoff artifacts are not removed. Exact extraction-scoped chunk
  and mutation prefixes are swept with bounded S3 listing/batch-delete/multipart
  cleanup, reusing the existing validated prefix IO. Successful passes repeat
  daily for late writes; failures/partial passes retry after five minutes.
- Added a real-time obsolete-artifact scenario to the retention suite: publish
  and append, wait for old chunks/mutations to disappear while current artifacts
  remain, reconstruct both source versions, append again reusing the original
  pack, and verify the public read. This does not implement obsolete source-pack
  GC or verify cleanup races with paused in-flight work/provider retries.
- Fresh images and migrations 037/038 are building for
  `pufferfs-retention-local-87747`; sanitized log
  `/private/tmp/pufferfs-retention-readiness.KqooX0/artifact-retention.log`.
  The new pending/conflict, embedding-cache, and obsolete-extraction scenarios
  are not passing results until this driver and external cleanup finish.

### Expanded retention passed; long-media fixture failure isolated (2026-09-05)

- Fresh run `97ebd39610b74bfea94e876b9189fc1c`, project
  `pufferfs-retention-local-87747`, passed in 331.52 seconds; cleanup passed in
  3.29 seconds, exit 0, and all isolated project containers/volumes were removed.
  This verifies migrations 037/038 on a fresh database, pending/conflict spool
  preservation, 64-of-70 receipt pruning, obsolete chunk/mutation removal,
  source/append/read preservation, shared embedding-cache reuse and actual
  scheduled expiry, unchanged vector-containing replay artifacts/search, and
  re-encoding after cache eviction. Reconciler logs reported one retired/deleted
  embedding pack and nine obsolete extraction sweeps, with no cleanup failures.
- Added synthetic AMR-NB encoding through Debian OpenCORE only in the fixture
  runner. The expanded suite has 27 short formats plus a 315-second recording.
  Run `pufferfs-media-local-4706` failed with exit 1 at fixture generation and
  stopped OrbStack after exhausting host disk. Read-only inspection found the
  intended long WAV was **3,953,917,952 bytes**, reporting **123,559.933563 seconds**
  of 16 kHz mono PCM, rather than 315 seconds. No roots or provider batches had
  been registered; only the synthetic organization/users existed. This is a
  fixture failure, not evidence against or for production media extraction.
- Replaced unlimited padding/time-based trimming with finite sample padding,
  sample-count trimming, timestamp regeneration, an output-size ceiling, a
  subprocess timeout, and exact WAV-header/size assertions before capture.
  The expected recording contains exactly 5,040,000 samples (about 10 MB).
  Removed only that reproducible malformed fixture from its isolated volume.
  Pruned four exact unused private Go build-cache entries from the preceding
  provider build (424.2 MB reported), retaining current images, diagnostics and
  unrelated user caches/data. Docker restarted; unrelated prior-running
  containers resumed normally. Host free space recovered to 4.1 GiB.
- The interrupted media tenant (`2f5b9cfe5f754afdab6b8ea439f1cfda`) was cleaned
  through the API after restarting Postgres/API; cleanup passed in 0.39 seconds
  and the isolated project was removed. An initial cleanup attempt failed
  because the API had started before Postgres finished crash recovery; that
  failure remains in the report. No root or paid provider job existed to erase.
  A fresh full media run is required;
  sanitized log is
  `/private/tmp/pufferfs-retention-readiness.KqooX0/expanded-media-amr.log`.
  A documentation update failed while disk was exhausted; its original file
  remained intact. No production deployment, commit, push or cutover occurred.

### Real index schema compatibility fix (2026-09-05)

- The real-provider run found HTTP 400 on both file reads and FTS: new
  per-file namespaces do not define `valid_from_generation_seq`, but the API
  sent an OR predicate referencing legacy generation fields anyway. Mock-era
  tests had not caught the actual provider schema validation.
- Reads now use the existing deployment-mode boundary. Legacy mode sends only
  generation filters; file mode sends only extraction filters and validates
  candidate publications in Postgres. Exact file reads use one published
  extraction ID throughout pagination; missing/unpublished/deleted paths fail
  closed. File mode no longer fetches an unused visible root generation.
- This supersedes earlier ledger claims of mixed legacy/per-file read fallback.
  Historical recapture and publication coverage must be validated before
  switching readers to file mode. Mixed read compatibility is not claimed.
- `go build ./...` passed. In Compose project
  `pufferfs-e2e-local-1-29253`, the driver was paused while workers and the
  collector kept running, then the API image was rebuilt/replaced. Public API
  checks returned two PDF pages, three reassembled Unicode lines, and FTS
  results. The existing test driver resumed with original artifacts and Batch
  jobs; this is a debugging run across an API upgrade, not a clean-image full
  suite pass. No unit tests, production deployment or personal-source ingestion.

### Published-content authorization E2E (2026-09-05)

- Added a production-path scenario against the captured native root. It creates
  and removes ACLs through authenticated HTTP APIs, reads/searches the real
  index, checks catalog filtering and proof-write rejection, and verifies that
  unrelated folders and the catalog/work identities are unchanged.
- Actual key provisioning returned HTTP 400 for the documented `acl:write`
  scope. The scope allowlist now accepts `acl:read` and `acl:write`. ACL lookup
  now includes documented `user:<id>` and wildcard targets alongside historical
  bare user IDs and role targets; no new permission subsystem or state was added.
- The initial fixture incorrectly treated a filename as a folder prefix. It was
  corrected to `/sessions/` before verification; file-specific ACL semantics
  were not added. The corrected four-target scenario passed in 1.94 seconds
  against the rebuilt API while the main driver was paused. Failed attempts
  remain in the phase log rather than being relabeled as passes.
- Added future full-run assertions for searchable replacements during an outage,
  immediate hiding of capture-time deletions, and current-only search after
  recovery. These new assertions require the next runner image; the currently
  running main driver still uses its original test image. The authorization
  scenario itself ran from the updated black-box driver mounted read-only.

### First completed real-provider debugging run (2026-09-05)

- Run `4405c911bb744f5995df926089918d85`, project
  `pufferfs-e2e-local-1-29253`, completed `scripts/test-e2e.sh` with exit 0.
  Main verification passed in 1,188.71 seconds including asynchronous Batch
  waiting and manual driver pauses. Capture accepted 1,024 files in 9.18 seconds
  against local S3/SQS; this is not an AWS throughput benchmark.
- Real Gemini output passed content/hash/location checks for PDF, DOCX, PPTX,
  PNG/JPEG/WebP/TIFF and WAV/MP3/MP4. All ten batches completed. Native bytes,
  generic JSONL, spreadsheets and structured files also published. Real Nomic
  produced 768-dimensional embeddings and vector/hybrid search ranked the
  astronomy fixture first; the no-vector roots produced no embeddings.
- Restart recovery passed in 58.72 seconds. Read-only follow-up queries returned
  no old `cobalt` result and one current `amber` result for the replacement;
  deleted/old rename paths returned no hits and the new rename path did.
- Malformed delivery passed transformation and real-index publication. The two
  healthy receipts completed while the malformed receipt stayed unacknowledged;
  actual receive logs confirmed a mixed three-message batch. This does not
  prove eventual DLQ redrive.
- API/provider cleanup passed in 7.25 seconds. No containers or volumes remain
  for that project. Sanitized evidence, including failed exploratory ACL
  attempts, is archived at `/tmp/pufferfs-e2e-debug.j21TPo/evidence.tar.gz`.
  Sources were synthetic, and no production deployment or personal recapture
  occurred. The clean rerun included updated authorization and stale-search
  assertions from startup, but failed before reaching them (see below).

### Clean corpus run: MP3 content failure (2026-09-05)

- Run `d3a97c8538174d8e9779007321286583`, project
  `pufferfs-e2e-local-1-32778`, passed capture, handoff repair, native extraction,
  follow, completed-work replay and multipart recovery. Main verification then
  failed after 459.15 seconds: `sample.mp3` chunks did not contain the expected
  word `orchid`. This is not a full-suite pass and its later scenarios did not
  run. The previous debugging run passed the same fixture.
- Cleanup passed in 6 seconds and removed the project. The transcript and exact
  provider handles had not been retained before deletion, so the cause is not
  yet established; do not assume either a production decoder defect or harmless
  transcription variation. Added bounded synthetic-content/location and provider
  handle diagnostics for future assertion failures, without relaxing the test.
- Sanitized evidence is archived at
  `/tmp/pufferfs-clean-e2e-failure.789eJ1/evidence.tar.gz`. A targeted, real-provider
  MP3 reproduction is still needed. The independent consumer-fix recovery run
  has also terminated; its narrower result is recorded below.

### In-flight index recovery and active-lease delivery bug (2026-09-05)

- Added `scripts/test-e2e-index-recovery.sh` and an optional Compose override.
  A test-only HTTP relay forwards actual index writes to a fixed real
  Turbopuffer HTTPS origin. It can hold a request or response but cannot invent
  provider success, import application code or access the database. The driver
  uses real CLI/API/SQS workflows; the shell kills/restarts actual worker
  containers. No application rows, leases or receive counts are manipulated.
- Baseline project `pufferfs-index-recovery-local-80581`, run
  `f93fd30b750745a394b05089c059f9d3`, reproduced lost-acknowledgement recovery:
  real Nomic vectors and a replayable mutation existed, Turbopuffer accepted
  the write, publication remained unacknowledged, and public search hid it.
  The Nomic process was then killed before receiving the response.
- Work `85c89cd3-a557-54c8-a5fb-2ca67f0a609c` held a database lease until
  `20:27:16 UTC`. The consumer logged EOF at `20:22:18`, then `busy` at
  `20:23:24`, `20:24:24`, `20:25:25`, and `20:26:26`. The recovery assertion
  failed after 303.67 seconds because the message entered its DLQ. This is a
  delivery/lease interaction, not a failed vector or provider computation.
  [SQS counts receives against its redrive limit](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-dead-letter-queues.html),
  so repeatedly treating a valid active lease as failure exhausts retries.
- The file consumer now keeps its existing bounded SQS slot and visibility
  heartbeat while observing another active attempt. It checks the same work's
  durable state every 15 seconds, without renewing the database lease. Expiry
  permits a new worker claim; completion permits acknowledgment. Superseded
  captures and deleting roots bypass the wait so stale work can finish its
  handoff. A typed `busy` response also covers the race after the initial read.
  This follows [SQS visibility extension semantics](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html),
  without changing queue limits, resetting receipts or treating Postgres as a
  work queue. `go build ./...` passes.
- Baseline cleanup passed in 1.03 seconds and removed its isolated resources.
  Evidence is archived at
  `/tmp/pufferfs-index-recovery-baseline.YXm6i7/evidence.tar.gz`. The later stale
  in-flight write phase did not run in that failed baseline and remains unproven.
  The separate clean corpus run did not include this newer consumer fix.

### Recovery replay assertion and local resource failure (2026-09-05)

- Consumer-fix run `fae26d2fa72649bcb30b5615d8d69b8a`, project
  `pufferfs-index-recovery-local-29980`, recovered publication after normal
  lease expiry. Its assertions confirmed attempt two, a new ownership token,
  and unchanged durable vector/mutation objects. It then failed on unequal
  compressed HTTP-body hashes. The installed Python 3.12 SDK uses
  `gzip.compress` without fixing its timestamp; wire bytes are therefore not
  mutation identity. Evidence remains at
  `/tmp/pufferfs-index-recovery-framing.OrTWB3/evidence.tar.gz`.
- The relay now records both wire and exact decompressed-payload hashes plus
  gzip timestamps, forwarding the original bytes unchanged. Replay assertions
  compare payload bytes without parsing/reserializing them. This change needs
  an executed pass; it does not retrospectively turn the failed run green.
- Added a focused, real-provider media Compose driver using the same three
  fixtures as the full corpus. It retains every tiny synthetic transcript and
  request mapping before content assertions, plus hashes, diarization locations,
  original-source retention and public search checks. No mock provider or
  weakened expected-content assertion was introduced.
- Both new runs terminated during image building when Docker's socket vanished.
  The host reported only 117 MiB free and OrbStack was stopped. These projects
  (`pufferfs-index-recovery-local-97218`, `pufferfs-media-local-5996`) never
  reached application startup or provider submission. Their build logs are
  `/tmp/pufferfs-index-recovery-payload-20260905.log` and
  `/tmp/pufferfs-media-repro-20260905.log`; no test pass is claimed.
- Removed two verified-unused, task-created temporary dependency environments
  (289 MiB; no sources/evidence) and restarted OrbStack successfully. Obsolete
  image tags from four completed synthetic runs were removed without touching
  containers/volumes from other projects. These disposable dependencies/images
  are rebuildable; test evidence and source files are retained. Free disk space
  remains a validation constraint, not a credential or provider blocker.
- Moved the Compose driver copy after vector dependency/model caching so future
  test-only edits do not invalidate those heavyweight layers. Python syntax,
  shell syntax and whitespace checks pass; this does not establish runtime
  validation. The full goal, migration and cutover remain incomplete.

## Latest verification and generality constraint (2026-09-05)

- The completed frozen images were reused without rebuilding or changing the
  application during execution. Run `6ea97387f73e46e597018b7315b6ba67` passed
  lost-response recovery (304.88 seconds), a second owned attempt with unchanged
  vector/mutation artifacts, identical decompressed replay bytes, all search
  modes and a delayed stale write after newer publication. Cleanup passed.
- Run `a063c31b89824d4b9457e118fd5f72c3` independently passed root deletion
  during an in-flight write, denial of public access to late physical rows,
  and their removal by the real scheduled reconciler. Deletion tombstones and
  durable cleanup mutations survived catalog deletion. Cleanup passed.
- Media run `e2072bf2adeb4db7993e5ce61583a69b` retained the MP3 transcript and
  exact provider mapping. Reading that same provider job's original result
  confirmed a proper-name transcription difference, not chunk rewriting.
  Evidence is in `/tmp/pufferfs-media-evidence.PvvrJg/`. Subsequent run
  `c150110e90104a9285ea3abc10c76fee` passed ordinary spoken-content, location,
  hash, source-retention and public search assertions in 151.56 seconds.
  Neither result establishes perfect speech recognition.
- The user explicitly forbids instance-specific behavior, including test-scenario
  accommodations. `AGENTS.md` now records this constraint. Inspection found no
  production branching on personal paths or fixture names. Test expectations
  now accompany fixture data; shared verification no longer recognizes sample
  filenames or skips a named large file. It streams and validates every
  extraction and checks every retained original. No application recipe system,
  fixture detection, transcript correction or test-mode bypass was added.
- The fixture-data changes subsequently passed the fresh E2E run below.
  All above Compose runs are terminal and their isolated containers/volumes
  were removed. No production deployment or personal recapture occurred.

### Fresh corpus and combined recovery verification (2026-09-05)

- Corpus run `cddfcc1dcc56444a93a533f75089691e`, project
  `pufferfs-corpus-djuvgm`, completed every phase with exit 0. Capture accepted
  1,024 synthetic files across 100 directories in 12.04 seconds while consumers
  were stopped; this is a local-emulator observation, not an AWS benchmark.
  Main verification passed in 270.51 seconds, including all ten real Gemini
  batches, streaming every extraction, checking every original, Nomic search
  and the four published-content ACL target forms. Multipart recovery passed
  in 38.38 seconds, normal-timeout completed-work replay in 302.73 seconds,
  restart recovery in 55.08 seconds, and all malformed-message phases passed.
  API/provider cleanup passed in 9.48 seconds.
- Combined run `a45488945d704d0a9c309a9d852477b8`, project
  `pufferfs-combined-vofrxg`, completed crash/replay, stale publication, root
  deletion and scheduled physical cleanup in one invocation with exit 0.
  Lost-response recovery passed in 305.06 seconds with unchanged vector/mutation
  objects and identical decompressed request payloads. Stale writes physically
  reached Turbopuffer after newer publication and after root deletion, without
  leaking through public APIs. Scheduled late-row cleanup passed in 28.34
  seconds; final external cleanup passed in 1.23 seconds.
- Application images remained frozen throughout both runs; all 37 production
  Python module hashes were checked against the worktree before reuse. Current
  black-box drivers were mounted read-only and were not edited during execution.
  No application fault hooks or fixture recognition were added. The newer
  Dockerfile cache-layer arrangement still needs a complete fresh build; image
  reuse does not verify that build change, Modal GPU execution or AWS IAM.
- Both projects have no remaining containers or volumes. Sanitized corpus
  evidence is archived at `/private/tmp/pufferfs-corpus-run.Djuvgm/evidence.tar.gz`;
  combined recovery evidence is at
  `/private/tmp/pufferfs-combined-recovery.VOFrxG/evidence.tar.gz`.
  CI configuration now runs both suites and validates both Compose topologies;
  shell syntax, Compose configuration and whitespace checks pass. Provider
  fault/format coverage, query endpoint authentication, historical recapture,
  authorized deployment/cutover and final legacy retirement remain unfinished.

## Implemented foundations (not cut over)

- Embedded authoritative migrations replace the duplicated inline schema.
- Migration 023 defines file/version/extraction records, source ownership,
  SQS delivery progress, provider request mappings, and S3 embedding locators.
- Transactional file registration advances captured heads independently of
  indexed heads; retries preserve identity and cannot regress captured state.
- Create-only presigned source pack uploads and completion validation.
- Version registration validates source extent ownership/bounds, persists
  manifests, records deliveries and attempts bounded SQS publication.
- Recovery publication retains unmarked ledger entries on SQS/database errors.
- Two-queue SQS constructor and per-file index grouping (legacy queue support
  remains temporarily until the old consumers are retired).
- Lazy multi-extent source reads with EOF hash verification.
- Verified append capture reuses prior extents only after hashing the prefix.
- Bounded Go file consumers maintain SQS visibility and verify durable database
  completion before acknowledgment. Modal attempt ownership is checked in SQL.
- Separate Modal transformation application reads source ranges directly and
  writes verified, deterministic compressed chunk artifacts before index handoff.
- Streaming text/generic JSONL preserves original bytes and source locations.
- Native CSV/TSV and XLSX/XLSM/template parsing produces bounded chunks with
  first-row context, cell addresses and sheet/row locations. XLSX formulas are
  indexed as expressions, not executed. Legacy spreadsheet decoders remain.
- Local visual preparation module renders PDF pages, normalizes raster frames,
  and invokes isolated-profile LibreOffice conversion. Generated images are
  iterator-scoped temporary files, never S3 objects. It is connected to Gemini
  submission; standalone SVG now shares bounded local rendering (details below).
  Real Office conversion fixtures now cover seven formats (details below).
- Media preparation decodes once through FFmpeg into iterator-scoped mono
  16 kHz WAV clips (maximum five minutes), with sample-derived source offsets.
  Native audio and video-with-audio fixtures verify exact coverage and cleanup.
- Gemini Batch request contracts use the specified model and provider file
  references. Transcript parsing validates segment bounds, scopes speaker labels
  per request and adjusts source timestamps; incomplete provider output fails.
  These contracts are connected to durable submission/collection.
- Provider submission ledger now reserves stable request/batch identities,
  records a pre-call submission marker and resolves lost responses by provider
  listing rather than blindly repeating paid creation. A preparation seal
  requires the complete contiguous set of submitted requests before handoff.
  Tested with real local Postgres and mocked provider calls. Remaining integration:
  temporary upload expiry/cleanup and stalled-work recovery. Transform and
  collector entrypoints disable implicit SDK retries for paid submissions.
- Collector implementation packs successful request text into S3, records
  per-request outcomes and assembles only sealed, fully successful extractions.
  Assembly commits completion and the index delivery ledger together. Separate
  scheduled Modal collector role added (not deployed). Local Postgres + fake
  provider/S3 tests cover replay and missing outputs. Failed-request retry and
  provider upload cleanup remain required.
- Transformation now connects captured visual/media sources to temporary Google
  uploads, bounded batch submission and a sealed waiting-provider handoff.
  A local Postgres integration test runs real PNG normalization from a verified
  captured source through mocked Google submission/collection to the pending
  index ledger, verifies no generated S3 images and no duplicate submit on retry.
- Added Nomic-compatible S3 float32 vector packs with tenant/model-scoped PG
  locators, batch-local text deduplication and grouped range reads. Added pure
  version-isolated row construction and byte/row-bounded mutation construction.
  Tested using local Postgres, fake S3 and a fake encoder. These pieces are not
  yet connected to the new index deployment; mutation persistence/publication,
  real GPU validation and current-version read filtering remain required.
- Mutation preparation now streams extracted chunks through bounded embedding
  batches (or CPU/no-vector mode), preserves namespace routing, and persists
  bounded write payloads in one replayable S3 artifact. Its reference and batch
  count are committed with attempt ownership before any external index writes.
  Tests cover 600-row CPU preparation, retry reuse and mismatched chunk counts.
- Mutation publication now replays version-isolated upserts, acknowledges each
  external write under attempt ownership, and advances indexed version/extraction
  pointers only after complete application and a transactional current-head check.
  Tests interleave a delayed old write after newer publication and verify old
  rows do not overwrite newer IDs or regress the catalog; lost responses replay
  identical upserts. This does NOT yet prove end-to-end stale-result safety:
  production read/search must enforce the indexed-extraction filter. Physical
  deletion/GC and actual Turbopuffer/Modal index integration are still required.
- Index worker now connects claim, preparation, Turbopuffer SDK writes and
  publication, with heartbeat and failure ownership handling. Separate CPU and
  bulk Nomic GPU Modal applications use a lean index image; the query pool is
  unchanged. Deployment definitions import successfully against installed SDKs,
  and a local Postgres/fake-Turbopuffer test verifies lost-response replay through
  the full worker entrypoint without regenerating mutations. Actual image build,
  GPU inference, cloud writes and deployment/cutover are not yet verified.
- Read/search now use a catalog snapshot to filter published extraction IDs
  before index ranking, preserving legacy visibility until first per-file
  publication and excluding captured deletions. Existing ACL/content-proof
  postfilters are retained. Local database tests verify snapshot construction.
  Root-sized ID/path filters still require scalability work, and endpoint-level
  stale-row, multi-chunk page/line, ACL and proof tests remain required.
- Single-file page/line reads now paginate in chunk order under one visibility
  snapshot instead of limiting rows to requested page count or 1000 chunks.
  Page parts are reassembled and new-format split-line fragments concatenate;
  legacy overlapping line chunks keep their prior behavior. Reads above 32 MiB
  fail explicitly. Pagination tests cover >512 chunks, nonadvancing cursors and
  oversized responses. End-to-end page/line + access-control tests remain.
- Removed generated-image asset endpoint, page/query response image fields,
  CLI image download/output-directory support and associated documentation.
  Existing S3 objects are untouched. Legacy transformation image persistence
  remains until the old pipeline is retired; new workers persist no images.
- Structured EML/MSG/VCF/ICS field parsing is connected to transformation,
  without provider inference. EML tests cover decoded headers, preferred plain
  body and attachment exclusion; VCF/ICS tests cover folding, escaped text,
  mixed events/tasks and oversized-field chunk splitting. Real MSG fixtures
  remain unverified. MIME parsing has an explicit 32 MiB input limit; logical
  VCF/ICS fields are bounded at 1 MiB rather than silently truncating.

## Original implementation checklist (historical)

This was the foundation-stage checklist. Subsequent entries record implemented
work; use the current validation gates above for what remains unverified.

1. Wire the local agent to source-pack/version APIs, retain source manifests
   locally, use verified append capture, and stop waiting for indexing in watch.
   Preserve capture reconciliation, ignore policies, moves, and multipart behavior.
2. Finish deployment wiring for the implemented authenticated file consumers;
   verify live visibility and DLQ behavior before cutover.
3. Implement direct-Postgres Modal transformation and collector deployments.
   CPU conversion/rendering is local, not nested remote calls. Gemini 3.5 Flash
   Lite Batch handles every document page/image and structured diarized media
   transcript. No native PDF fast path, provider roulette or persisted images.
4. Implement streaming text/generic JSONL, native spreadsheet row-range parsing,
   expanded format decoders and preserved structured-file support; test fixtures.
5. Implement durable chunk/vector/mutation S3 artifacts and one indexing path.
   Preserve Nomic and vector-disabled CPU mode; remove Postgres vector bodies.
6. Prove stale publication safety across lost worker ownership and in-flight
   external writes. A lease/pre-write version check alone is insufficient.
7. Implement reconciliation for missing deliveries, stalled workers, ambiguous
   provider submissions, incomplete publication and provider-input cleanup.
8. Update search/read to per-file publication with exact source locations,
   ACL/content-proof preservation, split-page reads and captured/indexed status.
   Remove generated-image API/CLI features. Update frontend status types/UI.
9. Define and implement retained-source/pack GC and root deletion against late
   workers/provider results. Sources must outlive processing; no successful-sync
   deletion of referenced source/output artifacts.
10. Update ECS/Modal deployment roles, secrets, SQS infrastructure, CI and docs.
    Drain/retire commit queue/worker, root-generation barrier, in-server
    processing, source base64, remote conversion wrappers and duplicate indexer.
11. Provide and verify migration/backfill tools. Existing originals may already
    have been deleted from staging and require local-agent recapture. Production
    rollout and data mutations require their normal explicit shipping authority.

## Original foundation verification (retired tests)

- `go test ./...` passes for the foundation changes.
- Local Postgres tests use ONLY `PUFFERFS_TEST_DATABASE_URL`, isolated schemas,
  and embedded migrations; no repository credentials or live data.
- Tests cover concurrent/stale capture, idempotency, rollback, deletion gating,
  tenant isolation, immutable source references and failed SQS handoff recovery.
- Source tests cover lazy range reconstruction, digest mismatch, truncation,
  extent bounds, verified append, rewrites and fixed-extent capture.
- Worker tests cover bounded text and spreadsheet chunks, multiline CSV,
  sparse XLSX cells, formulas, hidden sheets, artifact round trips and durable
  completion despite a failed enqueue. Database tests cover expired ownership.
- Full worker/provider, API compatibility, deployment and cutover verification
  remains required. Foundation tests do not prove the full objective complete.

### Native legacy spreadsheet decoder progress

- XLS/XLT, XLSB and ODS/OTS now use Calamine in the transformation worker,
  feeding the same row-to-chunk implementation as XLSX and CSV. No LLM,
  conversion service, or new worker interface is involved.
- Preserve sparse cell coordinates: Calamine's row iterator retains leading
  empty rows but strips leading empty columns; restore the column offset.
- Calamine provides cached cell values rather than formula expressions; it
  does not execute formulas/macros. The native decoder materializes a sheet;
  row iteration avoids an additional whole-sheet Python copy, not all memory
  growth. XLSX remains on openpyxl's read-only path.
- Seven spreadsheet tests pass, including generated real XLS/XLT and ODS/OTS
  fixtures. XLSB is routed but still needs a real binary fixture. Test fixture
  writers are CI-only dependencies.
- Worker discovery: 84 tests, 29 passed, 55 skipped without the isolated
  Postgres test connection. No production services were called.

- FODS now streams XML rows through the same chunker. It preserves repeated
  row/column coordinates, explicit text whitespace, covered-cell positions,
  and cached values (formula text when no cached value exists). Empty repeated
  rows advance coordinates without expansion. Processed XML rows are removed
  from the tree. DTD/entities are forbidden; expansion is limited to 1,048,576
  rows, 16,384 columns and 8 MiB of decoded cell text per row. These checks do
  not impose a pre-parse bound on an individual XML row's source size.
- Four FODS tests cover cell locations, whitespace, sparse/repeated rows,
  chunk byte limits, invalid repetition and XML entity rejection. Worker suite
  after this addition: 88 tests, 33 passed, 55 database-dependent tests skipped.

### Continuous database contract verification

- CI now provisions disposable Postgres 17 and supplies only
  `PUFFERFS_TEST_DATABASE_URL` to the existing unique-schema fixtures. Python
  installs psycopg; both Go and Python run the database contracts. Any skipped
  Python test fails the CI step rather than silently reducing coverage.
- Local verification against a newly created disposable Postgres 17 instance:
  `go test ./...` passed; all 88 Python worker tests passed with zero skips.
  This exercises real catalog/migration/lease/publication transactions with
  fake S3, SQS, Gemini and Turbopuffer boundaries, not live provider delivery.
- The workflow change has not been pushed or run on GitHub Actions yet.

### Independent handoff reconciliation role

- `modal/reconciliation_app.py` defines the separate `pufferfs-reconciliation`
  scheduled deployment: once per minute, 1 CPU, 512 MiB, at most one container,
  180-second timeout. Its image contains database/SQS/S3 and index-maintenance
  clients, with no renderer, LLM client or embedding dependencies.
- It fences at most 100 expired running attempts per invocation, retaining
  artifact references, acknowledged mutation counts and delivery timestamps.
  It then publishes committed-but-unconfirmed handoffs through SQS. It does
  not launch transformation/indexing or claim execution from Postgres.
- Known-delivered work is not resent just because its timestamp is old: the
  receipt may still be invisible, queued or in the DLQ. SQS redelivery and
  dead-letter policy remain authoritative. A subsequent consumer delivery
  can claim an expired attempt's work with a fresh token.
- Real Postgres tests cover expiry fencing, late completion rejection,
  retained progress, failed-send replay, missing index delivery, active lease
  preservation and no automatic resend of failed work. Modal application
  definition imports locally; deployment/schedule execution remain unverified.
- This role does not yet resolve ambiguous Gemini submissions, retry failed
  provider pages, clean provider uploads, or garbage-collect old source/index
  artifacts. Those remain required recovery/cleanup work before cutover.

### Local-agent capture transport foundation

- Shared `pkg/models/source.go` request/response types now define pack init,
  completion and version acceptance for both API handlers and the Go client.
- `cmd/pufferfs/capture_client.go` provides context-aware metadata calls and
  direct immutable PUT of a caller-owned captured byte reader. It never reads
  the live source path, forwards the API bearer token to object storage, or
  polls indexing. Signed upload URLs are omitted from transport errors, and
  PUT redirects are refused. HTTP 412 allows proceeding to the separate
  completion check, not treating object existence as capture acceptance.
- Local HTTP tests verify the init -> direct PUT -> completion -> acceptance
  boundary, conditional headers, auth separation, redirect refusal, upload
  errors and mismatched acceptance rejection. These are client contracts,
  not proof of a switched-over CLI or live S3 upload.
- CLI/watch still use the old capture orchestrator. Next integration work:
  durable capture journal/spool, resuming or renewing pack authorizations,
  captured-head state, large-source upload, then switch sync/watch to these
  calls and remove generation-bound capture orchestration after validation.

### Restart-safe local capture submission

- `cmd/pufferfs/capture_journal.go` persists per-capture pack mappings, the
  original capture request and acceptance using write/fsync/rename/directory
  fsync. A kernel file lock serializes submission and is released on process
  exit. Pack reads stay within the journal directory via `os.OpenRoot`.
- Submission verifies each pending local pack's size and SHA-256, saves its
  remote object identity before PUT, checks completion after ambiguous replies,
  and retains exact capture ID/request across registration retries. Accepted
  journals return without network calls; source files are not deleted here.
- The pack-init endpoint can renew an existing upload authorization, checking
  organization/root ownership and unchanged declared size. Signed URLs and
  headers remain in memory, not journal state.
- HTTP restart tests cover upload failure/renewal, lost registration response,
  identical retries, credential exclusion, immutable-input resolution, corrupt
  packs and concurrent submission. Capture tests pass under `go test -race`.
- Still required: create/fsync the immutable capture spools and journals from
  filesystem discovery, maintain captured-head state, wire CLI/watch to journal
  submission, and integrate large-source multipart upload. The journal submitter
  is implemented but is not yet the active CLI/watch ingestion path.

### Immutable local spool creation

- `createCaptureSpool` now takes up to 128 selected root-relative paths and
  captures their observed byte extents into disk packs using a 64 KiB copy
  buffer. Files share packs and can span pack boundaries; pack limits are
  explicit (up to 128 MiB). This does not replace the still-required multipart
  integration for the final large-source upload path.
- Each source receives an exact SHA-256 manifest. Packs are hashed, made
  read-only and fsynced before the journal is atomically published. The capture
  directory's parent is also fsynced. Empty files need no object; tombstones
  need no source read. Captured metadata and detected dirty paths are retained.
- Source reads are confined by `os.OpenRoot`. Nonblocking open followed by a
  regular-file check avoids hanging on a file replaced by a FIFO. Partial
  capture failures leave caller-owned spool directories; directories without
  `journal.json` are not submit-ready. Orphan-spool cleanup remains required.
- Tests reconstruct captures after live source rewrites, verify shared and
  split pack boundaries/hashes/modes, and reject escaping symlinks, FIFOs,
  missing paths and cancellation without publishing an incomplete journal.
  The restart test now runs real spool creation through the HTTP submitter,
  including upload/registration failures and source mutation after capture.
- Discovery/ignore selection, accepted-head storage, verified append reuse,
  root-level coordination and switching CLI/watch remain integration work.

### Local captured-head installation

- `capture_heads.go` stores one atomic JSON record per server/root/path hash,
  containing accepted version identity, resolved source manifest, observed file
  metadata, dirty flag and deletion state. Updating a capture touches only its
  changed paths, not a serialized root-wide map. Tombstones retain the previous
  version needed to recreate the path safely.
- Installation holds a kernel lock and compares server version sequences.
  Older/equal retries do not overwrite newer local state; conflicting identities
  at the same sequence are rejected. Per-file writes share the fsync/rename
  helper with the capture journal. The caller supplies an existing root-cache
  parent directory.
- `submitCapture` now performs restart-safe upload/registration followed by
  captured-head installation. A local disk failure after acceptance leaves the
  accepted journal available for replay without another network operation.
- Tests cover source/metadata retention, dirty state, stale replay rejection,
  tombstones, server/root isolation, invalid acceptance and local write recovery.
  The spool-to-HTTP restart test now verifies installed captured heads as well.
  Capture/head tests pass under Go's race detector; the full Go suite passes
  without database integration enabled on this iteration.
- CLI/watch discovery, head enumeration/bootstrap from the remote catalog,
  verified append integration and root-level capture coordination still need
  wiring before the old ingestion path can be retired.

### Remote captured-catalog bootstrap contract

- Added `GET /roots/{id}/captured-files` with keyset pagination and migration
  027's `(root_id,id)` index. It returns captured/indexed identities, content
  metadata, source-manifest locators and tombstones from Postgres, not S3 bodies.
- Reuses root sync authorization and path ACL evaluation; loads applicable ACLs
  once per page and fails closed on lookup errors. ACL lookup now propagates
  row-iteration errors rather than treating a partial read as successful.
- The Go client walks pages incrementally via a callback and checks cursor
  progress. An ACL-filtered empty page does not prematurely terminate the scan.
- Real Postgres/API tests cover pagination, captured-versus-indexed state,
  tombstones, denied paths, invalid limits, cross-org and anonymous requests.
  This is a live scan, not a snapshot: per-file registration CAS still detects
  concurrent changes. CLI bootstrap/discovery wiring remains required.

### Opt-in sync/watch orchestration

- `PUFFERFS_FILE_CAPTURE=1` now connects normal sync, subset sync and the
  existing watch caller to `runFileCaptureSync`. The default remains legacy
  until the remaining cutover gates are implemented and verified.
- The new orchestration locks its local root cache, resumes pending journals,
  scans the paginated captured catalog, reuses matching accepted-head metadata,
  applies existing ignore/subset discovery, captures at most 128 paths per
  batch, uploads/registers them and installs local heads. It returns `captured`
  without index polling, including when the legacy wait parameter is true.
- Accepted spools move from pending to completed storage after head installation;
  their bytes remain retained locally and in S3. The agent's own cache subtree
  is excluded from discovery even when it lives within the watched root.
- A 130-file HTTP integration test verifies two packed registration batches,
  unchanged-file reuse, selected update isolation, subsequent deletion, retained
  completed spools, cache self-exclusion and no dependence on indexed versions.
  It passes with the race detector. The full Go suite passes (database tests
  not enabled in this iteration).
- This is still migration-only: proof updates, new status/wait semantics,
  concurrent-writer conflict repair, verified append reuse, durable multipart
  upload and spool/source retention policies remain required. Do not advertise
  the flag as a completed production pipeline or retire the old path yet.

### Verified append integration

- The opt-in sync path now passes matching accepted source manifests to the
  spool builder, retaining at most the current 128-file batch's prior extents.
  `HashVerifiedPrefix` verifies the old byte prefix and returns the continuing
  SHA-256 state; the existing pack writer captures only the suffix, including
  suffixes that cross pack boundaries. No format-specific adapter is involved.
- Reuse requires matching server/root/path and previous version ID. Rewrites
  and truncations rewind into ordinary replacement capture. Equal content can
  reuse the complete manifest without creating a new source pack. Prefix
  verification still rereads local bytes; this reduces transfer, not all reads.
- Spool tests verify exact reconstructed hashes/content for append, unchanged,
  rewrite, truncation and mismatched-version cases. The HTTP sync test verifies
  that appending five bytes uploads exactly five bytes and retains both prior
  and new source extents in the accepted head. Race tests and Go suite pass;
  database integration was not enabled for this local-only change.
- This is source reuse, not incremental text extraction or index mutation
  optimization. Proof/status compatibility, concurrent-writer conflict repair,
  multipart upload and retention cleanup remain required before cutover.

### Per-file proofs and indexed metadata

- Migration 028 stores user/root/path content proofs per file. Registration
  records the validated version's hash before returning capture acceptance.
  Sequence-guarded upserts prevent old retries from overwriting newer proofs;
  deletion tombstones prevent fallback to obsolete root-wide proofs.
- Read/search proof filtering loads only requested paths. Existing root-wide
  proofs remain a fallback for paths with no per-file proof. This preserves the
  existing client-reported hash check, not cryptographic proof of possession;
  root authorization and path ACLs still apply separately.
- Catalog registration and proof recording are separate transactions. A proof
  failure returns 500; retrying the same capture repairs the proof. A real-PG
  API test injects that failure after catalog commit and verifies one version
  remains after repair. Tests also cover user/org isolation, hash/path mismatch,
  legacy fallback, deletion and stale retries. The full Go suite passes with
  the test database enabled.
- New index mutations now include `file_type`, preserving public language and
  Office labels and labeling expanded spreadsheet/image/media formats through
  the existing format table. This pure metadata mapping does not choose an
  extractor or provider. Both index images include the shared module. All 102
  Python tests pass with Postgres, including persisted mutation metadata tests.
  Previously persisted mutation artifacts are reused unchanged; this change
  does not backfill already-indexed rows.
- Proof compatibility is not finished: unchanged-file bootstrap for a new user,
  local cache identity across user switches, and mixed legacy/new updates still
  need explicit handling before cutover. No production deployment occurred.

### Explicit infrastructure mode

- Pulumi now has an explicit `processingMode=file` topology: separate ECS
  transform/index consumers and FIFO queues/DLQs, no commit consumer, and the
  transform/GPU-index/CPU-index endpoint settings. Query embedding remains
  separate. New file index queues do not reuse the legacy message contract.
- One plain topology value determines workers, queue naming and endpoint
  requirements. File mode requires SQS and validates concurrency against the
  consumer's 1..64 limit. The deployment configuration script and workflow pass
  the corresponding settings; legacy remains the default until cutover gates
  pass. CI now runs infrastructure tests rather than only compiling it.
- TypeScript compilation and six local tests pass, including execution of the
  real configuration script with synthetic inputs and a fake Pulumi executable.
  Invalid/missing file configuration fails before any config writes.
- This is not a deployed or production-ready cutover. Applying file mode to an
  existing stack removes legacy queue resources and chunk/commit services.
  Drain/account for old work and DLQs and review a real preview first. Modal
  deployments, external secrets, IAM/connectivity validation, source recapture,
  remaining processing/recovery gates and final legacy-code retirement remain.

### Failed-page provider retries

- Migration 029 records request attempt counts and a unique retry-parent batch
  link. The collector reserves a new batch containing only failed requests from
  a terminal batch, after the transform worker has sealed the complete input
  count. Successful request rows and S3 result locators stay unchanged.
- Retry reservation is transactional and serialized against root changes and
  concurrent collectors. A crash before submission leaves a discoverable retry
  batch; a lost create response uses the existing ambiguity reconciliation, not
  another paid create. Initial transform delivery still belongs to SQS.
- Each request receives at most three provider attempts. Exhaustion explicitly
  fails the extraction/transform ledger while retaining successful outputs.
  Deleted or superseded versions do not reserve retries; submission rechecks
  currentness before the paid create marker. The root lock is not held during
  provider network IO, so a concurrent capture can still occur after that check;
  per-file index publication fencing remains necessary.
- Real-Postgres tests cover concurrent reservation, only-failed-page submission,
  preserved successful artifacts, assembled page order, lost-response recovery,
  exhaustion, and deletion between reservation and submission. The full Go suite
  and 111 Python tests pass with the isolated test database enabled.
- Remaining provider gates include expired Google input refresh, orphan upload
  cleanup, operator recovery after retry exhaustion, and ambiguous markers whose
  provider create never occurred. No live Gemini calls or deployment were made.

### Legacy mutation boundary in file deployments

- A server configured with the transform queue now rejects legacy upload,
  multipart upload, upload-bundle, sync/init, generation heartbeat/artifact
  upload and generation abort mutations before reading bodies or touching
  processing/storage state. Authenticated callers receive HTTP 409 with
  `capture_protocol_required`; anonymous callers still receive HTTP 401.
- This prevents old clients from starting the root-generation pipeline against
  file-only consumers. A single route guard makes the protocol boundary
  explicit. Legacy deployments continue invoking the original handlers.
- Configuring the transform queue selects SQS even if the backend setting is
  absent, and explicitly selecting NATS fails before connecting. Queue factory
  tests cover both optional API and required worker initialization paths.
- Router tests exercise all eleven mutation routes with nil dependencies to
  detect accidental handler execution. The Go suite passes; database integration
  is not enabled for this routing-only change. Read/search routes are unchanged.
- This does not complete client upgrade negotiation or status compatibility,
  and does not remove legacy code. Cutover still requires those gates and source
  recapture validation. No deployed configuration changed.

### Immutable multipart storage foundation

- Added distinct immutable multipart create/complete methods, leaving the
  legacy overwrite-capable method unchanged until retirement. Creation stores
  a caller-generated durable upload identity in object metadata; completion
  sends `If-None-Match: *`. HEAD verifies both identity and size, including after
  an ambiguous completion response. Mismatched objects are never deleted.
- SDK-against-local-HTTP tests check create metadata, conditional completion,
  lost-response replay, wrong size, same-size foreign identity, and invalid
  part lists rejected before IO. These are not live S3 tests.
- The source-pack API and local journal are not wired to these methods yet.
  Multipart upload-ID persistence, signed-part renewal, part acknowledgements,
  completion recovery and abandoned-upload cleanup remain required. The current
  opt-in capture path still uploads bounded packs using immutable single PUTs.

### Resumable source-pack multipart API

- Migration 030 stores multipart upload IDs, fixed part size and the sealed
  completion parts list beside source-object metadata. The new init/part/complete
  endpoints reuse root sync permissions and validate org/root ownership.
- Init uses a client-persisted UUID to derive one stable object key. Concurrent
  initializers publish one winning upload ID; a known losing session is aborted.
  No database transaction spans S3 creation. A lost creation response can still
  leave an unknown orphan, requiring lifecycle cleanup before production.
- Part authorization signs exactly the expected byte count, including the final
  short part. Completion freezes ordered ETags before S3, rejects different retry
  payloads, and marks the source complete only after identity/size validation.
  Completed retries do not call S3 again. Multipart objects cannot obtain single
  PUT URLs or bypass validation through single-PUT completion.
- The API accepts 1..128 MiB packs with 16 MiB parts; larger files span packs.
  Real-PG tests cover same-request recovery, concurrent initialization, size and
  ETag conflicts, final-part limits, foreign orgs and completion retry. Go suite
  and targeted race tests pass. No live S3 calls or deployment occurred.
- Local multipart journal wiring, upload expiry/abort policy and lifecycle cleanup
  remain required. This completes the API boundary, not the end-to-end feature.

### Multipart capture-journal integration

- The opt-in capture CLI now uses multipart for new packs of at least 32 MiB;
  smaller packs retain single PUTs. Existing single-PUT journals do not change
  protocol mid-upload. File contents can still span multiple bounded packs.
- Before initialization the journal fsyncs a stable request UUID. It retains
  the server upload ID, fixed part size and sequential ETags, persisting each
  acknowledgement before advancing. Resume validates the same session and
  uploads only unacknowledged parts from the already-hashed immutable spool.
  Part transfers are sequential and stream from disk rather than buffering a
  whole pack. Each needed part obtains a fresh signed URL.
- Completion uses the exact persisted parts list; a lost completion reply can
  recover through init's completed flag. Version registration follows durable
  source completion. URLs and signed headers never enter the journal, redirects
  are rejected and API authorization is not sent with part PUTs.
- A 32 MiB HTTP integration test injects init, second-part and completion reply
  failures. Part 1 uploads once, part 2 twice, completion once and registration
  once. Rewriting the original file does not alter sent bytes. Targeted race
  tests and the Go suite pass (no database needed for this local-only change).
- Live-S3 validation, abandoned multipart cleanup/expiry recovery and spool
  retention remain cutover gates. No production service or data was changed.

### Proof-only bootstrap API

- Added `POST /roots/{id}/captured-proofs` for bounded batches of existing
  path/version/hash tuples. It validates all tuples under the root capture lock
  before persisting any per-user proof; stale/deleted/mismatched members reject
  the whole batch. Root sync permission and path ACLs remain enforced.
- This endpoint does not call S3, a provider, or SQS and does not create file
  versions/extractions/work. The existing client-reported hash semantics remain;
  this is not cryptographic proof of possession. Captured catalog pages now
  include a per-requesting-user `proof_current` flag via the proof primary key.
- Real-PG tests cover empty initial proof state, partial-batch rejection,
  mismatching hashes, idempotent bootstrap, proof visibility, unchanged version/
  extraction/work counts, deletion and org isolation. Go tests pass with the
  isolated database. No deployed service or production data was changed.
- CLI local hashing, cache/user identity handling and automatic proof-only
  bootstrap still need integration. The API alone does not close that gate.

### CLI proof-only bootstrap

- The opt-in sync path now verifies candidate files against captured hashes
  before deciding to create new versions. Matching files bootstrap metadata-only
  local heads and, when needed, register proofs in batches of at most 128.
  Changed files proceed through capture; forced sync deliberately bypasses this
  optimization. No source manifests are invented for bootstrapped heads.
- Local cached metadata is reusable only when the catalog reports the current
  user's proof as current. A user switch with missing proof forces actual byte
  verification, even with a shared local cache. Hashing uses bounded 64 KiB reads
  through a confined root, checks file identity/size/mtime afterward, and rejects
  symlink escapes. Existing matching heads keep their append extents.
- Proof acknowledgement precedes local cache installation. A proof failure
  stops the sync instead of silently omitting proof registration. A later retry
  can rehash safely; this operation does not create versions or extraction work.
- The 130-file CLI HTTP test verifies two proof batches, no uploads/new versions,
  no repeated proof requests on the next sync, and fresh bootstrap for another
  user. Tests also reject same-size/same-mtime altered content and symlink escape.
  Targeted race tests and the Go suite pass; no live services were used.
- Mixed legacy/new updates, full authorization end-to-end validation and broader
  cutover gates still remain. The production default is unchanged.

### Durable physical index deletion

- New tombstone work now persists a delete mutation in S3 instead of an empty
  artifact. Its filter is restricted to the root/path and either the same file
  identity at or below the tombstone sequence, or legacy rows with no file ID.
  It cannot remove newer per-file incarnations. Source objects are not deleted.
- Publication accepts only the exact expected tombstone filter and verifies the
  active root namespace before any mutation. Arbitrary delete filters and foreign
  namespace references fail before external writes. Existing catalog/attempt
  fences still determine whether the tombstone may publish its indexed pointer.
- The index worker repeats a partial delete until `rows_remaining` is false,
  capped at 100 writes per attempt. The artifact is acknowledged only afterward.
  Lost replies and bounded-attempt failures replay the same durable filter via
  SQS without re-extraction or embedding. This follows Turbopuffer's documented
  `delete_by_filter_allow_partial` / `rows_remaining` write contract.
- Real-PG tests with simulated index responses cover persisted-before-write,
  lost-reply replay, partial deletion and a delete in flight while a newer version
  publishes. The 119-test Python suite passed, followed by all nine focused
  deletion tests including malformed-delete/foreign-namespace rejection.
- This is not complete deletion/GC coverage: late stale upserts can still leave
  hidden physical rows after a delete, old update versions need cleanup, retired
  namespace cleanup and root-deletion races need validation, and pre-existing
  empty delete artifacts need repair. No live Turbopuffer calls or deployment
  occurred. Mixed legacy/new mutation traffic remains unsupported at cutover.

### Bidirectional deployment protocol guard

- Closed the reverse cutover gap: legacy deployments now reject all per-file
  capture endpoints with `409 capture_processing_unavailable` before database,
  storage, or queue access. Previously they could accept captures despite having
  no transform consumers. Authentication and scope checks precede this guard.
- The existing per-file deployment guard still rejects legacy mutations. There
  is no automatic protocol fallback; clients must resume their durable journal
  against the matching deployment after cutover.
- Router tests exercise all eight capture endpoints with anonymous, read-only,
  sync, and write callers using nil effect dependencies. Capture API integration
  fixtures now explicitly enable file processing. The full Go suite passes with
  a disposable Postgres database, including upload, registration, catalog, and
  proof integration tests. No production credentials or services were used.
- This closes a protocol-validation gate, not the overall cutover: stale index
  cleanup, retention, client status compatibility, historical recapture, live
  validation, and legacy retirement remain unfinished.

### Recurring stale-index cleanup

- Reconciliation scans at most 25 due, already-published files per invocation.
  For a live published version, cleanup matches strictly smaller version
  sequences; for a published tombstone it also matches the tombstone sequence.
  It never uses the newer captured head as its cutoff. Root/path/file identity
  restrict each filter; legacy rows without file IDs are scoped by root/path.
- The cutoff is monotonic under normal per-file publication: a delayed delete
  cannot match the visible version or a later recreation. Cleanup checkpoints
  cannot postpone a newer version's cleanup. Publication clears the previous
  cleanup locator and makes the file immediately eligible for another sweep.
- Cleanup records are packed together per organization/root in S3 before any
  index write. Postgres stores only pack reference, record ordinal, and next
  check time. Retries and later sweeps reuse the artifact; each pack is fetched
  at most once per root group. Replay verifies the exact cutoff and namespace,
  rejecting foreign or unbounded mutations before application.
- Successful sweeps are eligible again after one day, so late stale upserts are
  eventually removed. Partial deletes and failures are eligible after five
  minutes. These are minimum delays, not cleanup-latency guarantees: capacity
  is bounded at 25 files per scheduled invocation. A 90-second soft work budget
  stops starting additional writes; the index client uses a 20-second timeout
  without internal retries. The scheduled deployment's hard timeout still applies.
  Partial-delete behavior follows https://turbopuffer.com/docs/write.
- This is index-row cleanup only. Source packs/manifests, extraction outputs,
  embeddings, and replay artifacts remain retained; no successful sync or
  individual file deletion removes those bytes in the per-file pipeline. This
  preserves append reuse and replay and avoids deleting a shared source pack.
  Whole-root deletion is a separate operation requiring further race testing.
- Real-Postgres tests cover visible versus captured cutoff, delayed delete
  versus recreation, recurring late-write cleanup, interrupted uploads, lost
  index replies, partial results, malformed filters, bounded packing/scanning,
  publication during upload, and no cleanup for unpublished/deleting roots.
  The full Go suite and all 134 Python tests passed with simulated cloud IO.
  Updated reconciliation and CPU/GPU index deployment definitions also import
  successfully using the installed Modal SDK; this is not a deployment test.
- Remaining cleanup gaps: obsolete extractions of the same version, abandoned
  unindexed versions, retired/remapped namespaces, whole-root deletion races,
  and source/local-spool retention enforcement. A changed namespace is rejected
  on replay rather than silently retargeted. Mixed legacy/new writers remain
  unsupported. No live index writes or deployment have been performed.

### Same-version extraction publication order

- Reproduced two failures before fixing them: an old revision's in-flight write
  could finish after a newer revision and replace the indexed catalog pointer;
  a stale revision redelivery could also claim indexing after the newer result
  had published. File-version checks alone did not distinguish these jobs.
- Migration 032 adds immutable extraction registration sequences. Go's existing
  root-locked registration inserts each new revision once; idempotent retries
  retain its order, independently of revision naming or completion timestamps.
  Workers reject already-obsolete revisions at claim time and recheck under
  the final root/file publication lock. A pending newer revision does not block
  the older one from publishing first, but the catalog cannot regress afterward.
- New index rows carry extraction order. Replay validates version and extraction
  order attributes against the claimed job. Old artifacts without extraction
  order remain replayable; they are not silently assigned an invented order.
- Recurring cleanup now removes lower-order extractions of the same published
  version, explicitly excluding the published extraction ID. Future revisions
  and rows with missing order attributes survive. Turbopuffer's `Lt`/`Lte`
  operators match null values, so presence checks are essential; filter test
  doubles now reproduce that behavior rather than incorrectly treating null
  comparisons as false. See https://turbopuffer.com/docs/query#filtering.
- Cleanup locator persistence and acknowledgement compare both indexed version
  and indexed extraction. A same-version publication during upload or deletion
  cannot install an obsolete cutoff or postpone the new revision's cleanup.
- The migration preserves published historical results over pre-migration
  alternatives because their original registration order is unknowable. New
  revisions receive higher order. Old cleanup references are cleared for replay
  under the new contract. This is tested against real populated Postgres tables.
- Validation: all 142 Python worker tests and the full Go suite passed against
  disposable Postgres. The registration-order test also passed with Go's race
  detector. Reconciliation and CPU/GPU index deployment definitions import with
  the installed Modal SDK; no deployed behavior is claimed from that check.
- Historical rows without order, retired/remapped namespaces, abandoned
  unindexed versions, root-delete races, retention, status/read scaling,
  historical recapture, live validation and legacy retirement remain unfinished.
  Old workers must be drained before migration/rollout; no production migration,
  index write, or deployment was performed.

### Per-file status and client waits

- `captured-files?processing=true` now returns the latest registered extraction
  for each current capture, stage/status, attempt count and mutation progress.
  Complete means the exact extraction is published. A new revision of already
  indexed bytes remains pending until that revision publishes. Missing or
  inconsistent publication metadata is reported explicitly, not as completion.
- The option is metadata-only and uses the existing root/path authorization and
  keyset pagination. Ordinary capture scans omit it and avoid the extraction/work
  joins. Migration 033 indexes per-version extraction order for bounded lookup.
  No worker errors, source/chunk bodies, or provider results are returned.
- With `PUFFERFS_FILE_CAPTURE=1`, `sync status`, `sync status --watch`, and
  `sync wait` use this catalog instead of legacy root jobs. Summaries retain
  counts and at most 20 non-complete examples. Polling/catalog requests honor
  cancellation and deadlines; setup/root-resolution and local hashing still use
  existing helpers. Output errors and terminal processing failures stop waits.
- Filtered waits reuse existing glob/ignore semantics and rehash selected local
  files per poll. They require matching captured hash/size and publication, do
  not succeed on missing local captures, and ignore unrelated file failures.
  These live scans are not atomic root snapshots and introduce no server-side
  commit barrier. No status/wait command uploads or enqueues processing work.
- Capture mode rejects `--job-id`; `sync jobs` directs callers to per-file
  status instead of displaying old root jobs. Legacy mode behavior is retained
  until cutover. Dry-run compatibility and full legacy retirement remain open.
- Real-PG tests cover lifecycle states, mutation progress, same-version revision
  changes, ACL/org isolation, and default-query omission. CLI HTTP tests cover
  empty ACL pages, bounded examples, selected byte mismatches/missing files,
  unrelated failures, false completion, polling cancellation, filtered waits,
  and no legacy-state requests. Full Go tests, targeted race tests, and all 142
  Python worker tests passed using disposable Postgres and simulated cloud IO.
  No production migration, data mutation, or deployment occurred.

### Read-only capture previews

- Full and subset capture-mode dry runs now branch before legacy generation
  discovery, root creation, local cache writes, and pending capture submission.
  They resolve existing roots read-only or preview an uncreated root, fetch the
  current per-file catalog and effective ignore policy, and hash local files.
  Policy/catalog errors fail closed; capture-mode preview requires a server URL.
- Preview shares the regular-file, confined, 64 KiB hashing implementation used
  by proof bootstrap. It trusts no metadata hash cache, rejects escaping symlinks,
  and does not accept hashes from files whose identity/metadata changed during
  hashing. Refactoring proof verification retained the actual hash/size comparison
  at its caller; matching metadata alone still cannot register a proof.
- The preview diff follows the per-file protocol: force creates replacement
  versions for selected live files; renames are add/remove, not root-generation
  moves. Tombstones are omitted from the base and unselected paths cannot become
  removals. Output always says dry-run, including unchanged results. It reports
  source changes without claiming a precise append-aware upload byte count.
- No spool, head, proof, or metadata cache is created or updated. Tests hold the
  real capture lock and place an invalid journal in the pending directory, then
  verify the preview succeeds without reading/executing or changing that journal.
  Existing-root unchanged/force and new-root full/subset entrypoints exercise
  GET-only HTTP fakes, with no legacy state or mutation endpoints available.
- The Go suite and focused CLI race tests passed. Further tests cover ignore and
  subset behavior, add/remove rename semantics, policy failure, cancellation and
  symlink confinement. No database schema or worker changes were made this step;
  database integration tests were not rerun. No production services were used.
- Preview does not simulate already-pending captures; actual sync resumes them
  before scanning again, as the output states. Concurrent capture conflict
  recovery, retention, scalable reads, historical recapture, live validation and
  removal of the legacy pipeline remain required before cutover.

### Explicit concurrent-capture recovery

- Version registration now distinguishes a definitive base-version conflict
  (`409`, `capture_version_conflict`) from other failures. A Postgres-backed API
  test checks whole-batch rollback, including a new file preceding the conflict:
  no partial file/version/extraction/work/proof records or extra queue messages.
  Source artifacts already uploaded remain retained.
- Ordinary capture sync stops on this conflict without altering the pending
  request. Explicit `--force` retries the original journal first; only definitive
  rejection moves it unchanged into the local `conflicts/` directory. The next
  phase reads the latest catalog and captures current live files with a new ID.
  It never changes the rejected request's base or calls that request accepted.
- Recovery checks journal/root/server/capture identity, acceptance state,
  exclusive journal ownership, full selection of the rejected batch, and archive
  collisions. A rename preserves all journal and pack bytes; parent directories
  are synced. If syncing fails after rename, the error identifies the moved
  capture's location. Archived spools are recoverable and not automatically GC'd.
- HTTP integration tests cover default refusal, retained original bytes,
  recapture against the newer head, and an ambiguous acceptance response followed
  by exact-payload replay without another upload/archive. Separate tests verify
  that authorization errors, generic conflicts and server failures remain pending
  even with force, plus the archive guards above. Fresh-capture conflicts stop
  the invocation; recovery does not spin on a concurrently changing root.
- The full Go suite passed with disposable Postgres 17, and focused CLI conflict
  and journal race tests passed. No production credentials, paid provider calls,
  deployments or source deletion were used. Python workers were unchanged and
  their suite was not rerun for this step.
- This closes the stale-base retry loop, not the full migration: retention,
  scalable reads, multipart/provider lifecycle recovery, historical recapture,
  live validation and legacy removal remain outstanding.

### Path-scoped file-read visibility

- Single-file content reads no longer load the entire root catalog or serialize
  every published extraction/tombstone into the index filter. Their visibility
  snapshot now restricts the catalog query by the requested path, using the
  existing unique `(root_id, path)` index. Root-wide search retains its existing
  snapshot path; the shared filter construction preserves migration semantics.
- The line-range metadata fallback now has an explicit file-path interface and
  queries only that path's assigned namespace instead of fanning out to all root
  shards. Page/line constraints, index-side publication filtering, bounded chunk
  pagination, and the existing caller-side ACL/content-proof checks remain.
- A Postgres test inserts 10,000 unrelated catalog tombstones and checks that the
  selected-file filter stays below 512 bytes through pending, published, updated
  and deleted states. Unpublished captures retain the prior publication; paths
  not yet managed by the catalog retain legacy visibility during migration.
- A four-shard Postgres/HTTP integration fixture checks the actual serialized
  filters and request destinations for both content and metadata reads. Only the
  selected extraction/path and shard are queried. The full Go suite and focused
  server race tests passed against disposable Postgres 17; no production calls
  or deployments were made.
- Root-wide search still constructs a root-sized publication filter. This step
  removes that overhead from file reads, not from search; scalable search,
  cutover validation and legacy removal remain open requirements.

### Standalone SVG transformation

- SVG was listed as an image format but previously fell through to Pillow,
  which could not decode it. It now uses the existing PyMuPDF renderer, producing
  a temporary RGB PNG with `frame_number: 0`, then the ordinary Gemini Batch
  image-to-Markdown/chunk path. No new service or renderer dependency was added.
  PDF and SVG share pixel-size validation, the 2400-pixel maximum edge, and
  iterator-scoped cleanup. Generated PNGs are not persisted in S3.
- SVG preparation reads at most 32 MiB plus one limit-check byte, rejects DTDs
  and entities with defusedxml, and requires an SVG root. Image references must
  embed PNG/JPEG data; symbol references must be local fragments. Both `href`
  and `xlink:href` are checked. External/relative assets, scripts and HTML
  `foreignObject` content fail explicitly. The renderer receives bytes without
  a filesystem/archive resolver. This supports standalone static SVG within
  MuPDF's SVG feature set, not browser-equivalent CSS, animation or HTML layout.
- Primary renderer documentation: [PyMuPDF SVG input support](https://pymupdf.readthedocs.io/en/latest/pixmap.html#supported-input-image-formats).
  Tests render real vectors, symbol references, embedded PNGs and transparent
  backgrounds; check bounded output and deletion on iterator close/exhaustion;
  and reject external assets, active content, entities and oversized inputs.
- A real-Postgres/provider-fake integration test passes the rendered PNG through
  the existing upload, request mapping, batch submission and preparation seal.
  It verifies the frame anchor, `waiting_provider` state and temporary PNG
  cleanup. Live Gemini extraction, full Office-format fixtures and deployment
  validation remain outstanding; this is not a cutover claim.
- All 148 Python worker tests passed against disposable Postgres 17, with real
  local rendering and simulated cloud/provider IO. No tests were skipped.
  No Go/schema changes, production calls or deployments occurred in this step.

### Real Office conversion fixtures

- Added Linux integration tests generating synthetic DOCX and PPTX inputs, plus
  actual LibreOffice exports to DOC, ODT, RTF, PPT and ODP. All seven formats go
  through the production local conversion and temporary-PNG iterator. Tests
  check two-page/slide coverage, page anchors, RGB output, the 2400-pixel edge
  bound, source preservation and cleanup after exhaustion. Distinct red/blue
  slides verify ordering by rendered pixels rather than only page counts.
- A concurrent same-basename fixture exercises separate LibreOffice profiles
  and validates that PDFs retain their respective document text. PDF text is
  only a test oracle; the production extractor continues to send rendered
  images to Gemini. PDF inspection is serial because [PyMuPDF does not support
  multithreaded use](https://pymupdf.readthedocs.io/en/latest/recipes-multiprocessing.html).
- CI now installs LibreOffice Writer/Impress/Calc and the two fixture-generation
  libraries. Its existing no-skipped-tests gate requires the new fixtures to
  execute. The worker image also explicitly includes DejaVu fonts, matching the
  Linux test environment. This does not add document-generation dependencies
  to deployed workers.
- Initial cold-container concurrent runs exposed a LibreOffice exit 134; later
  runs passed, including with fresh shared XDG cache/config directories. The
  fixture captures converter stderr for diagnosis using synthetic data only.
  This failure is not yet explained or claimed fixed. Production stderr
  suppression remains unchanged.
- Final verification: all three Office integration tests passed in a new
  Python 3.12 / Debian trixie container with normal worker environment settings
  (no XDG override). Seven existing visual-preparation tests also passed locally.
  CI changes were not pushed or run in GitHub Actions. Test containers were
  removed; only synthetic temporary inputs were used, with no provider calls.
- These fixtures do not prove every template/macro format, arbitrary customer
  document fidelity, paid Gemini extraction or deployed Modal behavior. Full
  cutover validation, historical recapture and legacy removal remain required.

### Refreshing expired provider inputs before submission

- [Gemini Files API uploads expire after 48 hours](https://ai.google.dev/gemini-api/docs/files).
  Previously, resumed preparation and failed-page retry batches reused those
  URIs indefinitely. Both entrypoints now check existing temporary inputs before
  a not-yet-started batch is submitted. HTTP 404 or an explicit failed file state
  triggers replacement; authorization, rate-limit, transport and server errors
  defer instead of causing a new upload. Active inputs require no source read.
- Collector retries reconstruct expired inputs from the immutable S3 manifest
  and hash-verified captured bytes. Resumed transformation can reuse its already
  verified local source. Only missing PDF pages/image frames are rasterized and
  uploaded; successful provider request results remain unchanged. Office files
  still require local conversion to PDF. Media still decodes sequentially to
  preserve sample-derived timing, but only missing clips are uploaded. Temporary
  images, PDFs and WAVs are not written to S3.
- Replacement preserves request keys, ordinals, location metadata and inference
  attempt counts. The batch row is locked only for the short reference update,
  with a compare-and-swap against the old file identity. Started, submitted and
  ambiguous batches are never refreshed. Their existing reconciliation path
  remains authoritative; uploads whose responses/DB writes are lost can still
  become provider orphans until expiry.
- Submission builds its small JSONL envelope from the current request snapshot.
  Before marking submission started, it locks the batch and compares exact file
  IDs/URIs with that snapshot. A concurrent refresh invalidates the submission
  before a paid create. Conversely, a submission marker committed during refresh
  prevents the late refresh from changing any mapping. The explicit refresh step
  and shared upload-readiness helper follow the simple-data-flow skill; no
  additional queue, service or database schema was introduced.
- Postgres/fake-provider tests cover source reconstruction, corrupt source
  rejection, unchanged successful results, valid-file reuse, non-404 failures,
  both refresh/submission races, deletion before retry and initial-preparation
  resumption. A rendering spy verifies selected-page refresh calls the rasterizer
  only for that page. No live provider calls were made.
- File expiry after submission remains a provider batch failure, handled by the
  existing failed-request retry budget. This does not resolve an ambiguous paid
  create with no discoverable job, orphan upload cleanup, exhausted retries,
  historical recapture or final production cutover.
- Validation: the Linux Python 3.12 / Debian trixie suite ran all 168 tests with
  real LibreOffice, FFmpeg and disposable Postgres 17; 167 passed. The only
  failure was the already-observed concurrent same-basename LibreOffice test.
  Captured stderr reports `com::sun::star::lang::WrappedTargetRuntimeException`
  followed by `Unspecified Application Error` and exit 134 (also a javaldx
  warning). This is evidence of the failure, not a diagnosed root cause or fix.
  All provider refresh tests passed. Transform/collector Modal definitions
  imported locally; no deployment or paid provider call occurred. The full
  suite is not green and the Office failure remains a validation gate.

### Isolating LibreOffice's shared extension cache

- Separate `UserInstallation` profiles did not isolate all startup state.
  Synthetic concurrent-process traces show both invocations probing
  `/usr/lib/libreoffice/share/uno_packages/cache/stamp.sys`: one exclusive create
  fails with `EEXIST`, and a later removal races with the other process. The
  [LibreOffice 25.2 package-manager source](https://raw.githubusercontent.com/LibreOffice/core/libreoffice-25.2.3.2/desktop/source/deployment/manager/dp_manager.cxx)
  confirms that the shared extension cache uses this write probe separately
  from the user profile. The trace did not capture the fatal exception's C++
  stack; the shared-state diagnosis is supported by the trace and isolation
  tests, not a claim that every LibreOffice exit 134 has this cause.
- `office_pdf` now gives each conversion a private, mode-0700 temporary
  `UNO_SHARED_PACKAGES_CACHE` in addition to its existing isolated profile.
  Workers use built-in conversion filters, not administrator-installed shared
  extensions. Conversion remains an ordinary local subprocess with the same
  macro-security setting, timeout and output checks. No retry, global lock,
  extra service, or generated S3 artifact was added. This follows the
  simple-data-flow skill by removing shared mutable state at the IO boundary.
- The production-path concurrent fixture passed in ten independent fresh
  Python 3.12 / Debian trixie containers: twenty conversions, no crashes. Each
  pair uses the same basename with different document contents; the test checks
  distinct cache paths and validates each resulting PDF's identity and pages.
  This is evidence for the isolation fix, not a universal converter guarantee.
- A failure-path regression injects both a converter crash and a timeout.
  It verifies one invocation (no hidden retry), macro security, removal of the
  temporary output/profile/cache directory, and unchanged captured source bytes.
  Seven-format real Office fixtures continue to exercise conversion and page
  rendering. No provider calls, production credentials or deployment were used.
- Final verification: all 169 worker tests passed with zero skips in Python
  3.12 / Debian trixie, real LibreOffice and FFmpeg, and disposable Postgres 17.
  The eight visual-preparation unit tests also passed on the host. Go and
  database-schema code were unchanged in this step; the Go suite was not rerun.
  The disposable test database and test containers were removed afterward.
- This resolves the observed local Office validation failure; historical
  recapture, live-provider validation, scalable root-wide search, retention and
  deletion recovery, and final cutover/legacy removal remain required.

### Root deletion recovery after late writes

- Reproduced the exact failure with real Postgres and the production index
  publisher: start an index request, delete the root (cascading catalog/work
  rows), then apply the external write. Its acknowledgment correctly loses
  ownership, but the simulated index still contains the deleted document.
  There was no remaining catalog identity from which to schedule its removal.
- Migration 034 retains root cleanup targets independently of cascading root
  and organization rows. Transactional hooks capture active/retired namespaces,
  the legacy namespace, and exact root/generation S3 prefixes before deletion.
  Marking `deleting_at` also captures targets; organization deletion marks its
  roots before any child-table cascades. Rollback removes the targets together
  with the deletion marker. Already-marked roots are backfilled by the migration.
  Root IDs/organization ownership are immutable, and deleted IDs cannot be
  reused. A test confirms a concurrent replacement INSERT really blocks on the
  old root and then fails after deletion commits, not just a sequential check.
- The existing scheduled reconciliation role now performs bounded deleted-root
  cleanup, not a new queue or deployment. It reserves up to 25 maintenance
  targets with a five-minute retry delay and starts work within a 30-second
  soft budget. No database transaction remains open across provider requests.
  Successful targets are checked again after one day; backlog may extend that
  delay. Tombstones are not discarded after an apparently successful sweep.
- Index erasure records are packed in S3 under `maintenance/root-deletions/`
  before application, referenced in Postgres, validated against the immutable
  tombstone, and reused on partial/failure retries. Cleanup uses the recorded
  namespace plus `root_id = deleted root`, not namespace-wide deletion on
  replay. The [Turbopuffer partial-delete contract](https://turbopuffer.com/docs/write#param-delete_by_filter_allow_partial)
  determines whether another pass is required. Other roots' rows survive even
  in a reused namespace. HTTP 404 is already absent, not a permanent failure.
- Root source/output erasure lists one page and batches up to 1,000 keys into
  [S3 DeleteObjects](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html).
  It checks per-object errors and validates every returned key's exact prefix.
  Once current objects are drained, it lists/aborts at most ten matching
  [multipart uploads](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListMultipartUploads.html).
  Deadlines are checked between deletion/abort calls. Interrupted passes restart
  from the remaining first page; `NoSuchUpload` is treated as already gone.
- This follows the simple-data-flow skill by retaining explicit target records
  rather than relying on a disappearing work object. The race-debugging skill
  informed the controlled event timeline and the database-blocking check.
  Tests cover the API's storage-failure retry, durable intent before external
  deletion, root/org cascade survival, rollback, ID reuse, late index writes,
  S3 mutation-write failure/tampering, partial/lost responses, root isolation,
  1,503 objects across pages, twelve multipart uploads, and soft deadlines.
- Verification: full `go test ./...` passed with disposable Postgres 17; the
  focused Go root-deletion tests also passed under the race detector. All 182
  Python worker tests passed with zero skips in the Linux Python 3.12 / Debian
  trixie environment, including real Office/FFmpeg fixtures. The updated Modal
  reconciliation definition imports locally. No live S3/index mutations,
  production credentials, deployment, commit or push were used.
- Rollout requires migration 034 and the reconciliation principal's additional
  S3 delete/multipart-list/abort permissions, documented in configuration. The
  external Modal principal is not provisioned by the ECS Pulumi stack; no
  unnecessary permission was added to ECS consumers. Tests use synthetic data;
  deployed IAM/provider behavior remains unverified.
- Limits remain explicit: failed API deletes can leave root metadata marked
  deleting until retried; old hard-deleted roots predate these tombstones;
  versioned-bucket history, organization-shared embedding packs, Google files,
  and local capture spools are not purged by this sweep. Ordinary sync and
  file-level deletion retain captured versions/sources/outputs. Tombstones and
  small erasure artifacts are retained indefinitely. This step does not complete
  the broader retention, historical recapture, live-validation or cutover work.

### Resuming provider preparation without regenerating recorded inputs

- A real-PDF/Postgres regression exposed unnecessary rendering on initial
  preparation retry: with 64 recorded/submitted pages of a 67-page PDF, the
  previous code rasterized all 67 pages before its per-page database lookup.
  The new path renders only pages 64, 65 and 66. Existing successful request
  records/result references remain unchanged in the database fixture.
- Preparation now reads the durable request prefix in bounded 64-row pages,
  validates contiguous ordinals, and resumes each recorded batch identity before
  preparing the unrecorded tail. The existing unique extraction/ordinal index
  supports these reads. A metadata-read page may split a provider batch; this
  does not create/resubmit that batch again. No per-new-page existence lookup,
  migration, extra worker role, queue, or alternate batch format was added.
  Attempt heartbeats, batch reservation ownership, submission markers and the
  complete-request-count handoff seal remain authoritative.
- Prepared visual and media inputs now both carry their original `ordinal`
  alongside path, MIME type and source location. Both accept an ordinal
  selection. Provider refresh consumes that common record directly instead of
  reconstructing media versus image/page ordinals through separate branches.
  This follows the simple-data-flow skill: make the identifying data explicit
  and keep the resume/prepare/submit order visible in one function.
- PDF/image preparation skips rasterization/normalization of recorded inputs.
  Media still decodes sequentially from the beginning to preserve exact sample
  boundaries, but discarded clips do not create WAV files. Reading the first
  sample buffer before opening a WAV also avoids creating empty EOF files.
  Selected clips retain their original PCM samples, timestamps and ordinals.
  Iterator cleanup, decoder timeout/protocol restrictions and temporary-only
  image/media storage are unchanged.
- Tests cover 64 recorded plus three new pages (three rasterizations), one
  expired page in a reserved prefix plus three new pages (four rasterizations),
  a provider batch spanning two metadata-read pages, crash after final submit
  before sealing, missing prefix mappings, and ambiguous submission stopping
  before rendering/upload. A real-FFmpeg/provider-fake test refreshes only the
  middle expired audio clip while preserving its original location and the
  other clips' references. PCM comparisons verify selected range/set behavior
  and that only selected clips open temporary WAV outputs.
- All 194 worker tests passed with zero skips in Linux Python 3.12 / Debian
  trixie, with real LibreOffice, FFmpeg and disposable Postgres 17. The updated
  transform and collector Modal definitions import locally. No Go/schema
  changes were made this step and the Go suite was not rerun. No production
  credentials, provider calls, deployment, commit or push were used.
- This does not remove all retry computation: transformation still downloads
  and verifies the captured source, Office may reconvert to discover the tail,
  and audio decodes preceding samples. Expired Office inputs may require a
  separate conversion per refreshed batch. Uploads lost before request mapping
  remain unrecorded and can be regenerated; provider orphan handling is still
  pending. Mappings are scoped to the same immutable source/extraction revision;
  semantic converter or clip-boundary changes require a new revision, not
  reinterpretation of old mappings. Historical recapture, retention, scalable
  search, live validation and final legacy cutover/removal remain required.

### Historical inventory coverage and local recapture audit

- Reproduced a migration blind spot: ordinary per-file status correctly reports
  its one cataloged file as complete, but cannot account for two legacy-only
  paths. One still has local bytes; the other's original is missing. Retiring
  the old index on the strength of that status alone would be unjustified.
- Added `sync audit ROOT_ID --source PATH [--json]` in capture mode. It uses the
  existing root/state/catalog read endpoints, explicitly supplied local root,
  confined 64 KiB-buffer hashing and a pure per-file status decision. There is
  no new deployment, table, queue, uploader, or processing path. The old inventory
  and authorized captured paths are joined by path; current ignore patterns do
  not make historical paths disappear from the report.
- Reports absent originals, required captures, missing source metadata,
  pending/failed processing, exact-version publication and explicit deletion.
  Each record identifies historical membership and whether present local bytes
  still match the legacy hash. Missing files are never converted to tombstones.
  `catalog_covered` is the only successful command result; empty/incomplete
  inventories fail, while an incomplete `--json` report remains parseable.
- Null or unavailable legacy state, duplicate catalog paths, an advancing legacy
  generation, root identity changes, unsafe paths, escaping symlinks, nonregular
  files and unstable local reads fail closed. HTTP and hashing honor cancellation
  and the audit deadline. The tests caught and fixed null-as-empty acceptance
  and Cobra usage text appended to failed JSON reports.
- A local HTTP/storage-fake integration exercises the real existing capture
  implementation: two historical document paths become one immutable source
  pack and one registration; reconstructed extent bytes match their historical
  hashes. Repeating sync creates no additional upload or version. Audits remain
  incomplete while publication is pending, then report catalog coverage when the
  fixture records publication. This is not real document extraction/indexing.
- All new audit tests pass, including race checks. `go test ./... -count=1`
  passed with disposable Postgres 17 supplied through the test-only DSN. The
  actual CLI help was checked and `git diff --check` passed. Python workers and
  schema were unchanged, so the Linux worker suite was not rerun this step.
  No repository credentials, production calls, deployment, commit or push used.
- Configuration docs now describe preview -> ordinary capture -> wait -> audit,
  explicit missing-original handling and the limits of the evidence. The audit
  is not a full source-storage verifier: `source_storage_verified` remains false,
  S3/index contents are not read, unseen local paths are not scanned, path ACLs
  still apply, and live catalog/local observations are not an atomic snapshot.
  Already-lost legacy inventory cannot be reconstructed by this command. Actual
  historical recapture, source/index integrity checks, scalable search, retention,
  live provider/worker validation and final cutover/retirement remain unfinished.

### Catalog-bound source integrity and retained-source verification

- Reproduced a source-identity gap in transformation: a self-consistent S3
  manifest containing the wrong file hash could pass its own byte checksum and
  publish chunks under the requested catalog version. The new regression failed
  before the fix (`ValueError` not raised). The source reader now requires trusted
  catalog metadata rather than a bare object key. It checks the owning root,
  SHA-256-addressed manifest bytes, catalog file hash/size, and root-local pack or
  multipart extents before any source-pack read. Both transformation and expired
  provider-input refresh use this same boundary. Empty and malformed manifest
  representation checks now agree with the Go capture contract more closely.
- Added a read-only operator command, `python modal/source_verify.py --org-id
  ORG_ID --root-id ROOT_ID --bucket BUCKET`, not another Modal deployment. It
  checks every retained per-file version, including old originals behind deleted
  heads, against source ownership/completion metadata and full reconstructed
  SHA-256. It emits JSONL version evidence and a final summary; no uploads,
  enqueues, repairs, publication changes or deletions are performed.
- Metadata reads use explicit short read-only transactions with 15-second
  statement timeouts. S3 downloads occur outside database transactions. The scan
  is capped at its initial version-sequence watermark, uses pages of 64, and
  rechecks inventory counts/order at the end. Concurrent captures, missing roots,
  per-version failures and interrupted/empty scans cannot yield a successful
  final summary. This is live verification, not a cross-system atomic snapshot.
- A private 256 MiB temporary pack cache shares GETs across file/version extents,
  limits individual packs to the capture API's 128 MiB cap, and copies through
  64 KiB buffers. Corrupt/short/oversized downloads leave no partial cache files.
  Normal exit/error cleanup removes scratch; no source bodies enter reports.
  Cache eviction may redownload a pack, and manifests are still read per version.
  Pack GET call counts and transferred/verified byte totals are reported.
- Tests verify three retained source versions plus a deletion tombstone using
  one pack GET; wrong bytes fail only affected versions; 69 versions cross the
  pagination boundary; missing completion metadata prevents pack access. Other
  checks cover foreign/deleting roots, deletion during a read, concurrent new
  versions, deadlines, cache eviction/size bounds and secret-safe failures.
  An attempted SQL update inside the verifier's transaction is rejected by real
  Postgres. Work rows remain unchanged. Provider refresh rejects a mismatched
  catalog hash without replacing any request mapping or uploading a page.
- The actual operator CLI is tested in subprocesses with the real AWS SDK,
  a local HTTP S3 fixture and isolated Postgres. Both intact and missing-pack
  cases return the expected exit code and parseable final summary without
  credential leakage. CI now installs boto3 for this test. This is not live S3,
  Gemini, Modal GPU or Turbopuffer validation.
- All **220** Python worker/operator tests passed with zero skips in Linux
  Python 3.12 / Debian trixie, real LibreOffice/FFmpeg and disposable Postgres 17.
  Transform and collector Modal definitions import locally; `git diff --check`
  passes. No Go/schema code changed, so the Go suite was not rerun this step.
  No repository credentials, production data mutations, deployment, commit or
  push were used. Configuration docs describe the operator command and limits.
- The transaction guard follows [Postgres READ ONLY semantics](https://www.postgresql.org/docs/17/sql-set-transaction.html);
  source reads use the existing [S3 GET interface](https://docs.aws.amazon.com/boto3/latest/reference/services/s3/client/get_object.html).
  Operators should still use least-privilege credentials. The verifier proves
  readability of the checked bytes during its run, not future retention or
  index correctness. Actual historical recapture, live source/extraction/vector/
  mutation/index validation, scalable search, retention completion and final
  deployment cutover/legacy removal remain required.

### Read/search endpoint contracts and fail-closed path permissions

- Reproduced two failures through actual Go HTTP handlers with isolated Postgres
  and a contract-checking local index boundary. Renaming the test schema's ACL
  table caused reads/search to return private content with HTTP 200 and allowed
  write checks. A fresh per-file index schema rejected read, FTS, vector and
  hybrid requests because they explicitly requested absent legacy image fields.
- ACL query failure now fails closed: read/search return a generic permissions
  error, including failure during the post-fetch check. Legacy write checks deny
  access. Version registration loads path ACLs once per batch, instead of once
  per file, and stops before catalog/S3/SQS effects on lookup failure. Existing
  root authorization and literal deny-prefix semantics are unchanged.
- The index query boundary now takes an explicit typed wire request shared by
  single and hybrid queries. Read/search use one exclusion projection rather
  than duplicated legacy inclusion lists. It retains extraction identity for
  split-line assembly but excludes vectors, generated-image references and
  internal artifact/generation metadata. This follows Turbopuffer's documented
  [attribute projection contract](https://turbopuffer.com/docs/query): unknown
  excluded attributes are ignored, unlike unknown included attributes. No schema
  metadata lookup, extra index request or migration column was added.
- Nine new endpoint test cases cover fresh/legacy schema projection, all three
  search modes, a 700-chunk page, a long JSONL line split across 701 chunks, CRLF
  preservation, legacy overlapping line chunks, page-only metadata errors,
  publication changes, and deletion while old rows remain physically present.
  Unpublished/obsolete rows outnumber top-k and precede current rows in the index
  fixture, proving the API sends its publication predicate before result limits.
  File reads query only the selected shard; search fans out across four shards.
- Authorization tests cover missing identity/scope, foreign/restricted roots,
  independent path/hash/user proofs, literal bracket-containing ACL prefixes,
  admin proof bypass without bypassing path denials, ACL failures before and
  after index fetch, and registration failure without catalog/queue effects.
- `go test ./...` passed against disposable Postgres 17. Focused read/capture/
  visibility tests also passed under `go test -race`. These use real HTTP handlers
  and database transactions, not live Turbopuffer ranking or Modal inference.
  The separately tagged live CLI integration suite was not run. Python workers
  and schemas were unchanged; their previous 220-test result was not rerun here.
  No production credentials, deploy, commit or push were used.
- Remaining gates include scalable root-wide publication filtering (the current
  query still serializes the root's published extraction IDs/managed paths),
  live provider/index validation, historical recapture, retention completion,
  and final cutover/legacy retirement. Large read-fragment concatenation still
  needs allocation measurement; endpoint correctness tests are not throughput
  or whole-system completion evidence.

### Candidate-bounded search publication validation

- Measured the root-wide filter problem through HTTP: a root containing one
  searchable file and 25,000 unrelated tombstones sent **439,474 bytes per shard**
  for `top_k=1`. The regression test failed against the previous implementation.
  The same fixture now sends **539 bytes per shard**, with the same four index
  requests on the clean path. This measures request bytes, not live latency or
  Turbopuffer throughput.
- Removed the root-wide visibility snapshot path. Search fetches bounded ranked
  candidates, loads only their file publications through `(root_id,path)`, and
  retries after excluding rejected extraction IDs or replaced legacy paths.
  It returns a complete validated rank set, not the leftovers of a fixed top-k.
  Hybrid ANN/BM25 lists are both validated before fusion; namespace routing,
  query-embedding isolation and downstream ACL/content-proof checks remain.
- Each file's first observed publication is pinned in a request-local map. A
  test publishes a formerly pending version between retry passes: the in-flight
  search retains the earlier publication and the next search sees the new one.
  This avoids excluding both versions during a concurrent update. No whole-root
  atomic snapshot is promised, and no database transaction or connection is held
  across index IO. The map is bounded, not a shared or persistent cache.
- Per namespace, validation allows at most 16 passes, 4,096 exclusions, a 1 MiB
  conservative exclusion-string encoding budget and 8,192 observed candidate
  paths. Exhaustion or a nonadvancing provider response returns explicit HTTP
  503 (`search_publication_busy`, `Retry-After: 1`) with no partial success. An
  unusually stale-heavy index can therefore return 503 until cleanup progresses;
  this is a resource bound, not evidence that every workload fits it.
- Each root's database/index search now has a 30-second context. Query and
  multi-query HTTP requests and their retry backoff honor cancellation; a search
  timeout returns 504. Query embedding precedes this budget, and selected roots
  are processed separately. The old search-only hybrid transport wrapper was
  removed. Incomplete multi-query responses now fail rather than silently losing
  a ranking list. Legacy write transport behavior otherwise remains.
- Single-file snapshots share the indexed candidate lookup, still scoped to one
  path. A root/left-join check distinguishes an uncataloged legacy path from a
  deleting/foreign root; disappearance no longer grants legacy fallback. Real
  `EXPLAIN (ANALYZE,BUFFERS)` verifies the root/path index with 25,000 unrelated
  catalog entries. Root-deletion-during-fetch and publication-budget endpoint
  tests verify no content/partial results escape on failure.
- Verification: `go test ./...` passed against disposable Postgres 17; focused
  search/read/visibility/transport tests passed with `go test -race`. The previous
  700-chunk page, split JSONL line, legacy schema and access-control tests remain
  green. New tests cover pass/count/byte/cache bounds, nonprogress, malformed
  candidates, no held database connection, per-file snapshot changes, legacy
  handoff, HTTP/backoff cancellation and incomplete hybrid responses. A hanging
  cancellation fixture was corrected to drain the POST body and always release
  its test handler; the completed reruns above include that correction.
- The retry approach uses Turbopuffer's documented [exclusion-based search
  pagination and filtering](https://turbopuffer.com/docs/query). The local index
  fixture evaluates predicates/projection/limits, not real ANN/BM25 scoring.
  Live query/index validation and stale-heavy load/cleanup measurements remain,
  along with historical recapture, retention completion and final cutover/legacy
  retirement. No schema/Python worker changes, repository credentials, live
  provider mutations, deployment, commit or push were involved in this step.

### Bounded, batched index maintenance

- Raised the recurring published-file cleanup sweep from 25 to at most 1,000
  selected files, with at most eight concurrent index requests. The existing
  scheduled reconciliation deployment owns it; no queue, service, schema or
  mutation wire format was added. This removes obsolete index rows below the
  published version/revision cutoff, never below merely captured state.
- Reservations, immutable cleanup-pack locator installation, and successful
  completion checkpoints now use bulk SQL updates. A real-Postgres 25-file
  fixture measures five explicit SQL statements and eight peak index calls,
  with one S3 pack upload and one read. No database connection is held over
  index IO. This reduces metadata round trips and serial waiting, not the
  number of per-file Turbopuffer delete requests.
- Locator installation and completion retain the version/extraction guards;
  completion also matches the saved artifact reference. A rolled-back locator
  transaction starts no index writes. Lost index responses, partial deletion,
  and failed completion checkpoints retain the exact replayable mutation and
  five-minute retry eligibility. Successful files are rechecked after one day
  to catch late stale writes. A new publication remains immediately eligible.
- The soft 90-second sweep budget stops queued index requests before they
  start; already-started requests finish under their network timeouts and their
  successes are checkpointed. The deployment still has a 180-second timeout.
  These are resource bounds, not a guarantee that every selected file completes.
- Found and fixed an amplification introduced by larger sweeps: caching whole
  historical packs could retain up to 1,000 unrelated packs of 1,000 records.
  Cleanup now groups references, streams each pack once, and retains only the
  current sweep's selected records. The regression with four 1,000-record packs
  measured 34,013,923 peak traced allocated bytes before the fix and 126,766
  afterward on macOS (114,645 in the full Linux run). This measures traced
  allocations, not process RSS or live S3 performance; unrelated compressed
  records still must be read and decoded.
- Oversized and truncated packs cannot apply an otherwise valid prefix, and a
  shared bad pack is read once rather than retried for each file. Tests also
  cover one root's preparation failure without stopping or mixing another
  tenant, partial/failed files within a successful batch, and deadline expiry
  with eight started requests and 17 deferred files.
- Verification: 28 focused cleanup tests passed against disposable Postgres 17.
  All **230 Python worker tests passed, zero skips**, in the Linux container
  with real LibreOffice and FFmpeg (56.630 seconds). The 1,000-file local fixture
  completed in 0.108 seconds with fake S3/index IO and real Postgres, and a second
  sweep picked up the 1,001st file. This is not a live throughput benchmark.
  The reconciliation Modal definition imports successfully; `git diff --check`
  passed. Go code and schemas were unchanged in this step.
- The existing SDK creates namespace-scoped clients over a shared HTTPX pool;
  HTTPX documents its synchronous client's
  [thread-sharing contract](https://www.python-httpx.org/api/#client). No SDK
  configuration is mutated during the sweep. Real concurrent Turbopuffer
  throughput/rate-limit behavior remains to be validated before rollout.
- This addresses maintenance scaling needed by candidate-bounded search, not
  the entire cutover. Live provider/index integration, historical recapture,
  remaining retention/recovery work and final legacy retirement are still open.
  No production writes, deployment, commit or push occurred.

### Live Gemini model metadata check

- A read-only authenticated request on 2026-09-04 to
  `GET /v1beta/models/gemini-3.5-flash-lite` returned HTTP 200, canonical name
  `models/gemini-3.5-flash-lite`, and methods `generateContent`, `countTokens`,
  `createCachedContent`, and `batchGenerateContent` for the configured key.
- The repository `.env` was sourced only in that request's subprocess; no
  credentials were printed. No files were uploaded, inference or paid Batch
  jobs created, provider resources deleted, or production data mutated.
- This verifies the requested model's availability and advertised Batch method,
  not actual Batch request/result contracts, quota, transcription/diarization
  quality, or deployed worker connectivity. Those need their own live tests.

### Real Google SDK boundary verification

- Added `modal/tests/test_gemini_sdk.py` using the actual `google-genai` client,
  an in-process HTTPX transport and disposable Postgres. Previous provider tests
  supplied mock SDK objects, so they did not exercise wire serialization, typed
  response conversion or SDK pagination/retry behavior. This adds that layer
  without a new production abstraction or alternative ingestion path.
- The synthetic PNG/WAV fixtures pass through `upload_prepared`, real resumable
  upload requests, `reserve_batch`, `submit_batch`, preparation sealing,
  `collect_batch` and extraction assembly. Assertions cover upload size/type
  headers, JSONL MIME type, stable request keys, `fileData` references, the exact
  requested model, output-token bounds and structured audio configuration.
  Page and clip requests are combined only to test the provider interface;
  this is not a claim that one source file contains both modalities.
- The fake HTTP endpoint queries Postgres at the Batch create boundary to prove
  the submission marker and uploaded envelope identity are durable before the
  call. An HTTP 503 simulating an accepted request with an unavailable response
  causes exactly one SDK create request with `attempts=1`. The next local submit
  requires reconciliation; the SDK traverses a two-page listing and recovers
  the original job without submitting it again. Google's
  [Batch contract](https://ai.google.dev/gemini-api/docs/batch-api) explicitly
  states that repeated creates are not idempotent.
- Collection exercises the SDK conversion of the provider's `metadata.state`
  and `metadata.output.responsesFile`, then downloads JSONL through the real
  SDK. Reversed result order is restored to source order; clip-relative times
  become source offsets and speaker labels stay request-scoped. Repeated
  collection downloads once, writes only text artifacts and creates the pending
  index handoff. Truncated model output and a per-request status error cannot
  publish a partial file or create index work.
- The first fixture run incorrectly expected upload size in JSON metadata;
  the SDK sends it in resumable-upload headers. The fixture was corrected to
  validate those headers. No production defect was inferred from that fixture
  error, and production provider code was unchanged in this step.
- CI now installs `google-genai` for these mandatory, non-skipped tests. The
  focused module passed on macOS. All **237 worker tests passed, zero skips**,
  in Linux with real LibreOffice, FFmpeg, Postgres 17 and `google-genai` 2.22.0
  (57.751 seconds). `git diff --check` passed. The developer guide documents
  dependencies, isolated-database requirements and the local-only boundary.
- No application credentials were loaded, files sent to Google, paid jobs
  created, production records changed, services deployed, commits made or
  branches pushed. The disposable test database was removed. A tiny paid live
  Gemini test was requested through the approval UI and has not been approved
  or submitted as of this entry. SDK-level verification does not replace live
  model/diarization, quota, AWS/SQS/Modal/index integration, historical recapture
  or final cutover and legacy removal.

### Docker Compose E2E-only testing transition (2026-09-05)

- User instruction supersedes the previous testing approach: no unit tests or
  in-process integration tests. Removed 64 Go/Python/TypeScript test files and
  replaced CI/release test commands with `scripts/test-e2e.sh`. Uncommitted test
  contents were archived locally before removal; earlier test counts above are
  historical evidence, not verification of the current suite.
- `compose.e2e.yml` runs actual API, CLI, separate SQS consumers, transform,
  collector, CPU index, Nomic index, query and reconciliation roles. Production
  entrypoints and dependency files are reused. Postgres 17 and LocalStack S3/SQS
  run locally; Gemini and Turbopuffer remain real providers. No provider stubs,
  fake vectors, private handler calls or work-table fixture writes.
- Implemented capture, extraction/publication, search/read, root authorization,
  no-vector/vector separation, worker outage, append/rewrite/rename/delete,
  process restart, duplicate SQS delivery and cleanup phases. This is not yet
  coverage-equivalent to the removed suite; outstanding scenarios are listed
  explicitly in `tests/e2e/README.md`.
- Verified builds for production Go applications, worker/runner images and the
  real pinned Nomic image; Pulumi TypeScript build, Compose configuration,
  shell syntax and diff whitespace checks pass. API, Postgres, S3/SQS and worker
  HTTP processes start together; the query role returned one real 768-dimensional
  embedding over HTTP on CPU.
- The `capture` phase passed in 9.33 seconds: real CLI -> API -> Postgres/S3/SQS
  registered 1,024 synthetic files across 100 nested directories plus format
  fixtures, exercised 33 MiB multipart, pagination and ignored `.env`, and
  returned while both execution consumers were stopped. API/provider cleanup
  of this isolated run also passed. No Gemini batches or index writes were
  submitted. Actual extraction-through-index/search phases remain unrun pending
  approval for paid provider use. CPU execution is not CUDA/Modal/ECS validation.
- Before the testing instruction, fixed assembly's per-page retry-pack rereads:
  request metadata now uses 64-request windows and groups required pack reads
  within each window, with a conservative 64 MiB selected-output budget. The
  focused retired fixture measured 64 alternating-retry pages at 2 GETs instead
  of 64. That result is historical only until reproduced through a new E2E
  failure scenario; no new unit coverage should be added.
- No deployment, commit, push, production source recapture or cutover occurred.
  The broader pipeline goal remains incomplete. Configure a review-protected
  GitHub `e2e` environment with dedicated provider keys before running CI/PR code.

### Multipart interruption, expiry and completion recovery (2026-09-05)

- Re-read the goal and current worktree. The previous goal turn was progress:
  actual Compose roles, E2E-only source tests and a verified capture phase were
  added. Paid Gemini/Turbopuffer execution approval was still absent, so this
  turn exercised only the local capture/recovery path and isolated cleanup.
- Reproduced the next production defect through the real CLI, API, Postgres and
  S3 in Compose: kill the CLI after the first durable 16 MiB acknowledgement,
  abort its S3 session, then resume. Before the fix, resume failed with
  `multipart part upload returned HTTP 404`. No work-table fixtures, private
  handler invocation, mocks or unit tests were used.
- Added a Toxiproxy source-upload TCP hop to the Compose harness. Network
  bandwidth limiting makes interruption repeatable; disconnect/latency faults
  exercise real HTTP cancellation and AWS SDK behavior. The first harness run
  found its control endpoint bound to localhost; the container binding was
  corrected before the production regression was measured.
- Resume checks an existing session with bounded `ListParts(MaxParts=1)`.
  `NoSuchUpload` plus a missing completed object produces the explicit
  `source_multipart_expired` code. Generic errors remain retryable failures,
  never proof of expiry. The CLI persists a new upload UUID/key mapping and
  clears only old part acknowledgements while retaining exact spool bytes,
  file hashes and capture ID. No schema change or new production deployment.
- An already-completed S3 object is verified against its saved upload identity
  and size. A frozen completion manifest and live-root check allow the API to
  recover its missing DB acknowledgement without creating a replacement upload.
  The task IAM policy now includes `s3:ListMultipartUploadParts` (not deployed).
- The three-case Compose phase passed in **39.78 seconds**: active-session
  restart (including an S3 network outage that preserves journal identity),
  confirmed expiry, and S3 completion followed by a deliberately lost API
  acknowledgement. Every case rewrites the live file before resume and verifies
  the original 32 MiB capture is registered first, with independently hashed S3
  bytes, followed by a distinct version of the rewritten file. Recovery roots
  are also included in the full suite's later indexing/read verification.
- The emulator now receives the same one-day incomplete-multipart lifecycle
  configuration as Pulumi. The test uses explicit S3 abort to reproduce expiry;
  it does not prove real AWS lifecycle timing, IAM, or cloud connectivity.
  Bootstrap and API/fault-proxy readiness passed. Go build, Pulumi build, Compose
  validation, shell syntax and `git diff --check` passed.
- No paid Gemini batch or Turbopuffer index write, production data change,
  deployment, commit, push, historical recapture or cutover. The remaining full
  extraction/index/search phases still require approved provider execution;
  broader goal completion remains unproven.

### SQS outage and independent scheduled handoff recovery (2026-09-05)

- Added a separate Toxiproxy SQS hop for application roles. The bootstrap and
  black-box driver retain direct emulator access for setup and assertions.
  Production queue messages, SDKs and delivery code are unchanged.
- New CLI/API scenario captures 12 files while SQS is disconnected. All file
  versions commit, work remains pending with null enqueue acknowledgements and
  zero execution attempts, and the CLI succeeds. Only the scheduled reconciler
  starts; the collector and both execution consumers remain stopped. After an
  observed failed invocation, restore the connection and let the next normal
  schedule deliver the original IDs. Real SQS receipts prove that all 12
  reference-only messages exist; visibility is released without acknowledging
  them so later workers still execute the actual work.
- The first run exposed a Compose adapter defect: an exception escaped the
  scheduled-role loop and permanently exited its container. The adapter now
  logs the failed invocation and retains the 60-second schedule, touching its
  health heartbeat only on success. This models independent
  [scheduled invocations](https://modal.com/docs/guide/cron), not an automatic
  retry of a failed input; Modal's [input retry policy](https://modal.com/docs/guide/retries)
  is a separate mechanism. Serial execution and hard-timeout differences remain
  explicit in the E2E runbook.
- The next real scheduled invocation repaired delivery, then exposed a second
  harness configuration error before any cleanup request: the Python SDK reads
  `TURBOPUFFER_REGION` from its environment even when the application omits that
  argument, and rejects it with a fixed base URL. Compose now resolves one URL
  for Go/Python and does not also forward region. See the
  [SDK constructor](https://github.com/turbopuffer/turbopuffer-python/blob/main/src/turbopuffer/_client.py).
  No global environment mutation or provider-client replacement was added.
- The initial handoff assertions passed in 2.23 seconds for capture during
  outage and 48.72 seconds for scheduled delivery. Existing 1,024-file capture
  passed again in 8.66 seconds and all three multipart recovery cases in
  36.33 seconds, with both execution consumers still stopped. Isolated API
  cleanup passed. Full-suite ordering now also requires reconciliation health
  after delivery before starting collection; later publication/read assertions
  include the recovered handoff root.
- A clean rerun with both harness fixes passed outage capture in **4.67 seconds**
  and automatic delivery in **58.65 seconds**. The same reconciler container
  logged its initial connection failure, then reported 12 published handoffs
  with zero cleanup targets/failures and became healthy. No manual resend,
  shortened schedule, consumer or collector was involved.
- No unit tests were added. Go build, shell syntax, Compose configuration and
  diff whitespace checks passed. These are local capture/recovery results, not
  evidence of the unrun Gemini extraction, Turbopuffer publication/search,
  deployed IAM, historical recapture or cutover. No paid model submission or
  index write, deployment, commit or push occurred.

### Native transformation and completed-work replay in Compose (2026-09-05)

- Extended the full-suite pre-index phase through the actual CLI, Go API, SQS
  transform consumer, HTTP transformation worker, S3 chunk artifacts and index
  SQS. Only native-format jobs are queued while the transform consumer runs;
  the index consumer and collector stay stopped. No production worker logic or
  provider implementation is copied into the driver.
- Thirteen fixtures cover byte-preserving UTF-8/CRLF text, empty files, generic
  JSONL with a record over 1 MiB and an unfinished final record, identical JSONL
  under ordinary/session-like paths, CSV/TSV quoted multiline and oversized
  cells, sparse XLS/XLSX/ODS/FODS cell coordinates, XLSX formula expressions,
  and email/contact/calendar fields. Actual artifact assertions verify bounds,
  contiguous ordinals, content hashes, exact source bytes/line ranges, complete
  cell contents and addresses, and small index messages with production FIFO
  grouping. Later full-suite read/search checks include this same native root.
- Native capture passed in **0.43 seconds**. The transformation assertion phase
  passed in **2.15 seconds**, verifying **394 chunks across 13 files**, each
  transformed once, with 13 index jobs pending and zero index attempts. These
  phase timings exclude container startup and are not production throughput
  measurements. No provider batch, embedding or mutation was created.
- Added completed-work replay with the HTTP transform worker stopped and the
  Go consumer restarted. Three fresh SQS deliveries per completed job must
  drain without changed work attempts, artifact keys/ETags/timestamps or index
  execution. The driver uses the production per-work/per-file FIFO grouping,
  with fresh deduplication IDs only to force actual duplicate delivery.
- The first replay exposed a test deadline error: its 180-second wait expired
  while one received message was still inside the configured 300-second
  visibility interval, holding subsequent messages in that FIFO group. The
  read-only emulator message inspection confirmed the receipt/group state;
  it is not part of the suite and was never used to delete/reset receipts.
  Queue-drain waits now derive their budget from visibility plus long-poll
  settings and a 60-second margin. This preserves normal
  [SQS visibility and redelivery behavior](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html).
- The corrected run kept the same consumer alive and passed in **109.81 seconds**
  after the original visibility interval expired, draining the outstanding
  group plus 39 newly sent duplicates. Every transform remained at one attempt;
  chunks were unchanged and the transform DLQ was empty. This is recovery
  evidence, not a claim that duplicate processing normally takes two minutes.
- A subsequent replay with production FIFO group IDs passed in **1.17 seconds**
  with the same stopped HTTP worker and live restarted consumer. The earlier
  180-second failure remains recorded in the results; it was not replaced with
  a production visibility change or a test-driven receipt acknowledgement.
- Go build, Compose configuration, shell syntax and diff checks passed. No
  unit tests, paid model submission, index write, deployment, commit, push,
  historical recapture or cutover. Full provider-backed publication/search and
  deployed execution remain unverified and require approved execution.
  Isolated API cleanup passed in 0.74 seconds; the synthetic Compose containers
  and state/CLI volumes were removed, with sanitized diagnostics retained.

### Reference-only Go SQS publication (2026-09-05)

- Extended the native capture E2E phase to inspect actual API-published SQS
  bodies before starting the consumer. The first run failed: Go encoded 14
  fields for a per-file job, including six empty legacy generation/shard fields,
  while Python publishers already emitted the eight work-reference fields.
  A real queued Go message measured 490 bytes.
- SQS enqueue now selects the eight string references directly before its one
  JSON serialization: `job_id`, `work_id`, `org_id`, `root_id`, `file_id`,
  `version_id`, `extraction_id`, `stage`. No marshal/decode/filter round trip,
  new transport type hierarchy or change to message grouping/deduplication.
  Legacy jobs without `work_id` retain their existing encoding; NATS is unchanged.
- Rebuilt the production Go image and reran capture/transform in Compose.
  Thirteen new Go transform messages passed exact-field/reference/FIFO checks
  at **371 bytes each**, 119 bytes smaller (about 24%). Python index messages
  remained **367 bytes each**. This reduces body bytes, not API-call count, and
  is not evidence of lower SQS billing or a throughput improvement.
- The initial 13 pre-fix file messages were left queued. The real consumer
  handled those and 13 new messages: SQL showed **26 completed transformations,
  one attempt each, 788 chunks**, and **26 pending index jobs with zero index
  attempts**. This proves mixed old/new per-file wire compatibility, not a full
  legacy root-generation pipeline test. The new capture and transformation
  phases passed in 0.40 and 2.16 seconds, excluding image/container startup.
- Go build and diff checks passed; no unit tests, provider batches, index writes,
  deployment, commit or push. Full provider-backed validation, historical
  recapture, production cutover and legacy retirement remain outstanding.

### Continuous follow with an index backlog in Compose (2026-09-05)

- Added a black-box phase using one actual `pufferfs sync --follow` process,
  production two-second debounce, API registration, S3 uploads, SQS delivery
  and the real transformation worker. The index consumer remains stopped.
- Create, append, rewrite, truncate and delete produce five linked versions of
  one file. Every version reaches pending indexing without stopping capture;
  the public CLI status agrees. Append reuses prior extents and uploads only
  the suffix. Rewrite replaces extents; truncation has no source bytes.
- All four historical source manifests still reconstruct the expected hashes
  after deletion. Actual SQS inspection finds five eight-field index messages,
  367 bytes each, with production per-file FIFO grouping. All five work records
  remain pending with zero attempts; no Gemini batch or index write occurs.
- The phase passed twice in **22.38 and 22.41 seconds**, excluding image build
  and container startup. The final run uses offset-independent log inspection
  to observe watcher readiness without changing the child's stdout position.
  Full-suite verification also checks eventual deletion publication and a 404
  read for this root; those provider-backed checks remain unrun.
- Synthetic API cleanup succeeded; only this run's Compose containers, network
  and state/CLI volumes were removed. Sanitized diagnostics and phase results
  remain in the ignored artifacts directory. No unit tests, paid provider
  execution, deployment, commit, push, historical recapture or cutover.

### Malformed SQS batch isolation in Compose (2026-09-05)

- The real consumer previously returned an error for an entire received batch
  if any body failed JSON decoding, abandoning valid receipts too. A new
  black-box scenario captured two text files through the CLI and sent one
  malformed body through SQS in a valid job's FIFO group. The baseline failed
  after **90.64 seconds**: both real work rows remained pending at zero attempts,
  while all three receipts were invisible. No source/body or work-table writes
  bypassed the normal capture workflow; the only injected fault was SQS input.
- `SQSQueue.Pull` now skips only the malformed entry and returns valid jobs.
  The bad receipt is not acknowledged or reset. Logs contain message identity
  and receive-batch size, never the body. No new interface, queue, error wrapper
  or JSON round trip was introduced. This preserves SQS-owned
  [redelivery and DLQ policy](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-dead-letter-queues.html).
- The first fixed run exposed a separate setup race: after stopping a consumer,
  its outstanding receive completed server-side as new captures arrived,
  before the replacement consumer started. One valid receipt became invisible
  without reaching the new process; this run also failed its 90-second deadline.
  The test now lets the normal 20-second long poll expire before capture. It
  does not change visibility, redrive limits or production shutdown behavior.
- A fresh run passed with logs confirming **one three-message receive batch**.
  Both valid jobs transformed exactly once; their retained source hashes and
  chunk text/hashes matched the files. Two eight-field index messages existed,
  while queue state retained exactly one invisible malformed receipt. SQS can
  deliver multiple same-group messages in a
  [single receive batch](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/FIFO-queues-understanding-logic.html).
- The native-format scenario also passed again: **394 verified chunks across
  13 files**. Combined SQL state showed 15 completed transforms at one attempt
  each, 15 pending index jobs at zero attempts, and zero provider batches. The
  malformed assertion phase took 0.07 seconds after consumer startup; that is
  not an end-to-end processing-latency measurement.
- The full Compose script runs this scenario last, then starts indexing and
  checks both files through read and FTS. That publication phase remains unrun,
  as do real-provider full-suite validation and eventual malformed-message DLQ
  redrive. Both failed runs remain in the results; only isolated test resources
  are removed during API cleanup and Compose teardown.
- Go build, shell syntax, Compose configuration and diff checks passed. No unit
  tests, paid provider execution, deployment, commit, push or cutover.
