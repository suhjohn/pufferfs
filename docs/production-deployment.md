# Production Deployment

This repository uses four deployment surfaces:

- GitHub Actions for CI, release, and manual component deployments.
- Pulumi for AWS infrastructure, backend image builds, and ECS task definitions.
- Modal for independently deployed transformation, collection, indexing and reconciliation roles.
- S3 + CloudFront for the static web app and installer script.

## Native embedding deployment

Deploy matching API/consumers and four Modal apps: `transform_app.py`,
`index_app.py`, `collector_app.py`, and `reconciliation_app.py`. The CPU index
worker sends text to Turbopuffer, using `qwen/qwen3-embedding-8b` with 4096 float32
dimensions. The API uses native query embedding. Vector-disabled roots use the
same worker without embedding. No separate GPU or query app is deployed.

Migration 047 removes `embedding_packs`. For a populated old deployment, stop
old ingestion workers first, retire the authorized old data, then deploy the
new schema and roles. Old mutation artifacts contain supplied Nomic vectors
and cannot be replayed by the new worker. Changing the model alone does not
backfill an existing index. No mixed-model compatibility path is provided.
Deleting source files for old Modal app definitions does not stop their deployed
instances; explicitly retire those apps and remove obsolete endpoint variables.

The deployment uses `MODAL_TRANSFORM_ENDPOINT` and `MODAL_FILE_INDEX_ENDPOINT`.
The index app/label default to `pufferfs-index` / `pufferfs-file-index`. Optional
`PUFFERFS_INDEX_APP_NAME` and `PUFFERFS_INDEX_ENDPOINT_LABEL` allow isolated cloud
runs. The role requests two CPUs and 4 GiB RAM; it has no GPU dependency.
Retain Modal OIDC/IAM, the worker and endpoint-auth secrets, S3 and both SQS queues
for remaining roles. The API has no Modal RPC dependency; consumers use the
shared endpoint-auth secret.

`uv run --with boto3 --with 'psycopg[binary]' --with modal tests/e2e/cloud_index.py`
provisions a disposable Postgres database/login, isolated S3 bucket/SQS queues,
and temporary Modal CPU index app. Production CLI/API/consumers run in Compose.
It requires real provider credentials, Modal authentication, IAM-user credentials
capable of scoped STS federation, and a database login capable of creating roles
and databases. Use `sslmode=verify-full`, an installed CA bundle, and explicit
`PUFFERFS_CLOUD_DB_LOGIN_SUFFIX` when the database router requires it. No
production deployment or resources are replaced by this test. On cleanup
failure, protected recovery state is retained.

Workers use `/etc/ssl/certs/ca-certificates.crt` for TLS trust. Do not disable
certificate/hostname checks. `scripts/deploy/audit-worker-cloud.py` remains a
read-only check of the actual Modal worker principal's STS/S3/SQS access;
write/delete permissions still need a real workflow test.

Source, provider, obsolete artifact and index cleanup still run independently.
The Compose suite verifies fresh migrations and separate processes against
real external providers. It does not establish populated upgrade correctness,
production IAM, or physical erasure of versioned bucket history.

## Branch and PR Gates

Enable branch protection on `main` and require the `ci` workflow before merge.
The `ci` workflow runs on pull requests and pushes to `main`:

- `go build ./...`
- Docker Compose E2E against real model/search providers (review-gated `e2e` environment)
- `npm ci && npm run build` in `infra/pulumi`
- `npm ci && npm run build` in `web`
- `goreleaser check`
- `sh -n scripts/install.sh`

## GitHub Environments

Create GitHub Environments named `staging`, `production` and `e2e`. The `e2e`
environment requires reviewers plus dedicated `GEMINI_API_KEY` and
`TURBOPUFFER_API_KEY` secrets. Do not approve untrusted PR code for these secrets.

With both provider credentials in the shell environment, run
`python3 scripts/deploy/configure-e2e-secrets.py --repo OWNER/REPO` to configure
the E2E environment. A newly created environment requires manual approval by
the authenticated GitHub user (self-approval is allowed). Existing review rules
are preserved; an existing environment without reviewers is rejected before
uploading any secrets. Values travel over stdin, never command arguments/logs.
This command neither dispatches nor approves a workflow. Credentials copied
from `.env` are not independently issued CI-only provider keys.

