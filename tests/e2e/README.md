# Production-path end-to-end tests

All PufferFS application roles run in separate Docker Compose containers. The
test driver starts the real CLI and calls public HTTP endpoints. It does not
import application code, monkeypatch providers, seed work tables or invoke
transformation/index functions directly.

```text
test driver -> CLI -> API --capture metadata--> Postgres
                |      |--work IDs----------> transform SQS FIFO + DLQ
                |      |                            |
                |      |                    transform consumer
                |      |                            | HTTP {work_id, attempt_token}
                |      |                      transform worker
                |      |                       /           \
                |      |           native text/chunks    real Gemini Batch
                |      |                  |                  |
                |      |                  |             collector (60s)
                |      |                  +--------+---------+
                |      |                           |
                |      |                       index SQS FIFO + DLQ
                |      |                           |
                |      |                     index consumer
                |      |                      /          \
                |      |                CPU/no-vector   Nomic worker
                |      |                      \          /
                |      |                      real Turbopuffer
                |      |                            |
                |      +<--published version----- Postgres
                |      |
                |      +--query--> query embedder (separate Nomic process)
                |      +--search/read----------------> Turbopuffer
                |
                +--presigned PUT/multipart--> S3 source packs

CLI/API source-upload TCP traffic passes through the fault proxy before S3.
Application SQS traffic uses a separate fault-proxy port. Bootstrap and the
test driver's queue assertions connect directly to the emulator.
Only the test driver controls faults; production requests/bodies are unchanged.

workers/collector/index <--> S3 source, chunk, vector and mutation artifacts
reconciler (60s) <--> Postgres delivery ledger --repair send--> SQS
reconciler --bounded cleanup--> S3 / Turbopuffer
```

| Role | Compose service | Trigger / interface |
| --- | --- | --- |
| Local agent | `e2e` runs built `pufferfs` | Synthetic filesystem; HTTP registration + signed S3 uploads |
| API | `api` | Production Go image, real auth/migrations/catalog/read/search |
| Transform delivery consumer | `transform-consumer` | Actual Go worker claims SQS receipts; invokes HTTP transform worker |
| Index delivery consumer | `index-consumer` | Actual Go worker claims index SQS; selects CPU or Nomic endpoint |
| Transformation worker | `transform` | Same `transform_app.transform_file` entrypoint; LibreOffice/FFmpeg and actual Google SDK |
| Batch collector | `collector` | Same scheduled production function every 60 seconds |
| No-vector index worker | `index-cpu` | Same authenticated production entrypoint, actual Turbopuffer SDK |
| Bulk embedding/index worker | `index-vector` | Same Indexer class and pinned Nomic revisions; CPU device locally |
| Query embedding worker | `query` | Same QueryEmbedder class, separate process/model |
| Reconciliation worker | `reconciler` | Same production function every 60 seconds |
| Database | `postgres` | Postgres 17; production migrations run by Go startup |
| Object store / queues | `aws` | LocalStack S3 and independent SQS FIFO queues + DLQs |
| Test network fault injector | `faults` | Separate source-upload and SQS TCP proxies; driver can throttle or disconnect them |

Compose starts containers. The two Go consumers claim SQS messages and invoke
worker HTTP endpoints. Postgres records ownership, versions and recovery state;
it is not an execution queue. Modal's `.local()` adapter runs the same decorated
functions/classes inside these containers, without creating a Modal deployment.
The scheduled-role adapter logs failed invocations and continues its 60-second
schedule. Only successful invocations refresh the health heartbeat.

## Run

Requires Docker Compose, enough memory for two real Nomic processes plus Office,
and dedicated `GEMINI_API_KEY` / `TURBOPUFFER_API_KEY` credentials. Internet is
required for building pinned Nomic weights and for actual Gemini/Turbopuffer
requests. This is intentionally not a free offline suite.

```sh
set -a; source .env; set +a
bash scripts/test-e2e.sh
```

The script explicitly disables Compose's implicit `.env` loading. Only selected
provider credentials/endpoint settings are forwarded. Postgres, AWS credentials,
bucket and queues are fixed disposable values; production `DATABASE_URL` and AWS
credentials are never inherited by these services. No host ports, Docker socket
mounts or personal source directories are used.

