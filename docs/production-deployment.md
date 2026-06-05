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
AWS_REGION=us-east-1
PULUMI_STACK=prod
PROJECT_NAME=pufferfs
AVAILABILITY_ZONES=["us-east-1a","us-east-1b"]
FRONTEND_URL=https://pufferfs.com
COOKIE_DOMAIN=.pufferfs.com
API_DOMAIN=api.pufferfs.com
WEB_DOMAIN=pufferfs.com
OAUTH_REDIRECT_URL=https://api.pufferfs.com/auth/callback
GOOGLE_CLIENT_ID=<oauth-client-id>
VITE_API_URL=https://api.pufferfs.com
API_HTTPS_READY=true
WEB_HTTPS_READY=true
ENABLE_BILLING=false
VITE_ENABLE_BILLING=false
```

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
PUFFERFS_CLI_DOWNLOAD_BASE_URL=https://github.com/suhjohn/pufferfs/releases/download
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
  `s3://pufferfs-pulumi-state`, and set `PULUMI_CONFIG_PASSPHRASE`.

Do not commit `Pulumi.<stack>.yaml`; stack config is set by
`scripts/deploy/configure-pulumi.sh`.

## Manual Component Deploys

Run `.github/workflows/deploy.yml` from GitHub Actions with:

```text
environment: production
component: backend | frontend | installer | all
```

Component behavior:

- `backend`: configures Pulumi, previews, builds an immutable Docker image tagged
  with the workflow commit SHA, pushes to ECR, and runs `pulumi up`.
- `frontend`: builds `web/`, syncs `dist/client/` to the Pulumi-managed web
  bucket, and invalidates CloudFront.
- `installer`: uploads `scripts/install.sh` to `/install.sh` in the web bucket
  and invalidates only that path.
- `all`: runs backend, frontend, then installer.

## CLI Releases

`release.yml` runs only when a SemVer tag is pushed:

```sh
git tag v0.3.0
git push origin v0.3.0
```

It creates GitHub Release artifacts through GoReleaser and updates the Homebrew
cask tap. To make the tap publish work, create `suhjohn/homebrew-tap` and set
repository secret `HOMEBREW_TAP_GITHUB_TOKEN` with write access to that tap.

After a successful release, update the deploy environment variable
`PUFFERFS_CLI_LATEST_VERSION` and run the `backend` deploy component so
`GET /cli/version` advertises the new release.

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
aws s3 sync ../../web/dist/client/ "s3://$(pulumi stack output webBucketName)" --delete
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
