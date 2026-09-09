# CPU versus A10 embedding capacity

On September 8, 2026 PDT, the current Nomic/PyTorch implementation encoded the
same cold synthetic corpus at **0.65–0.71 vectors/s on CPU** and **74.8–77.0
vectors/s on one A10**. CPU batching alone did not close the cost gap. Retain the
production A10 allocation and batch 32; these small, sequential runs do not
establish an optimum batch size or justify changing the deployed worker count.

## Measured rates

Each run used one bulk embedding worker and one admitted file input. CPU
requested two Modal physical cores (four vCPUs) and 6 GiB; A10 requested one
physical core (two vCPUs), 6 GiB and one A10. All workers used the pinned
`nomic-ai/nomic-embed-text-v1.5` model, 768 dimensions and the production precision
paths: float32 on CPU, float16 on CUDA. Observed packages were PyTorch 2.14.0,
Sentence Transformers 6.0.1 and Transformers 5.16.1 across all five runs.

| Hardware | Encoder batch | Vectors / encoding second | Published chunks / worker wall second |
| --- | ---: | ---: | ---: |
| CPU, 4 vCPU / 6 GiB | 1 | 0.709 | 0.688 |
| CPU, 4 vCPU / 6 GiB | 8 | 0.672 | 0.647 |
| CPU, 4 vCPU / 6 GiB | 32 | 0.645 | 0.628 |
| A10, 2 vCPU / 6 GiB | 8 | 74.835 | 18.114 |
| A10, 2 vCPU / 6 GiB | 32 | 77.049 | 7.636 |

Encoding rate divides 256 actual `encoder_texts` by summed `encode_run` seconds.
This includes tokenization, the forward pass, normalization, CPU conversion and
first-call encoder warmup. It excludes model/container startup, S3, database and
search IO. The worker wall rate spans the first index invocation's start through
the last one's completion, including inter-invocation gaps and publication.

The encoder batch is the number of texts processed together by the model. It is
independent of concurrent file inputs and the up-to-512-vector S3 storage pack.
The test held file concurrency at one and used the same 64-record files, producing
one pack per file. It varied only configured hardware and encoder batch size;
physical hosts and actual placement were not held constant.

CPU runs were in AWS `us-west-2`. The A10 batch-8 worker ran in `us-phoenix-1`;
batch 32 ran in `uk-london-1`. Both were observed as NVIDIA A10 devices. London
had higher measured IO time, so its lower publication rate cannot be attributed
to encoder batch 32. Neither GPU's short encoding interval supports a sustained
GPU-utilization claim. Production uses four admitted file inputs per worker to
overlap IO; this benchmark held that at one for the batch comparison.