Compose resolves one Turbopuffer URL for all roles, using `TURBOPUFFER_API_URL`
or `https://<TURBOPUFFER_REGION>.turbopuffer.com` (default `gcp-us-central1`). It
does not also forward the region environment variable: the Python SDK rejects
a region combined with a fixed URL, even if the application omits its region
argument. Use a fully resolved URL when explicitly overriding the endpoint.

Each invocation gets a unique Compose project, volume set and synthetic tenant.
Results and credential-redacted logs go to `tests/e2e/artifacts/`. The private
state volume holds fixture bytes and test API keys; it is not uploaded to CI.

## Scenarios implemented

1. Disconnect application SQS delivery, then capture 12 files through the CLI.
   Confirm all versions are accepted with unconfirmed delivery and zero worker
   attempts. Start only reconciliation, observe its failed scheduled invocation,
   restore SQS, and wait for the next scheduled run to deliver the same 12 work
   IDs across multiple SQS batches. Inspect real receipts and small reference-only
   bodies, then release visibility without acknowledging them. No registration
   retry, collector or execution consumer repairs this handoff.
2. Capture 13 native-format fixtures and inspect the API's actual SQS messages
   before starting only the transform consumer. Both Go API and Python worker
   publications must use exactly eight reference fields, without legacy
   generation/shard data, and the production FIFO grouping rules.
   Confirm retired upload/sync endpoints return 404, capture creates no
   generation/job rows, and `root current` resolves the captured local root.
   Verify the actual worker's S3 chunk bodies/hashes, UTF-8 byte/line ranges,
   generic JSONL with a record over 1 MiB and an unfinished final record,
   identical behavior under session-like filenames, CSV/TSV quoted multiline
   and oversized cells, sparse XLS/XLSX/ODS/FODS addresses, unevaluated XLSX
   formulas, and email/contact/calendar fields. Index SQS must contain only work
   references with production FIFO grouping; no provider batch, vector, mutation
   or publication may exist.
   Stop the HTTP transform worker and restart its Go consumer, then redeliver
   each completed transform three times. The consumer must acknowledge receipts
   without executing work or rewriting chunks. Queue-drain deadlines include
   the configured visibility timeout and long-poll duration: a receive response
   lost during restart can leave a receipt invisible until normal redelivery.
   Before that restart, keep one actual `sync --follow` process running through
   file creation, append, rewrite, truncation and deletion with its normal
   debounce. Every version must reach index-pending while the index consumer
   remains stopped. Verify status through the CLI, the linked version history,
   append extent reuse, retained historical source hashes after deletion, and
   five real index messages with zero execution attempts. No session-specific
   adapter, direct handler invocation or database mutation is involved.
3. Capture 1,000 text files in 100 directories plus native/code/JSONL, spreadsheet,
   document, presentation, image and media fixtures while **both consumers are
   stopped**. Confirm catalog pagination, SQS backlog, ignored `.env`, and real
   multipart for a 33 MiB file. Capture must not wait for indexing.
4. Kill the real CLI after one 16 MiB part is durably acknowledged. Exercise
   active-session resume, a disconnected S3 endpoint, confirmed S3 upload
   expiry, and S3 completion followed by a lost API acknowledgement. Rewrite
   the live path before resume: the first registered version must still hash
   to the original 32 MiB capture, then a separate version records the rewrite.
   The expired case aborts through S3 to emulate the one-day lifecycle condition;
   it does not fast-forward an emulator clock or edit application database rows.
5. Start consumers. Wait for exact captured versions, including the native-format
   root, live-follow deletion, recovered SQS handoffs and interrupted multipart
   captures, to be published. Check S3
   source hashes/ranges, contiguous chunks, mutation acknowledgements, PDF page
   reads, real FTS, root authorization and authenticated worker endpoints.
   Generated images and converted audio must not exist in S3.
6. Exercise a separate vector-enabled root with real Nomic embeddings and real
   vector/hybrid search; verify the no-vector root produced no embeddings.
   Then create/remove actual folder deny rules against the already published
   native root. Bare user IDs, `user:<id>`, roles and `*` must hide matching
   reads, FTS and catalog entries, reject proof writes and leave other folders
   accessible. Scoped ACL keys must work; removing rules restores access without
   new work, artifacts or publication changes.
7. Stop transform and CPU index workers. Append JSONL, rewrite, rename and delete
   local files through CLI sync. Verify durable capture while workers are down,
   old published content still readable/searchable for a pending replacement,
   immediate hiding of captured deletions, and unchanged source extents reused.