For `production`, enable required reviewers so deploys need approval before they
can touch AWS.

### Required Environment Secrets

Set these secrets on each deploy environment:

```text
AWS_ROLE_ARN
PULUMI_ACCESS_TOKEN        # when using Pulumi Cloud
PULUMI_CONFIG_PASSPHRASE   # when using an S3/local passphrase backend
DATABASE_URL
JWT_SECRET
TURBOPUFFER_API_KEY
MODAL_SECRET_KEY
MODAL_TOKEN_ID
MODAL_TOKEN_SECRET
GEMINI_API_KEY
GOOGLE_CLIENT_SECRET
```

Optional secrets:

```text
PUFFERFS_ADMIN_KEY_HASH
POSTHOG_KEY
STRIPE_SECRET_KEY
STRIPE_WEBHOOK_SECRET
MODAL_TRANSFORM_ENDPOINT
MODAL_FILE_INDEX_ENDPOINT
```

Modal endpoints may be stored as variables instead of secrets.

### Required Environment Variables

Set these variables on each deploy environment:

```text
PROJECT_NAME=pufferfs
MODAL_WORKSPACE_ID=<workspace-id>
MODAL_ENVIRONMENT=main
FRONTEND_URL=https://pufferfs.com
COOKIE_DOMAIN=.pufferfs.com
API_DOMAIN=api.pufferfs.com
WEB_DOMAIN=pufferfs.com
OAUTH_REDIRECT_URL=https://api.pufferfs.com/auth/callback
GOOGLE_CLIENT_ID=<oauth-client-id>
VITE_API_URL=https://api.pufferfs.com
VITE_POSTHOG_KEY=<posthog-project-token>
VITE_POSTHOG_HOST=https://us.i.posthog.com
POSTHOG_ENABLED=true
POSTHOG_HOST=https://us.i.posthog.com
API_HTTPS_READY=true
WEB_HTTPS_READY=true
ENABLE_BILLING=false
VITE_ENABLE_BILLING=false
```

Optional transactional email variables:

```text
ENABLE_EMAIL_LOGIN=true
TRANSACTIONAL_EMAIL_FROM=team@your-domain.com
TRANSACTIONAL_EMAIL_FROM_NAME=PufferFS
TRANSACTIONAL_EMAIL_REPLY_TO=support@your-domain.com
TRANSACTIONAL_EMAIL_APP_URL=https://your-domain.com
TRANSACTIONAL_EMAIL_SES_REGION=us-west-2
TRANSACTIONAL_EMAIL_IDENTITY=your-domain.com
TRANSACTIONAL_EMAIL_IDENTITY_ARN=arn:aws:ses:us-west-2:123456789012:identity/your-domain.com
SES_CONFIGURATION_SET=
SES_FEEDBACK_EMAIL=
SES_FEEDBACK_IDENTITY_ARN=
SES_ENDPOINT_URL=
```

Leave `TRANSACTIONAL_EMAIL_FROM` unset to keep invites database-only and make
email-code login unavailable. Set `TRANSACTIONAL_EMAIL_IDENTITY` when you want
Pulumi to create the SES identity and output DNS validation records. Omit it
when the sender identity is already verified in SES; in that case set
`TRANSACTIONAL_EMAIL_IDENTITY_ARN` if you want the ECS task role scoped to that
identity instead of all SES identities. The older `INVITE_EMAIL_*` Pulumi config
names remain accepted as aliases.

Required Modal endpoint variables, unless stored as secrets:

```text
MODAL_TRANSFORM_ENDPOINT
MODAL_FILE_INDEX_ENDPOINT
```

The workflow creates/updates `pufferfs-workers` with the database/provider
configuration, artifact bucket, queue URLs and `PUFFERFS_AWS_ROLE_ARN` from
Pulumi outputs. Workers exchange their Modal identity for refreshable AWS role
credentials. The role trusts the configured workspace/environment and named
worker apps; its policy covers the artifact bucket and sending to the two work
queues. Pulumi creates the Modal OIDC provider unless
`MODAL_OIDC_PROVIDER_ARN` selects an existing account-wide provider.

The workflow also updates `pufferfs-endpoint-auth` with
`PUFFERFS_MODAL_ENDPOINT_AUTH_KEY`, matching the consumers’
`MODAL_SECRET_KEY`. Transformation and index endpoints use this secret.

