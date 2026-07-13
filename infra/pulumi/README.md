# PufferFS AWS Pulumi Deployment

This stack deploys PufferFS to AWS:

- ECR repository and locally built app image.
- VPC with public ALB subnets, private ECS subnets, and NAT egress.
- ECS/Fargate API service behind an Application Load Balancer.
- ECS/Fargate worker services for `chunk`, `embed`, `index`, `commit`, and `cleanup`.
- Isolated SQS FIFO queues and dead-letter queues for each worker stage.
- CloudWatch alarms for queue age and dead-letter messages; set
  `pufferfs:alarmTopicArn` to route alarms to an SNS topic.
- 3 private NATS JetStream nodes retained as a rollback backend.
- S3 artifact bucket.
- Secrets Manager entries for `DATABASE_URL`, `JWT_SECRET`, `TURBOPUFFER_API_KEY`, and optional `PUFFERFS_ADMIN_KEY_HASH`.
- CloudWatch logs.

Postgres is not created here. Provide `pufferfs:databaseUrl` for the database you want the services to use.

## Requirements

- AWS credentials in the shell, for example via `AWS_PROFILE`, `AWS_ACCESS_KEY_ID`, or SSO.
- Docker running locally because Pulumi builds and pushes the app image to ECR.
- Node 20+ for current Pulumi npm packages.

## Stack Setup

```sh
cd infra/pulumi
npm install

pulumi stack init dev
pulumi stack init prod
pulumi stack select prod
pulumi config set aws:region us-west-2
```

Set environment shape:

```sh
pulumi config set pufferfs:projectName pufferfs
pulumi config set pufferfs:availabilityZones '["us-west-2a","us-west-2b"]'
pulumi config set pufferfs:imageTag "$(git rev-parse --short HEAD)"
```

Set secrets:

```sh
pulumi config set --secret pufferfs:databaseUrl "$DATABASE_URL"
pulumi config set --secret pufferfs:jwtSecret "$(openssl rand -base64 32)"
pulumi config set --secret pufferfs:turbopufferApiKey "$TURBOPUFFER_API_KEY"
```

Optional admin key:

```sh
pulumi config set --secret pufferfs:adminKeyHash <sha256-admin-key-hash>
```

Set Modal endpoints:

```sh
pulumi config set pufferfs:modalChunkEndpoint https://...chunk-file-endpoint.modal.run
pulumi config set pufferfs:modalEmbedEndpoint https://...embed-chunks-endpoint.modal.run
pulumi config set pufferfs:modalQueryEmbedEndpoint https://...embed-query-endpoint.modal.run
pulumi config set pufferfs:modalChunkShardEndpoint https://...chunk-shard-endpoint.modal.run
pulumi config set pufferfs:modalEmbedShardEndpoint https://...embed-shard-endpoint.modal.run
pulumi config set pufferfs:modalIndexShardEndpoint https://...index-shard-endpoint.modal.run
```

Advertise CLI release compatibility from `GET /cli/version`:

```sh
pulumi config set pufferfs:cliLatestVersion 0.3.0
pulumi config set pufferfs:cliMinVersion 0.2.0
pulumi config set pufferfs:cliDownloadBaseUrl https://pufferfs.com/releases
```

Frontend URL (for API OAuth redirects + CORS):

```sh
pulumi config set pufferfs:frontendUrl https://app.pufferfs.com
```

### Custom domains + HTTPS (DNS in Cloudflare)

DNS is managed in Cloudflare, so Pulumi creates the ACM certs and **exports the
records you add by hand** — it does not write DNS. Because CloudFront/ALB can
only attach an *issued* cert, this is a **two-phase** flow per domain.

Domains: frontend on the apex (`pufferfs.com`) via CloudFront, API on
`api.pufferfs.com` via the ALB. Cross-subdomain `Secure` cookies require HTTPS.

**Phase 1 — create certs + get the records:**

```sh
pulumi config set pufferfs:webDomain pufferfs.com
pulumi config set pufferfs:apiDomain api.pufferfs.com
pulumi config set pufferfs:cookieDomain .pufferfs.com
pulumi up
```

Then add these records in Cloudflare (all **DNS only / grey cloud**):

| Cloudflare record | From stack output |
| --- | --- |
| Validation CNAME (web cert) | `pulumi stack output webCertValidation` |
| Validation CNAME (api cert) | `pulumi stack output apiCertValidation` |
| `pufferfs.com` CNAME (flattened) | `pulumi stack output webDistributionDomain` |
| `api` CNAME | `pulumi stack output apiAlbHostname` |