8. Restart workers, API and consumers. Verify current reads, deletion/rename,
   current-only search results, unchanged-file identities, repeated delivery through actual SQS and empty
   DLQs. Completed jobs must not execute again.
9. Stop both consumers, capture two text files, and send one intentionally
   malformed JSON body through SQS beside a valid job in the same FIFO group.
   Before capture, let any outstanding 20-second receive from the stopped
   consumer expire; otherwise that abandoned poll can take a new receipt before
   the replacement consumer starts, exercising a different failure condition.
   Start the transform consumer: both valid captures must become verified S3
   chunks and index jobs before the normal five-minute visibility timeout.
   Real consumer logs must confirm one mixed three-message receive batch;
   queue state must retain the malformed receipt without acknowledging it.
   Start indexing and verify both files through read and FTS. This runs after
   empty-queue/DLQ assertions; cleanup removes the isolated malformed receipt.
   It does not shorten visibility, reset receipts, or prove eventual DLQ redrive.
10. Delete only this run's roots/users/tenant through the API and verify source
   removal. Cancel unfinished provider batches recorded by the isolated DB.

Missing credentials, timeouts and failed cleanup are failures, not skips. The
default per-phase wait is 3,600 seconds (`PUFFERFS_E2E_TIMEOUT_SECONDS`). Gemini
Batch completion is asynchronous and may exceed this; a timeout is not proof
of a code defect. The scheduled collector deletes our tracked page/clip and
JSONL uploads after their durable consumers release them. Generated Gemini
batch-result files are different: Google's documented retention is six weeks,
and deleting the batch does not prove its result bytes are erased. Generated
images are never persisted in PufferFS S3.

## Cleanup and CI

The shell EXIT handler runs API/provider cleanup before removing Compose volumes.
If cleanup fails it retains the project and state and prints its exact name.
Retry with the same credentials:

```sh
docker compose --env-file /dev/null -p PRINTED_PROJECT -f compose.e2e.yml run --rm --no-deps e2e cleanup
docker compose --env-file /dev/null --profile test -p PRINTED_PROJECT -f compose.e2e.yml down --volumes --remove-orphans
```

Never use an arbitrary project name or remove volumes before external cleanup.
A hard host/CI kill cannot run an EXIT trap; provider-side test-account retention
and manual cleanup are still necessary. Do not upload `run.json` or the private
state volume. CI/release call `e2e.yml`: six isolated suite jobs, at most two
concurrently, each with separate artifacts and a 120-minute timeout.
Configure its `e2e` GitHub environment
with required reviewers and dedicated provider secrets before enabling it for
PRs. Never approve untrusted PR code to receive those secrets.

## In-flight index recovery suite

`bash scripts/test-e2e-index-recovery.sh` adds the optional
`compose.e2e-index-recovery.yml` topology, with fresh disposable resources and
the same production CLI/API/consumer/index entrypoints. It uses one small real
Nomic vector root and a native-text CPU root; it submits no Gemini jobs.

```text
index worker --original write--> test relay --same bytes, HTTPS--> Turbopuffer
test driver --arm/release----------^
API --read/search, bypassing relay------------------------------> Turbopuffer
shell --SIGKILL/restart----------> index worker process
```

The relay has a fixed real Turbopuffer origin, no database client, no application
imports and no host port. It records only namespace, wire/payload hashes and transport
progress, never credentials or content. Its controls are test-only. It cannot
fabricate a successful provider response: writes reach the real provider.

1. Hold a successful real index response before the worker can receive it.
   Assert that vectors/mutations are durable but publication is unacknowledged.
   Kill the actual Nomic worker, release the orphaned response, restart and wait
   for ordinary lease/SQS recovery. Require a new attempt, identical replayed
   decompressed JSON bytes, unchanged S3 vector/mutation objects, working search
   and no DLQ. Gzip timestamps are recorded separately; compression framing is
   not the mutation identity. The original compressed bytes are forwarded intact.
2. Hold a second-version write before forwarding it; capture a third version
   while it is in flight. Kill/restart the CPU worker and let the third version
   publish. Release the old write afterward and prove through a direct,
   read-only provider query that stale rows physically exist. Public read/search
   must still expose only the third version. Reconciliation is stopped for this
   part so background cleanup cannot make the stale-row assertion vacuous.
3. Hold an update while deleting its root through the public API. Release the
   update after deletion and prove the late provider rows exist while catalog,
   read and search deny access. Start the actual scheduled reconciler and check
   that it removes the physical rows and source prefixes using durable deletion
   artifacts, with tombstones surviving the removed catalog. This scenario
   now passes in sequence with the first two scenarios in one invocation.

