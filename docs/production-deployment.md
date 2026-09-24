# Production deployment

The simplified code deploys three ECS/Fargate roles: API, ingestion, and
background. Postgres is externally managed. S3 stores sources and chunks;
Turbopuffer and extraction providers are external services. See the
[role diagram](architecture-and-functionality.md).

## Configuration

Copy `.env.example` and supply the selected environment's credentials. Never
commit `.env`. Required backend secrets are `DATABASE_URL`, `JWT_SECRET`,
`TURBOPUFFER_API_KEY`, and `GEMINI_API_KEY`. Vision fallback additionally needs
`PUFFERFS_VISION_BASE_URL` and `MODAL_PROXY_TOKEN`. ECS uses its task IAM role for
S3; no Modal worker secrets, HTTP worker tokens or SQS URLs are needed.

From `infra/pulumi`, select the intended stack, run
`../../scripts/deploy/configure-pulumi.sh`, `npm run build`, and
`pulumi preview --diff --refresh`. Review the resource removals before applying.
`alarmTopicArn` optionally routes current-work failure/age alarms to SNS.

## Coordinated upgrade from v0.8.2

Migrations 048–050 change the work lifecycle and remove delivery/mutation
columns. Old writers cannot run against the new schema. This upgrade has a
maintenance window; ordinary later releases can use rolling deployments.

1. Back up Postgres and referenced legacy mutation artifacts; retain the previous images. Check that every live root
   has exactly one active namespace (`shard_count=1`). Migration 049 refuses
   multi-namespace roots; those require a separately planned data migration.
   Check whether any other IAM role uses a stack-owned Modal OIDC provider
   before allowing its removal; an externally supplied provider is not managed
   by this stack. The cutover script enforces this check with `iam:ListRoles`
   and refuses removal while another role trusts the provider.
2. Publish the shared CLI release manifest before advertising its API redirect.
   `scripts/deploy/publish-cli-manifest.sh` verifies release archive checksums
   before publishing. The deployment workflow handles existing and new stacks.
3. Run `scripts/deploy/retire-old-workers.sh` from `infra/pulumi`. It stops only
   the four retired PufferFS CPU apps and old ECS API/consumer services. It
   validates Modal's app-list fields and waits for stopped apps with zero
   containers before proceeding. It leaves the shared vision endpoint alone.
   The one-time step needs the old Modal environment credentials. The
   Modal-specific step is skipped after `pipelineVersion=3`. To repair leftover Modal apps after that
   cutover, run `python3 scripts/deploy/retire-modal-workers.py` from the
   repository root with Modal installed, `MODAL_ENVIRONMENT` set, and the
   intended workspace's credentials configured through a Modal profile or
   `MODAL_TOKEN_ID`/`MODAL_TOKEN_SECRET`. This standalone cleanup is safe to
   repeat and does not stop the current ECS services.
4. Apply Pulumi. The API applies production migrations on startup; workers
   retry database operations while startup/migration is in progress. Pulumi
   removes SQS resources, old consumers and the Modal worker IAM/OIDC role.
5. Wait for API and both worker services, check health and logs, then run an
   isolated CLI capture → wait → query/read → update/delete smoke workflow.
   Confirm cleanup and inspect current work failures/age.
6. After retaining the rollback configuration, remove obsolete stack settings:
   `modalSecretKey`, `modalTransformEndpoint`, `modalFileIndexEndpoint`,
   `modalWorkspaceId`, `modalEnvironment` and `modalOidcProviderArn`.
   Retire the matching CPU-worker endpoint/auth settings in the GitHub
   environment. Keep the vision endpoint and `MODAL_PROXY_TOKEN`; they remain
   in use. The old Modal deployment credentials are needed only for the
   one-time retirement step and may also serve other applications.

Do not roll an old image onto the new schema. Rollback requires stopping new
writers and restoring the matching database backup and old deployment. This is
why the one-time migration is separate from an ordinary rolling release.

## Segmented pipeline upgrade (pipeline version 4)

Migration 056 adds shared segments. Old API/worker images cannot interpret
new append memberships, so this is another coordinated cutover. The deployment
retirement script stops the current ECS API and both worker services when the
stack reports pipeline version 3, waits for them to drain, and then permits
Pulumi to start the new roles and migrations. It does not require Modal
deployment credentials for this cutover. Captures remain durable in Postgres/S3;
expired attempts resume from their last committed checkpoints. Keep a database
backup and the previous images. Once format-2 data exists, rolling back to an old
binary requires restoring a matching backup, not merely reversing the schema.

Publish the new CLI after the backend exposes `/catalog-changes` and
`/capture-summary`. Existing CLI versions retain the older API endpoints.
No existing corpus needs to be reindexed. Legacy artifacts stay readable and
pending legacy index jobs can finish unchanged. New extractions select the
segmented format; append reuse begins once a predecessor has that format.

Configure identical `embeddingRequestsPerMinute` / `embeddingTokensPerMinute`
in every API/background role; the deployment exposes their `PUFFERFS_…`
environment settings. These are shared account/model budgets, not worker-local
limits. The initial limits persist in `provider_capacity`; changing them requires
draining embedding producers and updating that row before deploying matching
configuration. Search limits use `searchConcurrency` and
`searchTenantConcurrency` across API replicas. See the
[implementation and verification record](flow-optimization-implementation.md).

## GitHub workflows

- `ci.yml`: Go, web, infrastructure and configuration checks on PR/main.
- `e2e.yml`: real-provider production-process tests, manually callable and used
  by releases. Secrets are confined to the `e2e` environment.
- `release.yml`: paid E2E gate, then CLI archives/checksums and GitHub release.
- `deploy.yml`: manually selected backend/frontend/installer/CLI mirror/all.

Local E2Es use the same scripts. Compose verifies application behavior, not ECS
networking/IAM or production capacity. Do not present a local pass as a deployed
smoke result. A release version must identify the source actually released;
implementation alone does not bump, publish or deploy a version.