Size connections and execution together. `PUFFERFS_DB_MAX_CONNS` bounds each
API/consumer pool (default 4). `PUFFERFS_TRANSFORM_MAX_CONTAINERS` and
`PUFFERFS_MODAL_INDEX_MAX_CONTAINERS` set both the respective consumer
concurrency and worker container caps (default 16). Budget across every replica,
overlapping deployments, worker DB operations, collector, reconciliation and
administrative connections. For a small database, begin with 2 for all three
settings and increase only with measured headroom. Compose limits Postgres to
25 connections and Go pools to 2 to exercise this constraint.

To share database connections across Python workers, set the production GitHub
environment secret `PUFFERFS_WORKER_DATABASE_URL` to a transaction-pooled
endpoint. Deployment installs it as `DATABASE_URL` in the worker secret, covering
transformation, indexing, collection and reconciliation. The API and ECS
consumers retain the direct `DATABASE_URL` for startup migrations and their
session advisory lock.
Secret updates reach newly started containers. For otherwise unchanged apps,
use `modal app rollover APP --env main --strategy rolling` to refresh their
configuration, then verify the new containers use the pooled endpoint before
increasing capacity. Running work finishes on the previous containers.

Keep the pooler's server connection limit separate from its client limit. For
example, with 25 database connections and 3 reserved, four pooled server
connections plus four Go processes capped at two each consume at most 12
application connections in steady operation. Rolling Go replicas can raise
that to 20; leave the remaining connections for administration and startup.
Worker client connections can exceed four because each releases its server
connection when its transaction ends. Keep certificate and hostname verification
enabled, and measure transaction waits before increasing worker concurrency.

Tune transformation's container count separately from its input concurrency.
For example, four containers with `PUFFERFS_TRANSFORM_INPUTS_PER_CONTAINER=4`
give the transform consumer 16 outstanding job slots. This overlaps storage
and provider IO within each existing 2-vCPU/4-GiB worker. Keep the total at most
64, and measure memory and throughput with representative file formats.
Index concurrency is controlled independently by `PUFFERFS_INDEX_INPUTS_PER_CONTAINER`
and `PUFFERFS_MODAL_INDEX_MAX_CONTAINERS`. Measure native-provider rate limits
and memory before increasing either setting.

The Compose suites route Python workers through a real transaction-mode
PgBouncer process capped at four server connections, while the API, consumers
and assertion runner use direct Postgres connections. The local pooler uses
isolated fixture credentials without TLS; production uses provider-managed
PgBouncer with verified TLS. Postgres version and hosting differences remain
as documented for the existing Compose environment.

Optional CLI release variables:

```text
PUFFERFS_CLI_LATEST_VERSION=0.7.0
PUFFERFS_CLI_MIN_VERSION=0.7.0
PUFFERFS_CLI_DOWNLOAD_BASE_URL=https://pufferfs.com/releases
```

## AWS OIDC Role

Use GitHub OIDC instead of long-lived AWS access keys.

Create an IAM role trusted by `token.actions.githubusercontent.com` and restrict
it to this repository and environment, for example:

```text
repo:suhjohn/pufferfs:environment:production
```

The role needs permission to manage the resources in `infra/pulumi`, including
ECR, ECS, ELB, CloudFront, S3, ACM, IAM role attachments, Secrets Manager,
CloudWatch Logs, SQS and VPC resources. An upgrade also needs permission to
retire the previous EFS and service-discovery resources.

Store that role ARN as the environment secret `AWS_ROLE_ARN`.

## Pulumi Backend

For GitHub Actions, use a remote Pulumi backend. The deploy workflow supports:

- Pulumi Cloud: set `PULUMI_ACCESS_TOKEN`.
- S3 backend: set environment variable `PULUMI_BACKEND_URL`, for example
  `s3://pufferfs-pulumi-state-940827433648-us-west-2?region=us-west-2`,
  and set `PULUMI_CONFIG_PASSPHRASE`.

Do not commit `Pulumi.<stack>.yaml`; stack config is set by
`scripts/deploy/configure-pulumi.sh`.

## Manual Component Deploys

Run `.github/workflows/deploy.yml` from GitHub Actions with:

```text
environment: production
pulumi_stack: optional override, defaults to the environment stack
component: backend | frontend | installer | cli-release | all
```

Component behavior:

- `backend`: configures Pulumi, previews, builds an immutable Docker image tagged
  with the workflow commit SHA, pushes to ECR, runs `pulumi up`, waits for the API
  and consumers, then configures and deploys the six Modal roles.
- `frontend`: builds `web/`, syncs `dist/client/` to the Pulumi-managed web
  bucket, and invalidates CloudFront.
- `installer`: uploads `scripts/install.sh` to `/install.sh` in the web bucket
  and invalidates only that path.
- `cli-release`: mirrors a tagged CLI release to `/releases/<tag>/` in the web
  bucket, writes `/releases/latest.txt`, and invalidates those paths.
- `all`: runs backend, frontend, installer and CLI release mirroring.

## CLI Releases

`release.yml` runs only when a SemVer tag is pushed:

```sh
git tag v0.3.0
git push origin v0.3.0
```

It creates GitHub Release artifacts through GoReleaser. The cross-platform
installer script (`curl -fsSL https://pufferfs.com/install.sh | sh`) downloads
these release archives and verifies their checksums.

After a successful release, update the deploy environment variable
`PUFFERFS_CLI_LATEST_VERSION` and run the `backend` deploy component so
`GET /cli/version` advertises the new release.

## Transactional Email DNS

Login-code and invite emails use Amazon SES only when
`TRANSACTIONAL_EMAIL_FROM` is set. For a new domain, set
`TRANSACTIONAL_EMAIL_IDENTITY` to the domain you want SES to verify, for example
`your-domain.com` or `mail.your-domain.com`, then run the backend Pulumi deploy
once.

After the deploy, read:

```sh
cd infra/pulumi
pulumi stack output inviteEmailDkimValidationRecords
pulumi stack output inviteEmailIdentityVerificationStatus
pulumi stack output transactionalEmailDkimValidationRecords
pulumi stack output transactionalEmailIdentityVerificationStatus
```

Create each returned record in your DNS provider as a CNAME. The names look
like:

```text
<token>._domainkey.your-domain.com  CNAME  <token>.dkim.amazonses.com
```

Keep these records DNS-only if your DNS provider has proxying. SES will mark
the identity verified after it sees the records. If the SES account is still in
sandbox, request production access in the same `TRANSACTIONAL_EMAIL_SES_REGION`;
otherwise SES can only send to verified recipient addresses.

## Local GitHub Actions

Use `act` through the repo wrapper to reproduce workflow dispatch and CI behavior
locally:

```sh
brew install act
scripts/actions-local.sh list
scripts/actions-local.sh ci
scripts/actions-local.sh deploy-backend-dryrun
scripts/actions-local.sh release-dryrun
```

The wrapper reads optional `.env.act` and `.secrets.act` files; both are ignored
by git through the local config ignore rules. The deploy command is guarded
because it can touch real infrastructure:

```sh
PUFFERFS_ACT_RUN_DEPLOY=1 scripts/actions-local.sh deploy-backend
```

## Local Deploy Equivalents

Populate Pulumi config from local `.env`:

```sh
cd infra/pulumi
set -a
source ../../.env
set +a
pulumi stack select prod
../../scripts/deploy/configure-pulumi.sh
```

For production, use the `prod` stack with `AWS_REGION=us-west-2`. Do not use a
second production stack with the same `pufferfs.com` / `api.pufferfs.com`
domains unless you also use different domain names. The only `us-east-1`
resource that remains is the CloudFront ACM certificate provider, because AWS
requires CloudFront certificates in `us-east-1`.

Backend:

```sh
npm run build
pulumi preview --diff
pulumi up --yes
```

Frontend:

```sh
cd ../../web
VITE_API_URL=https://api.pufferfs.com VITE_ENABLE_BILLING=false npm run build
cd ../infra/pulumi
aws s3 sync ../../web/dist/client/ "s3://$(pulumi stack output webBucketName)" --delete --exclude 'releases/*'
aws cloudfront create-invalidation --distribution-id "$(pulumi stack output webDistributionId)" --paths '/*'
```

Installer:

```sh
cd infra/pulumi
aws s3 cp ../../scripts/install.sh "s3://$(pulumi stack output webBucketName)/install.sh" \
  --content-type 'text/x-shellscript' \
  --cache-control 'public, max-age=300'
aws cloudfront create-invalidation --distribution-id "$(pulumi stack output webDistributionId)" --paths '/install.sh'
```
