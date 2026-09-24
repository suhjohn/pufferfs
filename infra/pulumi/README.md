# AWS infrastructure

This stack builds the Go API and Python worker images and deploys three
ECS/Fargate services: API, ingestion, background. It provisions private task
subnets, ALB, NAT, ECR, artifact S3, static-web S3/CloudFront, Secrets Manager,
CloudWatch logs and current-work alarms. Postgres is supplied externally.

See the [deployment runbook](../../docs/production-deployment.md) for the
coordinated upgrade; do not run old writers against migrations 048–050.
The segmented pipeline also requires a coordinated API/worker cutover for
migration 056; `pipelineVersion=4` records that transition.

```sh
cd infra/pulumi
npm ci
pulumi stack select prod
../../scripts/deploy/configure-pulumi.sh
npm run build
pulumi preview --diff --refresh
```

After reviewing the intended environment and cutover, the deployment workflow
runs the retirement step, `pulumi up`, service stabilization and health checks.
Docker, Node, Pulumi and authorized AWS credentials are required. Configuration
comes from `.env.example` / the selected GitHub Environment.

Native embedding batches default to 64 documents, with a hard maximum of 256.
`embeddingBatchDocuments` controls the background task setting; GitHub deployments
populate it from `PUFFERFS_EMBEDDING_BATCH_DOCUMENTS` (default 64).

`embeddingRequestsPerMinute` / `embeddingTokensPerMinute` configure the shared
API/worker provider budget (defaults 1,024 requests and 2,000,000 estimated tokens
per minute). `searchConcurrency` / `searchTenantConcurrency` configure aggregate
and per-tenant search admission (defaults 32/16). Initialized provider budgets
are persisted in Postgres; follow the runbook when changing their values.

Worker concurrency defaults to four file jobs per process (1–64).
`workerIngestionConcurrency`, `workerBackgroundConcurrency`, and each worker
service's desired count control separate limits. Collection and maintenance
have their own threads. `workerCpu` / `workerMemory` configure worker
resources; defaults are 2 vCPU / 4 GiB. API resources are separately configured.
Set `alarmTopicArn` to receive failure and age alarms. Multiple workers claim
disjoint database jobs; replicas multiply the configured execution capacity.

No SQS or Modal CPU deployment is part of this topology. The optional Modal
vision endpoint is an external provider accessed through its proxy token.
