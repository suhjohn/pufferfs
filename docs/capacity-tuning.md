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
- Use synthetic isolated inputs for E2E experiments. Track the separately
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
