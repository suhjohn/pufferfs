# Production Deployment

This repository uses three deployment surfaces:

- GitHub Actions for CI, release, and manual component deployments.
- Pulumi for AWS infrastructure, backend image builds, and ECS task definitions.
- S3 + CloudFront for the static web app and installer script.

## Branch and PR Gates

Enable branch protection on `main` and require the `ci` workflow before merge.
The `ci` workflow runs on pull requests and pushes to `main`:

- `go test ./...`
- `npm ci && npm run build` in `infra/pulumi`
- `npm ci && npm run build` in `web`
- `goreleaser check`
- `sh -n scripts/install.sh`

## GitHub Environments

Create GitHub Environments named `staging` and `production`.

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
GOOGLE_CLIENT_SECRET
```

Optional secrets:

```text
PUFFERFS_ADMIN_KEY_HASH
POSTHOG_KEY
STRIPE_SECRET_KEY
STRIPE_WEBHOOK_SECRET
MODAL_CHUNK_ENDPOINT
MODAL_EMBED_ENDPOINT
MODAL_QUERY_EMBED_ENDPOINT
MODAL_CHUNK_SHARD_ENDPOINT
MODAL_EMBED_SHARD_ENDPOINT
MODAL_INDEX_SHARD_ENDPOINT
```

Modal endpoints may be stored as variables instead of secrets.

### Required Environment Variables

Set these variables on each deploy environment:

```text
PROJECT_NAME=pufferfs
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

Optional invite email variables:

```text
INVITE_EMAIL_FROM=team@your-domain.com
INVITE_EMAIL_FROM_NAME=PufferFS
INVITE_EMAIL_REPLY_TO=support@your-domain.com
INVITE_EMAIL_APP_URL=https://your-domain.com
INVITE_EMAIL_SES_REGION=us-west-2
INVITE_EMAIL_IDENTITY=your-domain.com
INVITE_EMAIL_IDENTITY_ARN=arn:aws:ses:us-west-2:123456789012:identity/your-domain.com
SES_CONFIGURATION_SET=
SES_FEEDBACK_EMAIL=
SES_FEEDBACK_IDENTITY_ARN=
SES_ENDPOINT_URL=
```

Leave `INVITE_EMAIL_FROM` unset to keep invites database-only. Set
`INVITE_EMAIL_IDENTITY` when you want Pulumi to create the SES identity and
output DNS validation records. Omit it when the sender identity is already
verified in SES; in that case set `INVITE_EMAIL_IDENTITY_ARN` if you want the
ECS task role scoped to that identity instead of all SES identities.

Required Modal endpoint variables, unless stored as secrets:

```text
MODAL_CHUNK_ENDPOINT
MODAL_EMBED_ENDPOINT
MODAL_QUERY_EMBED_ENDPOINT
MODAL_CHUNK_SHARD_ENDPOINT
MODAL_EMBED_SHARD_ENDPOINT
MODAL_INDEX_SHARD_ENDPOINT
```

Optional CLI release variables:

```text
PUFFERFS_CLI_LATEST_VERSION=0.3.0
PUFFERFS_CLI_MIN_VERSION=0.2.0
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
CloudWatch Logs, EFS, VPC resources, and service discovery.

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
  with the workflow commit SHA, pushes to ECR, and runs `pulumi up`.
- `frontend`: builds `web/`, syncs `dist/client/` to the Pulumi-managed web
  bucket, and invalidates CloudFront.
- `installer`: uploads `scripts/install.sh` to `/install.sh` in the web bucket
  and invalidates only that path.
- `cli-release`: mirrors a tagged CLI release to `/releases/<tag>/` in the web
  bucket, writes `/releases/latest.txt`, and invalidates those paths.
- `all`: runs backend, frontend, then installer.

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

## Invite Email DNS

Invite emails use Amazon SES only when `INVITE_EMAIL_FROM` is set. For a new
domain, set `INVITE_EMAIL_IDENTITY` to the domain you want SES to verify, for
example `your-domain.com` or `mail.your-domain.com`, then run the backend
Pulumi deploy once.

After the deploy, read:

```sh
cd infra/pulumi
pulumi stack output inviteEmailDkimValidationRecords
pulumi stack output inviteEmailIdentityVerificationStatus
```

Create each returned record in your DNS provider as a CNAME. The names look
like:

```text
<token>._domainkey.your-domain.com  CNAME  <token>.dkim.amazonses.com
```

Keep these records DNS-only if your DNS provider has proxying. SES will mark
the identity verified after it sees the records. If the SES account is still in
sandbox, request production access in the same `INVITE_EMAIL_SES_REGION`;
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
