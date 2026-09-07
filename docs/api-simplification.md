> Active scope: optimize only the critical sync → transformation → index flow.
> The broader API audit below is historical work and is paused.

# API and workflow simplification audit

The original audit covered every API and complete end-to-end workflows, with
multiple servers handling web requests. That broader audit is incomplete and
paused; current work is limited to the critical sync-to-index flow. Deployment roles and handoffs are documented in
[the role diagram/table](architecture-and-functionality.md#per-file-pipeline-deployment-roles).
All changes described here are local, not a production rollout.

## Requirements and evidence

| Requirement | Current evidence | Remaining work |
| --- | --- | --- |
| Reduce DB/network operations for every API | API-key resolution now joins user email; root metadata/grants load in one statement; effective ignore policies load in one statement. Route inventory below. | Trace and measure remaining route families, including auth/login, admin provisioning, capture, remaining search work and billing. |
| Preserve horizontal scalability | SQS/ownership protocol remains durable; worker scale-out and crash/restart tests passed in the worker report. New API E2E uses two real API processes sharing services. | Extend concurrency coverage beyond the verified membership mutations and in-flight read ACL changes. |
| Minimize code, mutable state and computation | No shared authorization cache introduced; repeated SQL round trips removed while keeping the shared permission policy. | Inspect remaining duplicate helpers, transient state and replay/control flows. LOC is secondary to fewer operations and correct behavior. |
| Verify complete E2E flows | [Worker verification](worker-throughput.md) covers indexing, migration, replay, expiry, search/read. | Expand route-specific multi-server E2E; do not infer broad completion from individual passes. |

## Current read-path change

API-key authentication previously fetched the key/membership, then user email.
It now uses one join and one database snapshot. Root listing previously loaded
roots and queried grants once per root; explicit multi-root search loaded each
root and its grants separately. `accessibleRoots` loads the requested root
metadata and matching grants in one statement, then applies the same permission
function used by locked capture commits. Request state is discarded afterward.

| Operation | Before (SQL statements) | Current (SQL statements) |
| --- | ---: | ---: |
| API-key identity resolution | 2 | 1 |
| List N roots, including key resolution | N + 3 | 2 |
| Get one root, including key resolution | 4 | 2 |
| Authorize N selected query roots, including key resolution | 2N + 2 | 2 |
| Effective ignore policy, including key resolution | 4 | 2 |

Search still has namespace, ACL, candidate-publication and provider calls after
root selection; the table does not count those as removed. Root/access metadata
is read in a single statement snapshot on whichever API handles the request.
Capture authorization still rechecks and locks current membership, key, group
membership and grants at commit after external IO. No cache invalidation scheme,
server affinity, or additional persisted state is needed.

`bash scripts/test-e2e-api-access.sh` runs two production API containers, actual
CLI/transform/CPU index/consumer/reconciler processes, Postgres and LocalStack,
and real Turbopuffer. PostgreSQL's `pg_stat_statements` extension is installed
only by disposable test infrastructure, for read-only query-count assertions.
The driver addresses both API IPs explicitly, verifies root/grant/role/key changes,
reads/searches actual CLI captures, then restarts both servers and checks again.
This is native-text/no-vector coverage, not a new GPU or media validation.

Two-API run `21d2f910b33c4fe580bc019006ebac4a` passed root/access and
identity query-count assertions, policy reads/updates, role/grant/key revocation,
CLI capture through real read/FTS, both API processes restarting, and cleanup.
Capture commit-race run `34b7b3baa4aa4856b5b6790250246c8f` passed all nine
existing revocation/denial scenarios in 37.87 seconds and cleanup in 4.40 seconds.
Neither run is evidence of browser JWT freshness or simultaneous owner mutations.

Final read-path run `7b5d5e530739481ca0f2e96acbb233b0` passed the expanded
three-distinct-root selected/all-root search checks, constant identity/access
statement counts, both servers restarting and cleanup. The final application
change afterward removed only unreachable `UpsertUser`, legacy root-creation/
name-lookup wrappers and unused role middleware; repository-wide call searches
and `go build ./...` passed. Compared with the preceding checkout, the Go
API/auth diff has 74 added and 149 removed lines (net 75 fewer). The extension
observes selected statement families; search's other database/provider calls
remain explicit work in this audit.

## Membership mutation and browser authorization

POST/PUT/DELETE membership routes now share one handler and one database call.
Migration 042 supplies `change_org_member`, a normal invoker-security PostgreSQL
function. It locks the organization row, rechecks the current caller membership
and API-key scope, reads the target under a row lock, and returns its profile and
resulting role. Changes for different organizations remain independent.

| Mutation body (authentication excluded) | Previous client DB calls | Current client DB calls |
| --- | ---: | ---: |
| Add/upsert member | 1 | 1 |
| Change role | 3–4 | 1 |
| Remove member | 2–3 | 1 |
| Platform-admin upsert | 4 | 1 |

These are network round trips, not a claim that the function contains one
internal SQL statement. The function keeps necessary lock/authentication reads
inside Postgres. Existing-member profiles come from the locked member read;
unchanged roles do not issue a data update. No persisted owner counter exists:
only another currently locked owner can change an owner's membership, and
self-demotion/deletion is forbidden. That caller remains an owner. POST now
applies the same target-role restrictions as PUT/DELETE, closing its prior
role-overwrite bypass. Idempotent self-add with the same role remains allowed.

The function is explicitly VOLATILE, so its queries use fresh snapshots after
waiting under normal READ COMMITTED isolation. This follows PostgreSQL's
[documented function snapshot semantics](https://www.postgresql.org/docs/17/xfunc-volatility.html).
Platform-admin provisioning keeps its separate repair authority (including
ownerless organizations) while using the same lock. Deleting an entire user or
organization is a separate platform-admin workflow, not an ordinary member
mutation. Apply migration 042 before updating API instances and drain old API
writers: old binaries do not follow this serialized mutation protocol.

Browser JWTs still carry their signed user/org identity and existing token
format, but current role/email come from one membership query per request.
This adds one read compared with trusting an obsolete role until token expiry;
it uses no per-server authorization cache or shared session-revocation table.
Member removal rejects the session on the next request. Logout still clears the
browser cookie; it does not individually revoke copied signed tokens.

Two-server run `d6ac1d9fb5d242409a6d2e66fa5efd77` passed 18 simultaneous owner
mutations (six each through POST/PUT/DELETE), exactly one surviving owner,
unchanged-row retry checks, profile responses, ordinary privilege parity and
server restarts. Capture-race run `51adb69091ce433e8481b2ab967a8ba9` passed all
nine existing scenarios in 36.19 seconds plus cleanup in 4.29 seconds. That
capture run predates removal of a redundant internal owner check; its target
membership operations do not exercise that removed branch.

Final combined run `d1a35d382b9d490ca21be8f15e188745` passed the consolidated
locked-profile read, all 18 owner races, no-op row-version checks, returned
profiles, current-role browser cookies, membership removal, invalid/expired
cookies, exact reads, and both API restarts. API cleanup passed in 1.10 seconds.
Cookie tests used synthetic signed inputs, not provider-issued sessions; the
full email/OAuth and invite-acceptance flows remain a separate validation gate.
No production schema, endpoint or worker configuration was changed.

## File read snapshots and result access

Reads load the file's published extraction and active namespace directory in
one statement, then retain that routing/publication snapshot across provider
pagination and the empty-range metadata fallback. No database connection stays
checked out during provider IO. Root creation initializes namespace rows in its
transaction; migration 012 backfills legacy roots. Namespace listing no longer
tries to allocate or repair state during a read.

The post-provider deny-prefix and personal-file proof lookups share one query.
Only distinct candidate paths are sent for proofs; ordinary organization roots
and admins send no proof paths. Access errors fail closed. The pre-read ACL
check and post-provider access check remain separate because a deny can commit
while a provider response is in flight. Search retains bounded candidate
publication validation; this change does not remove that correctness check.

| File read body (identity/root authorization excluded) | Previous client DB calls | Current client DB calls |
| --- | ---: | ---: |
| Nonempty organization/admin read | 4 | 3 |
| Nonempty personal read requiring a proof | 5 | 3 |
| Empty range, accessible metadata, organization/admin | 7 | 3 |
| Empty range, accessible metadata, personal proof required | 8 | 3 |

The before counts follow the previous call graph; current statement families
are asserted in E2E. These counts describe one unchanged published file. Provider pagination can
make multiple HTTP requests, but does not repeat routing, publication or proof
queries. Missing/denied files may exit earlier. A private result removed by its
post-provider access check can still enter the existing metadata fallback;
that fallback gets its own post-provider access check.

The two-API E2E suite now includes an external relay that forwards real
Turbopuffer responses and can hold one at the network boundary. Workers still
index directly into real Turbopuffer. Synthetic personal-root fixtures cover
multi-page Unicode reads, proofs, replacement publications and ACL changes from
the second server while a read/search response is held. The relay records only
transport metadata and publication identities, never credentials or content.
Run `a1e3eb614d2d4ada87b8cc85c4ed73a1` passed the new read/proof/network-race
scenarios, existing role/grant/key tests, all 18 membership races, cookie checks,
both API restarts and cleanup (1.92 seconds). The initial run's ACL fixture used
a file path where the API requires a folder prefix; it was corrected to a real
folder, with no production special case. All resources from both runs were removed.

Final run `a93002325ce246fb9f7fef544d15846a` passed the metadata-only fallback:
normal and large out-of-range reads transferred zero chunk-content bytes while
retaining line metadata and proof validation. The large fixture is a 4,160,001-byte
UTF-8 file containing one line, read across more than one 512-row provider page.
The run also counted the pre-provider ACL statement, repeated the network races,
all membership/cookie checks and both API restarts. Cleanup passed in 2.08 seconds;
all isolated containers, volumes and provider namespaces were removed.
`go build ./...`, Python compilation, shell syntax and `git diff --check` passed.
No deployment was performed. Page-based media extraction, GPU throughput and
Postgres failure recovery were not rerun by this native-text read scenario.

## Group provisioning and membership

Group creation, listing, member listing and member addition now each use one
client database call. Migration 043 adds invoker-security functions for the two
mutations. They use shared parent locks and group/member row locks; unrelated
groups and organizations remain independently writable across API instances.
Uniqueness constraints settle concurrent inserts, and a fresh statement snapshot
reads the winner when a request loses an insert race. There is no process-local
mutex, cache, or new persisted coordination table.

| Platform-admin group request | Previous client DB calls | Current client DB calls |
| --- | ---: | ---: |
| Create/upsert, without external identity | 4 | 1 |
| Create/upsert, with external identity | 5 | 1 |
| List groups or group members | 2 | 1 |
| Add group member | 3 | 1 |
| Delete group member | 1 | 1 |

These are network round trips, including the former BEGIN/COMMIT calls, not a
claim of one internal SQL statement. The mutation functions retain necessary
parent/membership checks and row locks. Retries of unchanged groups/members
preserve their row versions and timestamps instead of issuing a no-op UPDATE;
row locking still has PostgreSQL locking/WAL costs. Group data remains relational
because authorization depends on current membership.

External group IDs take precedence over an optional supplied group ID, matching
the existing API contract. Concurrent requests for that same identity converge
on one group. A name collision between different identities returns HTTP 409.
An existing group ID from another organization is also rejected with 409; the
old ID-conflict update could mutate that other organization's group. Listing
queries distinguish a missing parent (404) from an empty collection (the existing
JSON null response) in one statement.

The expanded two-API suite exercises concurrent ID/external-ID creates, no-op
retries, changed profiles, unique-name conflicts, tenant boundaries, PUT/DELETE
races, organization deletion, and state after both API restarts.

Run `4027d3afb9bf45cd81e0ac5abeb5d6c8` passed all group scenarios, including
24 concurrent creates (shared ID, shared external ID, and conflicting names),
eight concurrent member additions, six PUT/DELETE races, tenant isolation,
organization deletion and both API restarts. The existing read/proof/network-race,
18 owner-mutation and browser-cookie scenarios also passed. Cleanup completed in
1.82 seconds; all isolated containers, volumes and provider resources were removed.
The initial run stopped at an incorrect 403 test expectation for ordinary keys;
admin middleware correctly returned 401. Its cleanup also passed. No production
authentication behavior was changed to accommodate that assertion.

Apply migration 043 before rolling out these API binaries. Older API writers do
not follow the new group mutation protocol. No production deployment occurred.

## API-key creation and index maintenance

Key creation now uses `INSERT ... SELECT` to check membership and, for an
API-key-authenticated caller, the credential's current existence, expiry and
creation scope in the insert's statement snapshot. These checks happen after
the request body arrives. Platform provisioning no longer fetches a member
profile before insertion: its body drops from two database calls to one.
Ordinary creation remains one body call plus one authentication call, with the
fresh authorization check included in that existing insert. Foreign-key failures
from concurrent user/org deletion produce the same missing-authorization outcome
as a missing membership, without returning a raw key.

This is statement-snapshot authorization, not a promise that an overlapping
revocation cancels a creation that already observed a valid credential. There is
no external IO inside the insert. No additional row-lock protocol or transaction
round trips are required. Revocation committed while the body was pending is
observed by the later insert. The API continues to issue independent keys; deleting
a parent key does not transitively revoke keys it previously created.

Listing and deletion already use one database call each after authentication.
Listing collects typed rows with stream errors propagated and preserves its empty
JSON null response. It exposes metadata only. The list is scoped to the caller's
user/org; the existing deletion policy uses the caller's organization and key ID.
Scope aliases and the explicit non-empty scope requirement are unchanged. Email
and OAuth CLI issuance call the same insert after verified login; complete provider
login/issuance remains unverified in this pass.

Catalog inspection of the actual E2E Postgres found two exact non-unique duplicates
of unique indexes. Migration 044 drops `idx_api_keys_hash` and
`idx_root_index_namespaces_root`, retaining their unique constraint indexes with
identical columns, ordering, collations and operator classes. Each key or namespace
insertion now maintains one fewer index. The migration removes index state and
write work without changing the uniqueness rules or requiring a new cache.

Run `1256d6ea2d444a838a57b7cfb4d5a68e` passed the final snapshot-based insert,
empty-list behavior, alias/metadata/hash checks, eight simultaneous key creations
racing user deletion, and deterministic key/membership revocation during HTTP body
arrival. The external client waited for the real API's `100 Continue` response,
then changed authorization on the second API before sending the body. This covered
API-key, signed-session and platform-admin creation. All existing group, read,
membership and restart scenarios also passed; cleanup took 1.97 seconds.
Final combined run `2dd6f471f36c4bf4a153ad836a25e780` passed migration 044,
retained-unique-index assertions, key creation/revocation and user-deletion races,
the existing group/read/membership/browser scenarios, and both API restarts.
Cleanup completed in 1.81 seconds; all isolated containers, volumes and provider
resources were removed. `go build ./...`, Python compilation, shell syntax and
`git diff --check` passed. No production deployment was performed.

## Batched search routing and empty roots

Search loads active namespace routing for all selected roots in one database
query. The query checks catalog existence through the existing root index; a root
with no captured catalog rows needs no provider query. No publication counter,
cache, index or additional write is introduced. Routing arrays are already ordered
and filtered in SQL, so the search helpers no longer copy/filter/sort them again.
The same result/analytics path handles an empty root selection.

An uncaptured vector-enabled root also needs no query embedding. Explicit vector
search still rejects a selected root configured with vector search disabled.
`roots_searched` retains the count of selected logical roots, including empty ones;
namespace/provider counts reflect only the actual routing targets.

| Search routing overhead | Previous code | Current code |
| --- | ---: | ---: |
| Namespace/routing DB calls for N selected roots | N | 1 (0 for no roots) |
| Provider requests in the 13-root, 2-shard FTS fixture | 26 | 6 |
| Provider requests for one uncaptured root | 2 | 0 |

The previous counts follow the old per-root/per-shard call graph. Current counts
are asserted using `pg_stat_statements` and the external real-provider relay. The
13-root fixture has three populated roots and ten uncaptured roots. A later first
capture becomes searchable on both API processes without cache invalidation.
Candidate publication and post-provider ACL/proof validation still perform their
own bounded queries; this table does not count them as removed. Catalogs containing
pending files or deletion tombstones still use that validation, avoiding a full
catalog scan or another maintained index just to prove there are no publications.

The routing-only change retained a 30-second DB deadline and a separate
30-second provider deadline per populated root. The batched search rounds below
replace those per-root deadlines with one deadline for the complete search phase.
A shared query-error handler preserves 404/503/504 responses and retry metadata.

Final local run `e103c0f477634361aafabafe7564d042` passed exact routing/provider
counts across both API servers, FTS/hybrid selection and globs, empty vector roots,
first capture, authorization, provider timeout/recovery, and both server restarts.
The expanded key/group/read/member/cookie scenarios also passed; cleanup took
4.60 seconds. Initial routing run `a4bb5b3d40d94a6b87f6f5536c66f88e` passed before
the common timeout/error handling cleanup.

Cloud run `9cd00dc5ce5345cfaa217a4aae4c2dc3` passed actual AWS S3/SQS, Modal GPU
publication/query embeddings, and FTS/vector/hybrid search through single-root,
selected-root and all-root requests including an uncaptured root. It also verified
source/read, append reuse of 130 vectors, and multipart cleanup. The scenario took
238.00 seconds, with cleanup in 2.63 seconds; this is E2E elapsed time, not worker
throughput. This build preceded the timeout/error helper cleanup, which was then
verified in the local run. All test resources were removed; production deployment
and configuration were unchanged.

## Vector distances across roots and shards

Vector search now keeps the provider's distance through both shard and root
merges, sorting nearest first. Previously, a multi-shard root replaced distances
with reciprocal rank scores, while a single-shard root returned raw distances;
the cross-root merge then always sorted descending. Adding an uncaptured root to
a single-shard search could therefore reverse the populated root's results.
The provider's [query contract](https://turbopuffer.com/docs/query) defines ANN
`$dist` as distance and BM25 `$dist` as relevance; these require different ordering.

The vector shard merge concatenates candidate arrays and sorts distances. It
removes the two fusion maps and intermediate scored-record array from this path,
and introduces no SQL, provider calls, writes or shared state. Public vector
`score` now consistently means distance regardless of shard count. FTS and hybrid
fusion retain their existing behavior and still need the broader ranking audit.
Publication validation and post-provider ACL/proof checks remain in place.

The cloud regression compares API results with actual provider distances after
CLI capture and GPU publication. It checks single/selected/all-root searches,
uncaptured roots, both selection orders, every configured shard, and top-5 versus
all candidates. The old two-shard build failed the nearest-first assertion in run
`52d29dbfc4d3431182e0e2c973296017` (project `pufferfs-cloud-c34545519b82`);
cleanup passed in 7.65 seconds and all temporary resources were removed.
The corrected two-shard build passed in run `128d0eb97ccd4ccb9c8fba197f0b5213`
(project `pufferfs-cloud-47b7f5ab1532`): 298.34 seconds for the full cloud scenario,
4.66 seconds for API cleanup. This included real AWS/Modal publication, all search
modes, source/read, append cache reuse and multipart cleanup. All temporary
resources were removed. The single-shard build also passed in run
`568450d4e714443ab146c6f81147917d` (project `pufferfs-cloud-282a4a349457`):
177.93 seconds for the scenario and 4.33 seconds for API cleanup, followed by
removal of all temporary resources. Both configurations ran with one transform
worker and a maximum of one bulk GPU worker. These are whole-scenario durations,
including GPU startup, not a comparison of search latency. `go build ./...`,
Python compilation and `git diff --check` passed. Changes remain local.

## Batched publication and access reads

After routing and query embedding, the API now queries selected namespaces in
rounds, using at most 16 concurrent provider calls for the request. Each round
loads newly seen candidate publications in one SQL statement across roots and
shards. `jsonb_to_recordset` supplies the root/path groups; the existing catalog
index limits lookups to those paths. Outer joins distinguish missing roots from
uncataloged files without another existence read.

Each namespace keeps its first observed file publications and rejected extraction
IDs within the request. A namespace whose candidates all validate retains its
ranked lists; only namespaces with rejected candidates fetch another round.
Unchanged file paths need no second publication lookup. Hybrid lists are both
validated before fusion, and vector distances retain nearest-first ordering.
The existing limits of 16 rounds, 8,192 observed paths, 4,096 exclusions and a
1 MiB exclusion filter remain per namespace. No DB connection is held during
provider IO, and no shared cache or additional writes are introduced.

After all publications validate, one SQL statement loads folder denies and
required content proofs for all roots with result rows. Paths and proofs remain
root-scoped, including identical relative paths in different roots. File reads
reuse that same access code with one root. Access is checked after the last
provider response so a folder deny added during that IO takes effect.

| Search-body SQL reads, two roots with four populated shards | Before this change | Current |
| --- | ---: | ---: |
| Routing | 1 | 1 |
| Candidate publications, no stale rows | 4 | 1 |
| Folder denies/content proofs | 2 | 1 |
| Total, excluding identity and root selection | 7 | 3 |

Previous counts follow the old per-namespace/per-root call graph; current counts
are asserted around actual API requests using `pg_stat_statements`.

Empty candidate sets need neither a publication nor an access query. Stale
candidates may require further rounds and publication reads for newly discovered
paths. A held, unacknowledged write in the fixture required five provider queries:
four initially and one retry of the affected namespace, with one publication SQL
read total because the retry found only previously observed paths.

The complete provider/validation/access phase now has one 30-second deadline,
after embedding; routing keeps its separate DB deadline. This prevents request
time from growing by a full timeout for every selected root. The API retains
per-namespace candidate sets until validation and merging finish; large selections
still need memory/latency measurements. Provider concurrency is bounded by code;
the current fixture proves overlapping roots, not saturation at the 16-call cap.

Two-shard cloud run `2f5fe74147364623aee0e1268eb15181` passed actual AWS S3/SQS,
Modal GPU publication and query embeddings, FTS/vector/hybrid search, nearest-first
global top-k, source/read, append cache reuse and multipart cleanup. The scenario
took 177.67 seconds and API cleanup took 4.50 seconds. All temporary resources were
removed. These are E2E durations, not a search-latency benchmark.

Local run `f06d91da99d943c293222da708a354b6` passed publication/access SQL counts,
stale-shard retries, folder denies changed during search, root deletion during
provider IO, overlapping roots, timeout/recovery, missing/current/stale proofs
for identical paths in different roots, and both API restarts. The existing
key/group/read/member/session scenarios also passed. Cleanup took 6.51 seconds;
all temporary containers and volumes were removed. This run uses native text,
real Turbopuffer, Postgres and LocalStack, with separate production processes.

Initial runs exposed two fixture errors: ACLs normalize to folder prefixes, and
the preceding authorization test deliberately revoked its reader key. The tests
now use an actual `records/` directory and a fresh scoped reader credential. Both
failed runs cleaned up; production path and credential handling were unchanged.
Single-shard cloud run `1856d424ea4e42fdac168e895100b60e` also passed the complete
cloud scenario in 178.18 seconds, with API cleanup in 4.27 seconds and all
temporary resources removed. Both cloud configurations used one transform
worker and at most one bulk GPU worker. `go build ./...`, Python compilation
and `git diff --check` passed. These changes remain local.

## Atomic root creation

Public and platform-admin root creation now insert the root and its complete
namespace directory in one statement. A data-modifying CTE passes the inserted
root through `RETURNING` to one `INSERT ... unnest(...)` for all namespaces.
PostgreSQL executes these writes together, so another API sees either no root
or its full directory. There is no client-managed transaction, per-shard insert
loop, or metadata reload. This uses PostgreSQL's documented
[data-modifying CTE behavior](https://www.postgresql.org/docs/16/queries-with.html#QUERIES-WITH-MODIFYING).

The statement checks the organization, current actor membership/role, current API
key/scopes/expiry, and any different target owner's membership. A personal root
owned by its creator reuses the actor's membership result. The public handler
still rejects invalid scope/role combinations early; the statement also rejects
revocation or demotion committed while the HTTP body was arriving. Platform-admin
provisioning uses the same organization/owner checks without an end-user actor.

These are statement-snapshot checks, with normal foreign-key locking. A concurrent
revocation does not retract creation already authorized by that snapshot, and no
process-local authorization cache or new coordination lock is introduced. Missing
organizations return 404, invalid owners 400, and newly unauthorized actors 403.
Pure body validation now precedes the platform route's organization lookup, so a
request with both an invalid body and a missing org returns the body error first.

| Client DB round trips for creation, excluding authentication | Previous | Current |
| --- | ---: | ---: |
| Public org/restricted root, N shards | N + 3 | 1 |
| Public personal root, N shards | N + 4 | 1 |
| Platform org/restricted root, N shards | N + 4 | 1 |
| Platform personal root, N shards | N + 5 | 1 |

Previous counts include `BEGIN`, the root insert, N namespace inserts, `COMMIT`,
and applicable org/member preloads. The durable row count remains one root plus
N routing entries. Namespace names preserve the existing format; their org/root
hashes are computed once per root instead of once per shard. Namespace IDs remain
random UUIDv4 strings, generated by PostgreSQL's built-in
[`gen_random_uuid`](https://www.postgresql.org/docs/16/functions-uuid.html).
The existing 1–256 shard configuration bounds are unchanged. No migration is needed.

Local run `34796e1aa6cd4b0587a3b9c774ce8caf` passed two API processes with two
shards, one creation statement per request, role/scope parity, signed sessions,
body-arrival revocation/demotion/member removal, organization deletion, twelve
simultaneous creations, complete directories, and CLI capture/read/search before
and after both API restarts. The existing search/key/group/read/member/session
scenarios also passed. API cleanup took 7.32 seconds; all temporary resources were
removed. Initial verification caught a fixture expectation for an omitted
`owner_user_id` field; that assertion was corrected and that run also cleaned up.

Single-shard cloud run `df160af2a1fa4047b88cd133be901b3e` passed the complete
AWS/Modal GPU scenario through newly created roots, including all search modes,
global vector ranking, source/read, append cache reuse and multipart cleanup.
The scenario took 117.18 seconds and API cleanup 4.74 seconds; all temporary cloud
and Compose resources were removed. These are full E2E durations, not a root-create
latency benchmark. Counts before the change follow the old call graph; current
counts are asserted with `pg_stat_statements`. The 256-shard configuration has not
been exercised in this pass. `go build ./...`, Python compilation and
`git diff --check` passed. Changes remain local.

User deletion and deletion racing with capture/artifact creation remain part of
the broader workflow audit; the new org-deletion race uses uncaptured roots.

## Remaining findings to resolve

- Full email/OAuth issuance and invite acceptance remain unverified in this pass.
  Cookie fixtures exercise real signature validation and current membership,
  not a real identity-provider login. Invite acceptance now preserves existing
  membership instead of overwriting its role; the full login workflow still
  needs an end-to-end verification.
- Login provisioning has multiple user/identity/invite/org steps. Inspect
  concurrent callbacks and duplicate identity/workspace creation.
- Search routing, candidate-publication and access reads are now batched.
  Audit combining publication/access reads on the common no-retry path, FTS/hybrid
  cross-root ranking, top-k filtering and large-selection memory/latency while
  preserving post-provider authorization and bounded publication validation.
- User deletion explicitly deletes several child tables already covered by
  foreign-key cascades, and enumerates owned roots before final deletion. Audit
  that complete workflow against concurrent root/member/key creation.
- Query plans and large-catalog costs remain unmeasured for group identity
  lookups and per-user key listing; the current tests establish call counts,
  concurrency and public behavior, not a large-catalog performance benchmark.
- Several admin mutations check existence, mutate and reload. Replace redundant
  round trips with database constraints/returning rows where public errors and
  concurrency invariants remain explicit.
- Review source init/complete, multipart and capture retry/authorization flows
  as complete workflows; fewer SQL calls must not drop durable recovery or
  expose signed-upload revocation races.

## Route inventory

The entries below are a scope checklist, not a claim of optimization or verified
completion. Shared API-key improvements apply to authenticated routes; most
route bodies still need their own audit. OAuth and logout are registered by the
outer server router and are included explicitly.

| Route | Source | Audit status |
| --- | --- | --- |
| `GET /healthz` | [internal/server/handlers.go:107](../internal/server/handlers.go#L107) | Pending route/body audit |
| `GET /readyz` | [internal/server/handlers.go:108](../internal/server/handlers.go#L108) | Pending route/body audit |
| `GET /health` | [internal/server/handlers.go:109](../internal/server/handlers.go#L109) | Pending route/body audit |
| `GET /cli/version` | [internal/server/handlers.go:110](../internal/server/handlers.go#L110) | Pending route/body audit |
| `GET /auth/providers` | [internal/server/handlers.go:113](../internal/server/handlers.go#L113) | Pending route/body audit |
| `POST /auth/email/start` | [internal/server/handlers.go:114](../internal/server/handlers.go#L114) | Pending route/body audit |
| `POST /auth/email/resend` | [internal/server/handlers.go:115](../internal/server/handlers.go#L115) | Pending route/body audit |
| `POST /auth/email/verify` | [internal/server/handlers.go:116](../internal/server/handlers.go#L116) | Pending route/body audit |
| `GET /auth/me` | [internal/server/handlers.go:117](../internal/server/handlers.go#L117) | Pending route/body audit |
| `POST /auth/api-keys` | [internal/server/handlers.go:118](../internal/server/handlers.go#L118) | One insert with snapshot authorization; two-API body-arrival races passed |
| `GET /auth/api-keys` | [internal/server/handlers.go:119](../internal/server/handlers.go#L119) | Existing one-call list; metadata and empty-result E2E passed |
| `DELETE /auth/api-keys/{id}` | [internal/server/handlers.go:120](../internal/server/handlers.go#L120) | Existing one-call revoke; two-API scope/revocation/restart E2E passed |
| `GET /org` | [internal/server/handlers.go:123](../internal/server/handlers.go#L123) | Pending route/body audit |
| `GET /org/members` | [internal/server/handlers.go:124](../internal/server/handlers.go#L124) | Pending route/body audit |
| `POST /org/members` | [internal/server/handlers.go:125](../internal/server/handlers.go#L125) | One-call mutation; two-server concurrency verified |
| `PUT /org/members/{userId}` | [internal/server/handlers.go:126](../internal/server/handlers.go#L126) | One-call mutation; two-server concurrency verified |
| `DELETE /org/members/{userId}` | [internal/server/handlers.go:127](../internal/server/handlers.go#L127) | One-call mutation; two-server concurrency verified |
| `GET /org/invites` | [internal/server/handlers.go:128](../internal/server/handlers.go#L128) | Pending route/body audit |
| `POST /org/invites` | [internal/server/handlers.go:129](../internal/server/handlers.go#L129) | Pending route/body audit |
| `DELETE /org/invites/{id}` | [internal/server/handlers.go:130](../internal/server/handlers.go#L130) | Pending route/body audit |
| `GET /ignore-policy` | [internal/server/handlers.go:131](../internal/server/handlers.go#L131) | Read-path refactor; two-API E2E passed |
| `GET /ignore-policy/user` | [internal/server/handlers.go:132](../internal/server/handlers.go#L132) | Pending route/body audit |
| `PUT /ignore-policy/user` | [internal/server/handlers.go:133](../internal/server/handlers.go#L133) | Pending route/body audit |
| `GET /ignore-policy/org` | [internal/server/handlers.go:134](../internal/server/handlers.go#L134) | Pending route/body audit |
| `PUT /ignore-policy/org` | [internal/server/handlers.go:135](../internal/server/handlers.go#L135) | Pending route/body audit |
| `POST /admin/orgs` | [internal/server/handlers.go:138](../internal/server/handlers.go#L138) | Pending route/body audit |
| `POST /admin/users` | [internal/server/handlers.go:139](../internal/server/handlers.go#L139) | Pending route/body audit |
| `PUT /admin/orgs/{orgId}/members/{userId}` | [internal/server/handlers.go:140](../internal/server/handlers.go#L140) | One-call mutation; two-server concurrency verified |
| `POST /admin/orgs/{orgId}/groups` | [internal/server/handlers.go:141](../internal/server/handlers.go#L141) | One client DB call; two-API concurrency/retry E2E passed |
| `GET /admin/orgs/{orgId}/groups` | [internal/server/handlers.go:142](../internal/server/handlers.go#L142) | One client DB call; two-API concurrency/retry E2E passed |
| `GET /admin/orgs/{orgId}/groups/{groupId}/members` | [internal/server/handlers.go:143](../internal/server/handlers.go#L143) | One client DB call; two-API concurrency/retry E2E passed |
| `PUT /admin/orgs/{orgId}/groups/{groupId}/members/{userId}` | [internal/server/handlers.go:144](../internal/server/handlers.go#L144) | One client DB call; two-API concurrency/retry E2E passed |
| `DELETE /admin/orgs/{orgId}/groups/{groupId}/members/{userId}` | [internal/server/handlers.go:145](../internal/server/handlers.go#L145) | Existing one-call delete; two-API PUT/DELETE races passed |
| `POST /admin/orgs/{orgId}/users/{userId}/api-keys` | [internal/server/handlers.go:146](../internal/server/handlers.go#L146) | One insert including membership check; two-API E2E passed |
| `POST /admin/orgs/{orgId}/roots` | [internal/server/handlers.go:147](../internal/server/handlers.go#L147) | One atomic root/directory statement; owner/org checks, two-API races, restart and real CLI/GPU E2E passed |
| `POST /admin/orgs/{orgId}/roots/{rootId}/grants` | [internal/server/handlers.go:148](../internal/server/handlers.go#L148) | Pending route/body audit |
| `GET /admin/orgs/{orgId}/roots/{rootId}/grants` | [internal/server/handlers.go:149](../internal/server/handlers.go#L149) | Pending route/body audit |
| `DELETE /admin/orgs/{orgId}/roots/{rootId}/grants/{grantId}` | [internal/server/handlers.go:150](../internal/server/handlers.go#L150) | Pending route/body audit |
| `DELETE /admin/roots/{id}` | [internal/server/handlers.go:151](../internal/server/handlers.go#L151) | Pending route/body audit |
| `DELETE /admin/orgs/{id}` | [internal/server/handlers.go:152](../internal/server/handlers.go#L152) | Pending route/body audit |
| `DELETE /admin/users/{id}` | [internal/server/handlers.go:153](../internal/server/handlers.go#L153) | Pending route/body audit |
| `POST /roots` | [internal/server/handlers.go:156](../internal/server/handlers.go#L156) | One atomic statement with current actor/key/role checks; two-API body-arrival races and CLI/GPU E2E passed |
| `GET /roots` | [internal/server/handlers.go:157](../internal/server/handlers.go#L157) | Read-path refactor; two-API E2E passed |
| `GET /roots/{id}` | [internal/server/handlers.go:158](../internal/server/handlers.go#L158) | Read-path refactor; two-API E2E passed |
| `DELETE /roots/{id}` | [internal/server/handlers.go:159](../internal/server/handlers.go#L159) | Pending route/body audit |
| `POST /roots/{id}/sources/init` | [internal/server/handlers.go:160](../internal/server/handlers.go#L160) | Pending route/body audit |
| `POST /roots/{id}/sources/complete` | [internal/server/handlers.go:161](../internal/server/handlers.go#L161) | Pending route/body audit |
| `POST /roots/{id}/sources/multipart/init` | [internal/server/handlers.go:162](../internal/server/handlers.go#L162) | Pending route/body audit |
| `POST /roots/{id}/sources/multipart/part` | [internal/server/handlers.go:163](../internal/server/handlers.go#L163) | Pending route/body audit |
| `POST /roots/{id}/sources/multipart/complete` | [internal/server/handlers.go:164](../internal/server/handlers.go#L164) | Pending route/body audit |
| `POST /roots/{id}/versions` | [internal/server/handlers.go:165](../internal/server/handlers.go#L165) | Batched metadata/source validation and exact work handoff; two-API races, provenance, expiry, restart and real cloud E2E passed; S3 manifest batching pending |
| `GET /roots/{id}/captured-files` | [internal/server/handlers.go:166](../internal/server/handlers.go#L166) | Pending route/body audit |
| `POST /roots/{id}/captured-proofs` | [internal/server/handlers.go:167](../internal/server/handlers.go#L167) | Pending route/body audit |
| `GET /roots/{id}/state` | Removed | Retired generation inventory; use captured-files |
| `POST /roots/{id}/read` | [internal/server/handlers.go:169](../internal/server/handlers.go#L169) | Pinned routing/publication; combined ACL/proof checks; two-API E2E passed |
| `POST /roots/{id}/acls` | [internal/server/handlers.go:172](../internal/server/handlers.go#L172) | Pending route/body audit |
| `GET /roots/{id}/acls` | [internal/server/handlers.go:173](../internal/server/handlers.go#L173) | Pending route/body audit |
| `DELETE /roots/{id}/acls/{aclId}` | [internal/server/handlers.go:174](../internal/server/handlers.go#L174) | Pending route/body audit |
| `POST /query` | [internal/server/handlers.go:177](../internal/server/handlers.go#L177) | Root/routing/publication/access reads batched; two-API retry/deletion/ACL/proof and real GPU ranking E2E passed; common-path read fusion, FTS/hybrid ranking and large-selection audit pending |
| `GET /billing` | [internal/server/handlers.go:181](../internal/server/handlers.go#L181) | Pending route/body audit |
| `POST /billing/checkout-session` | [internal/server/handlers.go:182](../internal/server/handlers.go#L182) | Pending route/body audit |
| `POST /billing/webhook` | [internal/server/handlers.go:183](../internal/server/handlers.go#L183) | Pending route/body audit |
| `POST /auth/logout` | [cmd/server/main.go:122](../cmd/server/main.go#L122) | Pending route/body audit |
| `GET /auth/google` | [cmd/server/main.go:163](../cmd/server/main.go#L163) | Pending route/body audit |
| `GET /auth/callback` | [cmd/server/main.go:164](../cmd/server/main.go#L164) | Pending route/body audit |
