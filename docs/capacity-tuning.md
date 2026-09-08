# Capacity tuning: measurement contract

Status: experiments in progress; no final optimum has been established.

## Objective and fixed constraints

Maximize sustainable capture-to-search throughput while preserving correct
publication, reliable recovery, and responsive search. Measure representative
native text/logs, cached reindexing, and document/media provider work.

- Keep the current PlanetScale PS-5 plan and all database settings fixed.
- Keep one transform consumer replica and one index consumer replica.
- Keep direct API/consumer connection pools at two per process, Python worker
  client pools at two per process, and the existing transaction pooler's shared
  backend limit at four. Do not use more connections to conceal long transactions.
- Use synthetic isolated inputs for fault/recovery E2E experiments. Production
  capacity testing is explicitly authorized: capture synthetic user roots using
  the released CLI, observe real queues/workers and measure search under load.
  Track the separately
  authorized sessions reindex as an operational observation, not a controlled
  before/after comparison.
- Find efficient per-container input concurrency before increasing container
  counts. Retain independent query capacity.

## Variables and experiment order

For each consumer, K is concurrent admitted jobs. For each execution role, N is
maximum containers, I is concurrent file inputs per container, B is items per
compute/provider batch, and H is CPU/RAM/GPU per container. The initial admission
relationship with one consumer is K = N × I. Index CPU/GPU destinations share
one consumer budget today; that relationship assumes a homogeneous destination.