CPU sampled peak process RSS was 4.42, 5.25 and 6.86 GiB at batches 1, 8 and 32.
These are samples, not hard bounds. Batch 32 exceeded its 6 GiB request. Resource
requests can burst, and Modal bills CPU/memory against the higher of the request
or actual use. [Modal resource and billing semantics](https://modal.com/docs/guide/resources)

## Cost comparison

Current base rates imply $0.142272/hour for the requested CPU worker and
$1.196712/hour for the A10 worker, including their requested CPU and RAM.
One A10 worker therefore costs as much per hour as approximately 8.41 CPU workers
of this size. [Modal pricing](https://modal.com/pricing)

| Hardware / batch | Requested compute $ / million encoded vectors | Requested compute $ / million published chunks |
| --- | ---: | ---: |
| CPU / 1 | 55.74 | 57.42 |
| CPU / 8 | 58.77 | 61.10 |
| CPU / 32 | 61.22 | 62.93 |
| A10 / 8 | 4.44 | 18.35 |
| A10 / 32 | 4.31 | 43.53 |

These are arithmetic estimates from measured service rates and requested worker
resources, not invoices or total-system costs. They exclude startup, warm idle
time, resource bursting, placement premiums, other roles, network, storage and
search-provider charges. The CPU batch-32 request undercounts its observed RAM
use. Per-million figures extrapolate this small corpus's measured rates; they
are not measurements of a million-vector run.

At the best observed encoding rates, A10 was about 109 times faster per worker
and about 13 times cheaper per encoded vector. Adding enough tested CPU workers
to match that encoding rate would not make CPU cheaper, even assuming perfect
horizontal scaling. Including the observed IO/publication intervals reduced the
GPU cost advantage to approximately 1.3–3.1 times, with the placement caveat above.

This result applies to this model, precision, implementation and input-length
mix. It is not a comparison against optimized or quantized CPU backends, nor a
claim about short query embeddings or cached republication. The earlier
production observation of 192 new vector uploads/s across eight GPUs measured a
different workload and included IO, unfinished files and duplicate cache misses;
it is not the same metric as the encoder-only rates here.

## Corpus, validation and timing

Every cold run captured identical synthetic JSONL contents: four files, 64 lines
per file, 256 distinct chunks and 1,003,700 source bytes. Half the records averaged
663 tokens, half 1,533 tokens, including the document prefix. The pinned tokenizer
measured ranges of 631–697 and 1,456–1,612 tokens. This is a bounded model workload,
not a replay of the 3.66-million-chunk sessions corpus.

The production CLI/API, consumers and native transformation ran as separate
Docker Compose processes. The bulk and query embedding roles ran on real Modal.
Each run provisioned its own cloud PostgreSQL database/login, AWS bucket and SQS
queues, organization and real Turbopuffer namespaces. The database server and its
settings were unchanged. The existing [deployment topology](architecture-and-functionality.md#per-file-pipeline-deployment-roles)
explains the role boundaries preserved by this runner.

All five successful runs verified first-attempt publication, exactly 256 cold
cache misses, persisted vectors matching the published mutation vectors, retained
source bytes, exact line reads, FTS/vector/hybrid search, and a forced reindex
with 256 cache hits and zero encoder calls. API deletion and cloud cleanup
succeeded for every run. No restart/fault or broader authorization coverage is
claimed by this capacity sweep.

| Run | Encoding seconds | Index worker wall span, seconds | Cold capture to publication, seconds | Warm capture to publication, seconds |
| --- | ---: | ---: | ---: | ---: |
| cpu-b1 | 361.072 | 371.968 | 402.790 | 11.868 |
| cpu-b8 | 380.715 | 395.782 | 427.023 | 14.915 |
| cpu-b32 | 396.597 | 407.649 | 439.894 | 12.606 |
| gpu-b8-unpinned | 3.421 | 14.132 | 39.391 | 15.199 |
| gpu-b32-unpinned | 3.323 | 33.527 | 58.949 | 29.314 |

An initial A10 batch-8 attempt constrained to AWS/us-west never allocated a GPU
during an observed roughly five-minute queue wait. Its isolated runner was
stopped via Docker and ordinary cleanup removed all run-owned resources. It is
excluded from throughput results. The two successful A10 runs removed placement
constraints and retained the actual regions in telemetry.

## Reproduction and evidence

Load the required credentials as described in the repository instructions and
configure the explicit database login suffix if the provider requires one. Then:

```sh
uv run --with boto3 --with 'psycopg[binary]' --with modal tests/e2e/cloud_index.py \
  --scenario worker-throughput --containers 1 --inputs 1 \
  --gpu none --cpu 2 --memory-mib 6144 --records-per-file 64 --batch-size 8
```

Repeat for CPU batch sizes 1 and 32. For A10, use `--gpu A10 --cpu 1` with batches
8 and 32. Query embedding used `PUFFERFS_MODAL_QUERY_EMBED_GPU=none`. The CPU runs
set `PUFFERFS_MODAL_WORKER_CLOUD=aws` and `PUFFERFS_MODAL_WORKER_REGION=us-west`;
successful GPU runs left both unset. Fresh isolated organizations make each run
cold without deleting any production cache. The shared throughput reporter now
emits encoder vectors/s separately from publication and capture rates.

Private raw evidence is archived under
`~/.codex/artifacts/pufferfs-cpu-batch-20260908/`: sanitized run logs,
`final-results.json`, `resources.jsonl`, fixture token lengths/source hashes,
reproduction helpers and the aborted placement attempt. Runtime credentials
were removed by cleanup; they are not part of that archive.
