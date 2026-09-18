# Native embedding throughput investigation

These measurements predate the local [base64 redaction change](file-ingestion-and-chunking.md).
Corpus token/cost estimates below describe the unredacted input. They are not
estimates for newly extracted, redacted text.

Measured September 17, 2026. Production is still the SQS/Modal v0.8.2
deployment. The benchmarks below use the uncommitted, simplified worker runtime
in this checkout. Neither that runtime nor the isolated 256-document hotfix has
been deployed by this investigation.

## Sessions capture and production progress

The requested incremental CLI sync took **7.3 seconds**, capturing six changed
files. This measures upload/capture, not completion of indexing. At 12:16 UTC,
the captured root contained 5,430 files, 18.94 GB and 3,398,543 extracted chunks:

| Index state | Files | Chunks |
| --- | ---: | ---: |
| Published | 2,702 | 157,658 |
| Pending | 2,728 | 3,240,885 |

That is 49.8% of files but only **4.6% of chunks** published: the unfinished
files are much larger. File-count progress does not predict remaining time.

All captured files had completed extraction. Pending index work had exhausted
five attempts: 1,423 files last failed with HTTP 400, 1,303 with HTTP 429, and
two with expired leases. The prior queue snapshot had 2,726 dead-lettered index
messages and no active index messages. Queue counts and unfinished-file counts
are different quantities and were sampled at different times.

The 400 response explicitly rejects more than 256 embedded documents per
request. The local hotfix splits writes at the network boundary, including
already prepared mutation artifacts. The real-provider E2E passed for both
the deployed architecture's hotfix and the simplified runtime, including a
257-document artifact, worker termination, replay and search/read verification.
Retrying the unchanged oversized request cannot recover it.

During the three complete hours when production was publishing files, it
published 45,690–46,476 chunks/hour, approximately **762–775 chunks/minute**.
This excludes writes to unfinished files and repeated writes. A file is exposed
only after all its writes succeed, so published-file counts understate work
already attempted on large files. The largest file contains 86,126 chunks.

## Input volume

A later local scan found 5,431 files totaling 18.98 GB (the live directory grew).
A streaming scan identified **8.925 GB, or 47.0%, of base64 data-URI payloads**,
mostly PNG/JPEG images. The current JSONL contract chunks exact text bytes;
these embedded payloads therefore reach the text embedding model as base64.
They do not go through image-to-text extraction.

A local Qwen3-Embedding-8B tokenizer processed 2,048 byte-weighted random
6,000-byte windows: 12.29 MB and 6.70 million tokens. The resulting estimate is
**10.35 billion tokens** for the raw corpus. The approximate sampling interval
is 10.19–10.52 billion; chunk boundaries, provider prompting and directory
growth add uncertainty beyond that interval. No session text was included in
the synthetic benchmarks or this report.