The [deployment diagram and role table](architecture-and-functionality.md#per-file-pipeline-deployment-roles)
define each role's trigger, input, output and handoff. Their baseline allocations
for this experiment are below. Modal CPU numbers are physical cores; ECS CPU
numbers are vCPUs. Memory values are requests unless explicitly called limits.

| Deployable role | Baseline replicas / admission | Per-instance resources | Scaling signal and next adjustment |
| --- | --- | --- | --- |
| Local agent | One capture; four uploads; batches of 128 files | User machine; this run explicitly uses a 4 GiB spool limit | Measure upload/link and local hashing rates before increasing upload concurrency; a capture batch must fit the configured spool. |
| API server | Two replicas; DB pool two each | ECS: 1 vCPU / 2 GiB each | Keep redundancy. Size or scale against request latency, CPU and memory; account for added direct DB connections and rolling overlap. |
| Transform consumer | One replica; K=16 | ECS: 1 vCPU / 2 GiB | Within this one-replica experiment, change K with transformation execution slots, not in response to idle consumer CPU. |
| Transformation worker | N=4, I=4 | Modal: two cores / 4 GiB | Increase I when IO leaves CPU idle and measured RSS permits it; increase N when runnable work waits with the existing workers busy. Provider uploads also have their own per-job concurrency limit. |
| Collector dispatcher | One scheduled invocation per minute | Modal default: 0.125 core / 128 MiB | Launch the configured collector count; dispatcher replicas do not accelerate a provider job. |
| Provider collector / assembler | N=1, I=1; 50-second loop, up to 900 seconds for a long operation | Modal: two cores / 4 GiB | Increase N when overdue polls, completed provider results or assembly work wait on busy collectors. Remote provider processing time alone is not this signal. Claims use renewable DB leases. |
| Index consumer | One replica; K=8 | ECS: 1 vCPU / 2 GiB | Admit enough jobs to feed the chosen CPU/GPU slots. Both destinations share K and one SQS queue today; a mixed workload can consume slots unevenly. |
| CPU index worker | N=2, I=4 | Modal: two cores / 4 GiB | Overlap IO first; add containers only when publication throughput rises without exceeding DB/search limits. |
| Bulk index worker | N=2, I=4, B=32; N=2/3/4 sweep in progress at fixed K=8 | Modal: A10, two cores / 4 GiB | Stop increasing I when encoder wait dominates and GPU utilization is high. Compare N using publication throughput, utilization, DB waits and cost; do not treat a higher cap as added capacity until workers are running. |
| Query embedder | One warm, maximum two; I=1 | Modal: L4, two cores / 4 GiB | Preserve a separate query pool. Compare CPU/GPU latency on the same search workload before choosing hardware; scale against query queueing and latency. |
| Reconciler | One scheduled invocation per minute | Modal: one core / 512 MiB | Track oldest unrepaired handoff and cleanup work. The current deployment is a singleton; multi-reconciler execution needs separate E2E coverage before raising this cap. |

For R consumer replicas, total admitted work is R × K. Adding replicas while
keeping K unchanged also increases outstanding worker calls and direct DB
connections. Start a horizontal scaling calculation with the required service
rate and measured per-container rate; do not derive GPU count from a consumer's
spare CPU. The fixed shared pooler's four backends do not grow with worker count.
Stop a scaling step when throughput flattens, DB acquisition/query latency rises,
publication/provider limits bind, or query responsiveness degrades. These are
scaling rules; unmeasured steps above the tested sweep are not capacity claims.

1. Record source revision, deployed settings, workload size/type, cache state,
   warm/cold model state, resource placement, and fixed downstream limits.
2. Compare index I = 1, 2, 4 on the same bounded GPU allocation. Separate actual
   encoder time from encoder-lock wait, database wait/transaction time, S3 IO,
   and search writes. Compare B where memory and encoder measurements justify it.
3. Measure native transformation, provider submission/collection, CPU indexing,
   query latency under ingestion load, and local capture. Tune the observed
   bottleneck, preserving durability and bounded memory at every step.
4. Increase N only after useful per-container throughput stops improving with I
   and downstream services retain capacity. Adjust K within the single consumer.
5. Verify the chosen configuration with complete CLI/API workflows, updates,
   deletes, duplicate delivery, crashes/restarts, authorization, and source reads.

## Evidence required before completion

Record capture-to-search time, sustained source bytes/chunks per second, queue
wait, CPU/GPU utilization, peak RAM/VRAM, error/retry rates, database connection
occupancy, search latency, and compute cost. Include before/after measurements
with their workload/cache/placement limits. A live changing backlog cannot alone
prove a controlled speedup or an optimum across file formats.

The final deliverable must contain tested K/N/I/B/H settings and a scaling rule
for each role, deployment verification, and explicit limits. A green build or
one successful small-file test does not establish this result.

## September 7 production experiment ledger

This ledger distinguishes desired configuration from observed execution. Times
below use UTC on September 8 (September 7 in Pacific time).

| Revision / event | Observed result | Limitation |
| --- | --- | --- |
| `41ddc6f`: native index input concurrency; separate encoder wait/run metrics | Recovery E2E passed in 546.33 s; full corpus E2E passed in 903.96 s. Compose index and transform processes each reached four concurrent inputs. | Most full-corpus index work uses the CPU role; this does not establish concurrent GPU throughput. |
| Initial 64-text L4 batch | One CUDA OOM at 03:00:19 in an older, single-input worker. | Predates execution of the concurrent revision. Batch 64 is not a reliable baseline for all captured records. |
| 03:13:35: two L4 containers, four inputs, batch 32 deployed | Modal continued serving old workers and reported insufficient L4 scheduling capacity. | Exclude this transition from stable throughput comparisons. |
| `b5d0763`: deployment shell correction | The old `/bin/sh` interpreter skipped Bash capacity conditions. Fresh deployment applied one index consumer with K=8, verified in ECS task revision 62. | Earlier desired K=8 observations actually ran K=16; account for this in historical comparisons. |
| `76495ec`: configurable bulk GPU type | A10 experiment requested at 03:25:10 with the same N=2, I=4, B=32 and K=8. | Record actual startup, placement, throughput, memory and errors before choosing hardware. |
| Rollout probes at 03:33:43 and 03:35:49 | Temporarily allowing N=3 brought up one replacement during each probe. The cap returned to N=2 after 60 s and 10 s respectively; A10 workers started at 03:34:10 and 03:36:12. | Old workers draining long jobs can temporarily exceed the steady allocation. Scheduling messages alone did not identify the constraint. Exclude the transition from comparisons. |
| Cache IO transaction change | Real delayed-GET and late-PUT retirement E2E passed in 450.69 s; index crash/restart/replay E2E passed in 536.80 s. | Production throughput measurements still pending. |

The transform consumer remains one replica with K=16 (N=4, I=4). Both consumers
request 1 ECS vCPU and 2 GiB. Modal execution workers request `cpu=2` and 4096
MiB; Modal counts CPU in **physical cores**, so this is four vCPUs in Modal's
pricing terminology. Memory is a request, not a hard cap. See
[Modal resource units](https://modal.com/docs/guide/resources).

GPU-only base rates are $0.7992/hour for L4 and $1.1016/hour for A10, excluding
CPU, memory, network and any placement premium. Compare completed chunks per
dollar, including idle/warm container time, rather than GPU hourly price alone.
[Modal pricing](https://modal.com/pricing)

The initial sessions capture failed after 5,649.29 seconds because one
128-file batch exceeded the default 2 GiB spool. A normal resume began at
02:38:10 with an explicitly configured 4 GiB spool. Report the failed attempt,
resume gap and resumed work separately; do not present this as one clean run.

The synthetic production root captures 16 distinct JSONL files (3,872 chunks),
one 500-row CSV and one four-page PDF. Capture accepted 18 files / 16,941,934
bytes in 3.678 seconds. The PDF completed real provider transformation about
109 seconds after enqueue. Indexing initially waited behind the sessions
backlog; capture acceptance is not search readiness.

Reproduce CLI observations with `scripts/production-capacity.py`: `capture`
creates its own root and stores fixture expectations/cleanup identity in
`--state`; `verify` waits for publication and checks reads/search; `query`
measures FTS/vector/hybrid latency; `cleanup` deletes only the recorded synthetic
root. Supply the released binary using `--binary`. Query probes include CLI
startup and network time and are low-rate responsiveness checks, not a query
capacity benchmark. Root/work identities and logs remain in private artifacts.

Modal gives containers the same hostname (`modal`), so per-container aggregation
must use the documented `MODAL_TASK_ID`; local Compose uses its unique hostname.
Metrics now also identify the image. Historical metrics containing only that
shared hostname cannot establish per-container peaks across multiple workers.

CloudWatch's preceding two-hour observation (120 one-minute samples per service)
reported average consumer CPU below 0.14% and peak one-minute CPU below 0.98%;
average memory below 0.39% of the requested 2 GiB. These consumers hold queue
receipts and outstanding HTTP calls. Their idle CPU is not evidence of spare
GPU computation or a reason to request larger consumer instances.