No leases, attempt tokens, delivery counts or application database rows are
edited by the driver. This models delayed requests at an intermediate network
service, not a Modal scheduler or AWS durability guarantee. Index recovery logs
use separate artifact names; phase results share the append-only results log
and are distinguished by synthetic run ID. Cleanup retains the isolated project
if external deletion fails. CI/release configuration invokes this as its own
protected-environment matrix job. A complete local invocation using frozen application
images passed all three scenarios and cleanup. The consumer reclaimed work
after its normal lease, reused its S3 artifacts and did not exhaust delivery
retries. Wire gzip timestamps differed while decompressed payload hashes matched.
The source hashes were checked before image reuse; no running application or
driver was edited. This validates runtime behavior, not a fresh build of the
new Dockerfile cache arrangement or execution on GitHub-hosted runners. See the
[implementation ledger](../../docs/ingestion-implementation.md) for run IDs.

## Provider submission and partial-retry suite

`bash scripts/test-e2e-provider-recovery.sh` adds
`compose.e2e-provider-recovery.yml`. A separate relay forwards the SDK's real
Google traffic using its documented `GOOGLE_GEMINI_BASE_URL` setting. It has no
application imports, database access, host ports or configurable upstream host.
It does not edit payloads or invent provider outcomes.

```text
transform / collector --original requests--> provider relay --> real Gemini
test driver --hold/release next submission------^
shell --SIGKILL/restart--> actual transform worker
test driver --delete exact synthetic uploads-----------------> real Gemini
```

1. Hold a real successful batch-create response. Kill the transform process
   before it can record the returned job ID, then restart it and the collector.
   The normal provider listing and SQS lease paths must recover the same job,
   preserve input IDs, publish searchable chunks, and issue no duplicate create.
2. Hold a four-page submission before forwarding. Delete alternate exact
   run-owned page uploads through Google's API, then release the unchanged
   request. Gemini itself must report alternating successful/failed requests.
   Start collection; snapshot successful result objects before the next scheduled
   retry. Only failed pages may acquire new uploads or higher attempt counts.
   Check unchanged successful result references/ETags, final page order and FTS.
3. Require the scheduled production collector to acknowledge upload deletion,
   then confirm that provider reads cannot access the files (403 or 404).
   A 403 alone is not deletion evidence. No direct cleanup-function calls or DB
   updates. For the uploads explicitly removed by the fault, capture their real
   provider expiry timestamps before deletion. Require the collector either to
   acknowledge absence or to preserve the ambiguous 403 with the same future
   expiry deadline, never falsely recording deletion/expiry. Report this pending
   state separately. The short suite does not fast-forward time or prove the
   later 48-hour expiry transition; that remains an elapsed-time verification gate.

These are implemented scenarios, not a claim of a passing run. Consult the
implementation ledger for exact execution results.

## Expanded media suite

`bash scripts/test-e2e-media.sh` uses the same production-role Compose topology
with 27 short container/codec fixtures and one 315-second two-voice WAV fixture.
It includes the original WAV/MP3/MP4 corpus inputs plus M4A/M4B/AAC/FLAC/OGG/OGA/
OPUS/AIF/AIFF/WMA/AMR and MOV/M4V/MKV/WEBM/AVI/MPEG/MPG/WMV/FLV/3GP/MTS/M2TS/MXF.
It captures through the CLI, submits real Gemini Batch jobs, waits for
real index publication, and checks retained originals, text hashes, speaker/
timestamp locations and public FTS. No extractor is called inside the driver.