[Turbopuffer documents](https://turbopuffer.com/docs/embedding) a default
2-million-token/minute limit per organization/model for new organizations;
the actual account quota is opaque and was not confirmed. At that sustained
rate, 10.35 billion tokens alone require about **86 hours**, before retries.
At the listed Qwen 8B price of $0.07/million tokens, a complete fresh embedding
pass would be approximately **$725**, excluding storage, queries and repeats.
This is an estimate, not an observed charge or a request to reindex the corpus.

## Controlled E2E comparison

Each case captures eight synthetic JSONL files, 2,048 chunks and 8,034,304
source bytes, then forces a new extraction. Half the files contain English
text; half contain UTF-8/numeric logs. A full native embedding phase reports
2,535,584 accepted embedding tokens. Timings run from CLI sync start to catalog
publication and exclude the subsequent validation/cleanup duration.

| Mode | Index concurrency | Documents/write | Initial seconds | Reindex seconds |
| --- | ---: | ---: | ---: | ---: |
| Native Qwen | 1 | 256 | 178.652 | 208.724 |
| Native Qwen | 4 | 256 | 375.598 | 578.194 |
| Native Qwen | 4 | 64 | 68.393 | 108.489 |
| Full-text only | 4 | 256 actual / 512 bound | 8.232 | 6.109 |

The four-worker initial phase received 52 HTTP 429 responses versus 13 with
one worker. File attempts rose from eight to sixteen. It was 2.10× slower in
this comparison; increasing concurrency is not a demonstrated improvement.
Its reindex received 75 HTTP 429 responses and needed twenty file attempts;
one file finished only on its fifth attempt. It was 2.77× slower than the
one-worker reindex. Both cases ultimately passed all workflow assertions.

With four workers and 64-document requests, the initial phase took 68.393
seconds (**5.49× faster** than four workers/256 documents). It received eight
HTTP 429s and completed all eight files on their first work attempt. Successful
writes reported 2,535,584 tokens, or 2.22M accepted tokens/minute over this short
phase. Short-run burst capacity is not a sustained quota measurement.
Reindex took 108.489 seconds (**5.33× faster**), with 42 HTTP 429s and twelve
file attempts. All workflow assertions passed and isolated resources were
removed after all four cases.

The smaller-batch reindex also proved partial replay: three successful request
payloads were accepted a second time after a later batch failed. Those repeats
reported another **193,080 embedding tokens**, 7.6% above the corpus's 2,535,584
tokens. This is measured wasted work, not just a hypothetical retry concern.

**Recommended tested setting:** four index slots with
`PUFFERFS_EMBEDDING_BATCH_DOCUMENTS=64`, subject to the account's shared quota.
The configurable worker setting is implemented locally; its default remains
256 and neither production configuration nor the isolated legacy hotfix was
changed to 64 by this investigation. The measurements establish a useful
improvement, not a universal optimum across content, quotas or providers.

Results are recorded in `tests/e2e/artifacts/worker-throughput.jsonl` and
`worker-throughput-network.jsonl`. The latter contains request counts,
durations and provider billing/performance metadata, never document text.
An aggregate evidence snapshot is saved alongside this report as
[indexing-performance-results.json](indexing-performance-results.json).

In the one-index-worker case, initial publication took 178.652 seconds and
forced reindex took 208.724 seconds. Across both phases, index invocations spent
376.601 of 377.146 worker-seconds inside provider writes/retries (**99.86%**).
Database transactions took 0.226 worker-seconds; transformation took 1.180
worker-seconds. These inclusive spans are not disjoint wall-clock measurements.
The reindex also experienced one relay connection failure and a file retry.
All eight batches in each phase reported embedding tokens again on reindex;
these measurements do not demonstrate a free warm embedding path.

The benchmark uses separate production CLI, API, ingestion and background
processes with production migrations, real Postgres, LocalStack S3, and real
Turbopuffer/native Qwen. A network relay forwards requests unchanged. It checks
all retained source bytes, exact reads, chunk counts, stored vectors, and public
FTS/vector/hybrid queries (FTS only for the no-vector control).

Differences from production: local Docker hosting and storage/database latency,
the simplified database scheduler, small synthetic files, and shared external
provider quota without exclusive reservation. Ingestion concurrency stays at
four; the comparison varies background indexing concurrency. These short runs
do not establish a universal optimum or production-scale completion time.

## Reproduce

```sh
set -a; source .env; set +a
PUFFERFS_E2E_THROUGHPUT_CONCURRENCY=1 bash scripts/test-e2e-worker-throughput.sh
PUFFERFS_E2E_THROUGHPUT_CONCURRENCY=4 bash scripts/test-e2e-worker-throughput.sh
PUFFERFS_E2E_THROUGHPUT_CONCURRENCY=4 PUFFERFS_EMBEDDING_BATCH_DOCUMENTS=64 \
  bash scripts/test-e2e-worker-throughput.sh
PUFFERFS_E2E_THROUGHPUT_NO_VECTOR=true bash scripts/test-e2e-worker-throughput.sh
python3 scripts/embedding-throughput-report.py tests/e2e/artifacts
```

Run native cases sequentially to avoid competing with each other for quota.
The harness removes its isolated root/provider resources and Compose volumes
after validation. These are paid manual benchmarks, not a new mandatory CI gate.

## Improvements in priority order

1. Ship the verified 256-document cap and explicitly recover exhausted work.
   A code deployment alone does not redrive an SQS dead-letter queue.
   For the simplified runtime, use the measured 64-document setting to reduce
   throttling; requests remain safely below the provider's 256-document limit.
2. Confirm and raise the account's Qwen token quota with Turbopuffer. More local
   workers cannot increase an organization-level provider allowance. For the
   estimated raw corpus, a six-hour embedding target requires roughly 28.8M
   tokens/minute before overhead. No provider support message was sent.
3. Define a generic, explicit policy for embedded media in structured text.
   Preserve original source/read behavior while selecting useful semantic
   content. Do not implement Codex-path or fixture-specific exclusions. The
   measured 47% is a byte saving opportunity, not a proven token/time saving.
4. Avoid restarting a large file from its first write after prolonged 429s.
   The SDK retries individual calls, but exhausted SDK retries currently return
   to file-level retry. A bounded, lease-aware retry of the current batch could
   preserve progress during transient throttling; crash-safe progress would
   require a deliberate checkpoint design. Neither change is implemented here.
   The deployed Modal index invocation also has a one-hour timeout, which needs
   review for files containing tens of thousands of chunks at constrained
   embedding throughput. The simplified long-lived worker has no equivalent
   per-file one-hour invocation timeout.
5. Offer full-text-only indexing when semantic search is unnecessary. It is a
   supported explicit mode, but the sessions root was not changed to that mode.

The raw corpus was not force-reindexed, the production backlog was not redriven,
and no commit, push, deployment or CLI release was performed.