Wait until both certs show **Issued** in the ACM console (a few minutes).

**Phase 2 — attach the certs and serve HTTPS:**

```sh
pulumi config set pufferfs:webHttpsReady true
pulumi config set pufferfs:apiHttpsReady true
pulumi up
```

Without `webDomain`/`apiDomain` the stack serves the frontend on the default
`*.cloudfront.net` domain and the API on plain HTTP — no certs, no DNS needed.

### Google OAuth (optional)

```sh
pulumi config set pufferfs:googleClientId <client-id>.apps.googleusercontent.com
pulumi config set --secret pufferfs:googleClientSecret "$GOOGLE_CLIENT_SECRET"
pulumi config set pufferfs:oauthRedirectUrl https://api.pufferfs.com/auth/callback
```

### Transactional Email

Email-code login and invite notifications use SES. Existing `inviteEmail*`
config keys still work as aliases, but new deployments should use the
transactional names.

```sh
pulumi config set pufferfs:enableEmailLogin true
pulumi config set pufferfs:transactionalEmailFrom team@pufferfs.com
pulumi config set pufferfs:transactionalEmailIdentity pufferfs.com
pulumi config set pufferfs:transactionalEmailAppUrl https://pufferfs.com
```

If the SES identity already exists, set
`pufferfs:transactionalEmailIdentityArn` instead of
`pufferfs:transactionalEmailIdentity`. Pulumi exports
`transactionalEmailDkimValidationRecords` and
`transactionalEmailIdentityVerificationStatus` for DNS verification.

### Payment (optional)

Billing is **off by default**. Deploy without payments and nothing Stripe is
required. To enable it, set the flag and the two Stripe secrets:

```sh
pulumi config set pufferfs:enableBilling true
pulumi config set --secret pufferfs:stripeSecretKey "$STRIPE_SECRET_KEY"
pulumi config set --secret pufferfs:stripeWebhookSecret "$STRIPE_WEBHOOK_SECRET"
```

Optionally set the Stripe price the checkout uses:

```sh
pulumi config set pufferfs:stripePriceId price_123
```

This injects `STRIPE_SECRET_KEY` / `STRIPE_WEBHOOK_SECRET` / `STRIPE_PRICE_ID`
into the API tasks and sets `ENABLE_BILLING=true`. The API registers the
`/billing*` routes only when these are present; otherwise they return 404. Build
the web app with `VITE_ENABLE_BILLING=true` to match. Point your Stripe webhook
at `https://api.pufferfs.com/billing/webhook`.

## Deploy

Production deploys should normally run through
[`.github/workflows/deploy.yml`](../../.github/workflows/deploy.yml), which can
deploy only `backend`, `frontend`, `installer`, `cli-release`, or `all` and
uses immutable commit SHA image tags. The workflow is pinned to `us-west-2` and
derives a single active stack name such as `prod`.

For local deploys:

```sh
npm run build
pulumi preview
pulumi up
```

Pulumi will build `../../Dockerfile`, push it to ECR, then update ECS task definitions.
Set `pufferfs:imageTag` to a new commit SHA for each backend deploy so ECS rolls
forward by task-definition change instead of relying on a mutable tag.

## Outputs

```sh
pulumi stack output apiUrl
pulumi stack output artifactBucket
pulumi stack output appRepositoryUrl
pulumi stack output webBucketName   # S3 bucket for the web build
pulumi stack output webUrl          # CloudFront URL for the frontend
```

### Deploying the frontend

The web app (`../../web`) builds to a static folder; upload it after `pulumi up`:

```sh
cd ../../web && VITE_API_URL=https://api.pufferfs.com npm run build
aws s3 sync dist/client/ "s3://$(cd ../infra/pulumi && pulumi stack output webBucketName)" --delete
```

## Notes

- Production defaults to managed SQS via `pufferfs:queueBackend=sqs`. NATS is
  private to the VPC and can be selected as a rollback with
  `pufferfs:queueBackend=nats`.
- The S3 bucket is managed by this stack. Set `pufferfs:forceDestroyBucket=true` only for throwaway environments.
- The default topology includes one NAT Gateway. For a lower-cost dev stack, we can add a config flag to run tasks in public subnets instead.
