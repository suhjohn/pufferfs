# AWS infrastructure

This stack builds the Go API and Python worker images and deploys three
ECS/Fargate services: API, ingestion, background. It provisions private task
subnets, ALB, NAT, ECR, artifact S3, static-web S3/CloudFront, Secrets Manager,
CloudWatch logs and current-work alarms. Postgres is supplied externally.

See the [deployment runbook](../../docs/production-deployment.md) for the
coordinated upgrade; do not run old writers against migrations 048–050.

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

Worker concurrency defaults to four file jobs per process (1–64).
`workerIngestionConcurrency`, `workerBackgroundConcurrency`, and each worker
service's desired count control separate limits. Collection and maintenance
have their own threads. `workerCpu` / `workerMemory` configure worker
resources; defaults are 2 vCPU / 4 GiB. API resources are separately configured.
Set `alarmTopicArn` to receive failure and age alarms. Multiple workers claim
disjoint database jobs; replicas multiply the configured execution capacity.

No SQS or Modal CPU deployment is part of this topology. The optional Modal
vision endpoint is an external provider accessed through its proxy token.