The driver prints all synthetic transcripts and their exact provider
request mappings before checking expected content. Sanitized output is retained
as `media-results.log`; service logs use `media.log`. The focused reproduction
and the provider's original response established that the synthetic proper
name was transcribed differently in MP3; the pipeline preserved that response.
A subsequent run passed checks of ordinary spoken content, timestamps, hashes,
retained originals and public search. Those earlier three-file runs do not
validate the expanded suite. The five-minute-clip run failed diarization and
returned timestamps shifted by about four minutes after a long silence; an
explicit elapsed-time/speaker prompt alone did not fix it. New v2 extractions
use six minute-bounded requests for this recording. Assertions require speech
within three seconds of its fixture timestamps, no invented speech in silent
clips, distinct request scopes and nonempty speaker labels. Original
v1 extractions retain their five-minute clip boundaries on retry; capture
replays retain their original extraction revision instead of creating new work.
The v2 run indexed all 28 files and corrected the large timestamp shift, but
still failed the two-voice assertion. On 2026-09-05 the user accepted best-effort
Flash-Lite speaker labels, so exact voice separation is now a quality observation,
not a release requirement. Full transcripts and labels remain in the diagnostic
output. Timing/content/silence expectations are unchanged; those earlier runs
remain recorded as failures under their original contract.
The AMR fixture uses Debian's fixture-only
[OpenCORE AMR-NB encoder](https://packages.debian.org/en/stable/libs/libopencore-amrnb0),
called through its C interface with synthetic 8 kHz PCM frames. The production
worker still decodes it through its ordinary FFmpeg path. AMR extraction was
observed in the expanded runs, but a failing suite is not a verified full pass.

Expected words, search queries and location requirements belong to the fixture
data. Validators must not recognize a filename or add exceptions for an observed
provider result. The full corpus verifier streams every extraction and checks
every retained original, without named-file or size-based exclusions. This
latest fixture-data refactor passed a fresh, unchanged-application-image full
corpus run, including all real-provider, restart and malformed-message phases
and cleanup. The 1,024-file capture took 12.04 seconds against local S3/SQS;
main verification took 270.51 seconds. These are run observations, not production
throughput guarantees. Application code has no knowledge of the fixtures,
personal directory paths or test scenarios.

Test-driver files are copied after the vector dependency/model cache layers so
editing an assertion does not reinstall PyTorch or download model weights.

## Explicit gaps from production

- LocalStack is an AWS-compatible emulator, not AWS IAM, regional latency,
  throttling, failure domains or SQS durability. Version 4.14.0 is pinned so a
  newer licensed image cannot silently change local startup requirements.
- Compose replaces ECS and Modal scheduling/autoscaling, not application roles.
  Scheduled adapters run serially and do not emulate Modal's hard invocation
  timeouts or overlapping-input backlog. This does not prove deployed IAM,
  network policies or GPU concurrency.
- The real Nomic model runs CPU float32 here; production defaults to CUDA/half.
  Real-provider vector/hybrid ranking scenarios pass. Performance and exact GPU
  numerics are not proven.
- Worker-secret assertions cover transform, both index endpoints and the
  standalone query endpoint, including malformed/non-string credentials.
  The local adapter serializes calls per container like Modal's default input
  limit; it must not concurrently mutate one Nomic instance's position cache.
- This is **not yet a replacement for every previous assertion**. Provider
  lost-response/partial-retry and long/alternate media scenarios are implemented;
  their results must be tracked separately from earlier suite passes. Path ACL
  denial during capture and packed-file isolation now have passing observations
  in the new retention run. Additional capture-revocation scenarios are included
  below. XLSB/MSG and remaining visual/Office variants have a dedicated suite;
  see its recorded result rather than assuming a pass from coverage alone.
  expanded media remains unverified.
- A build/readiness pass is not an E2E pass. Only successful recorded phases prove
  those scenarios; never reuse retired unit-test counts as current evidence.

### Retention/security and cloud query checks

`bash scripts/test-e2e-retention.sh` builds current images and runs CLI spool
limits, incomplete-capture cleanup, accepted-pack release, 70 successive CLI
captures with 64-receipt pruning, append reuse after
cleanup, retained-source/read/search checks, cross-tenant root/key/completion/
source-reference forgery, pending/conflicted captures under spool pressure, and
concurrent authenticated Nomic queries. It also sets the ordinary embedding-cache
retention policy to 120 seconds and waits for the production 60-second scheduled
reconciler: two real Nomic publications reuse a cache pack, cold-pack expiry
removes its S3 bytes/locators while search and mutation artifacts remain intact,
then another publication re-encodes the cache miss into a new pack. The obsolete
extraction policy is also set to 120 seconds: a replaced version's chunks and
mutations must disappear while current artifacts, all source bytes, append reuse
and public reads remain valid. No database
timestamps or clock hooks are changed. Source retention also uses 120 seconds,
but the suite must wait through the actual 15-minute upload authorization before
expecting deletion. It tests obsolete/unaccepted packs, abandoned multipart
sessions, mixed-pack append reuse, replay after historical GC, and re-upload
from a retained pending spool even when the live file changed. Packed-file
security checks cover sibling-range grafts, capture-ID reuse, another uploader's
unbound bytes and legitimate same-file reuse by a second authorized writer.
Folder-denial races are introduced at the real manifest PUT response boundary.
The `capture-permission-races` phase also holds that response while revoking
user/org/group grants, downgrading a grant, removing group/org membership,
downgrading an editor role, changing a role-dependent folder deny, or deleting
the active API key. It requires HTTP 403 with no catalog/proof/pack binding, then
restores access and requires the same capture to index and read successfully.
Fresh-image run `8edecb6117ea47bdbdb67a3742c7ce22` passed all nine cases in
40.26 seconds and cleanup in 4.17 seconds, exiting 0. The standalone phase used
native text/CPU indexing and real Turbopuffer; it does not prove GPU/media
behavior or revoke already-issued S3 URLs. The retention suite includes these
scenarios. It uses the same production
processes, migrations and real search provider. It does
not yet prove every authorization boundary, populated-schema upgrade or artifact-cleanup
races with paused work/retries. Source-GC/security assertions passed in run
`e71ea8adf31c4eb288f51d4abf858301` (932.49 seconds). Initial root cleanup failed;
the API upgrade through migration 040 made cleanup pass in 4.43 seconds. This is
not a latest-code uninterrupted suite pass. Receipt pruning passed in run
`53d912786fa847d48aa340c5052b0423`; the expanded pending/conflict, embedding-cache
and obsolete-extraction scenarios passed in fresh run
`97ebd39610b74bfea94e876b9189fc1c` (331.52 seconds plus successful cleanup).

`python3 tests/e2e/cloud_query.py` is the separate real-Modal check described in
the deployment guide. It starts/stops a temporary app, not a production rollout.
Compose uses CPU float32; the cloud query role defaults to CUDA/float16 and logs
the actual device/dtype at model initialization. A cloud query pass does not
establish bulk GPU indexing, IAM permissions, autoscaling or production traffic.

### Expanded non-media formats

`bash scripts/test-e2e-formats.sh` generates 51 files: ten Word variants, twelve
presentation variants, ten spreadsheet variants (including actual binary XLSB),
seventeen image variants and Unicode/ANSI Outlook MSG files. Legacy/ODF files
are generated with LibreOffice export filters; OOXML templates/macro-capable
packages retain valid content types, with no active VBA project. Images include
HEIF/HEIC/AVIF, JPEG 2000 and two-frame GIF/APNG/TIFF. The JPX fixture exercises
an ordinary compatible single image, not advanced compositing.

Run `4277353a0ef24336b16385338656678c` passed all 51 variants in 1,189.31
seconds and cleanup in 7.37 seconds, exiting 0 with frozen application images.
The later driver-only change to report each completed file immediately was not
part of that run; its per-file assertions are the same.

Every file goes through CLI capture, the actual renderer/parser, real Gemini
where appropriate, durable chunks/mutations and public FTS. Assertions compare
original hashes, exact spreadsheet cells/addresses, extracted content, page/frame
anchors and public page reads. These are vector-disabled roots, so the suite
does not start Nomic/query pools. The media suite likewise uses only CPU index
publication; the corpus/index-recovery suites cover local Nomic separately.

### Real AWS and bulk GPU

`uv run --with boto3 --with 'psycopg[binary]' --with modal tests/e2e/cloud_index.py`
uses `compose.e2e-cloud.yml`: CLI, API, transform and consumers in Compose; actual
Modal GPU bulk/query deployments; actual cloud Postgres, S3 and SQS; real search.
See [cloud provisioning and cleanup requirements](../../docs/production-deployment.md).
It provisions temporary resources and credentials, rather than inheriting a
production queue/database into the test. The test checks scoped AWS credentials,
130 distinct vectors over several packed GPU batches, mutations and all search
modes, then an append with cached-vector reuse. Public multipart APIs exercise
real upload/resume/completion; root deletion exercises listing/abort/deletion.
This is a separate opt-in cloud run, not an automatic GitHub PR provisioning job.
It cannot prove the deployed ECS task principal, cloud failure domains, physical
deletion in versioned buckets, or production migration/cutover.

Run `eb6c0db4478041579561c8a4eb15ddfc` passed this actual-cloud path in 183.14
seconds and API cleanup in 1.84 seconds, with both GPU roles on CUDA/float16.
Temporary cloud and Compose resources were removed; production was unchanged.
