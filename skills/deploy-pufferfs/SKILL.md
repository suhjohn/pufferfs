---
name: deploy-pufferfs
description: Deploy or update the PufferFS AWS stack from this repository using Pulumi, then verify ECS, NATS, and ALB health. Use when Codex needs to run a real deployment, repair a partial deployment, inspect stack outputs, verify service rollout, or destroy the AWS environment for this repo.
---

# Deploy PufferFS

Use the AWS Pulumi stack in [infra/pulumi](/Users/johnsuh/pufferfs/infra/pulumi). Treat AWS as the only supported deployment target for this skill unless the repo changes.

## Workflow

1. Confirm prerequisites.
2. Load deploy secrets from `.env` without printing secret values.
3. Ensure Pulumi stack config is populated from `.env`.
4. Run `pulumi preview` before `pulumi up` unless the user explicitly wants a blind apply.
5. If `pulumi up` partially succeeds and then fails, fix the cause and rerun `pulumi up` rather than tearing down by default.
6. If custom domains are configured, complete the two-phase Cloudflare/ACM flow (add records, wait for `Issued`, set `*HttpsReady=true`, `pulumi up` again).
7. Build and upload the `web/` frontend (`s3 sync` + CloudFront invalidation).
8. Verify `/health`, the frontend URL, ECS service steady state, and Pulumi outputs before finishing.

## Prerequisites

- Run from `/Users/johnsuh/pufferfs`.
- Expect AWS credentials to already work via `AWS_PROFILE` or environment variables.
- Expect these keys in `.env`: `DATABASE_URL`, `JWT_SECRET`, `TURBOPUFFER_API_KEY`, `MODAL_CHUNK_ENDPOINT`, `MODAL_EMBED_ENDPOINT`, `MODAL_QUERY_EMBED_ENDPOINT`, `MODAL_CHUNK_SHARD_ENDPOINT`, `MODAL_EMBED_SHARD_ENDPOINT`, `MODAL_INDEX_SHARD_ENDPOINT`.
- For the web frontend + Google login, also expect: `WEB_DOMAIN`, `API_DOMAIN`, `FRONTEND_URL`, `COOKIE_DOMAIN`, `OAUTH_REDIRECT_URL`, `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `VITE_API_URL`. Optional billing adds `ENABLE_BILLING`/`VITE_ENABLE_BILLING` and the `STRIPE_*` keys. See the runbook for the full mapping to `pulumi config`.
- Expect `PULUMI_CONFIG_PASSPHRASE` in `.env` when using the local Pulumi secrets backend.

Read [references/runbook.md](./references/runbook.md) for the exact commands and checks.

## Node Runtime

Pulumi's current npm packages require Node 20+.

- Check `node --version` before running Pulumi.
- If the local shell is on Node 18, prefer the bundled Codex runtime Node and prepend it to `PATH`.
- In Codex desktop, call `load_workspace_dependencies` and use its Node path if needed.

## Deploy Rules

- Use `set -a; source .env; set +a` before Pulumi commands so config values come from the repo-local environment.
- Keep secrets out of terminal summaries. It is fine to report key names, presence, and redacted config.
- Use the existing `prod` stack if it already exists. Initialize it only when missing.
- Configure Pulumi from `.env`; do not hand-edit `Pulumi.<stack>.yaml` unless the user asks.
- Prefer `pulumi stack output ...` for reporting deploy results.
- Secrets (`DATABASE_URL`, `JWT_SECRET`, `TURBOPUFFER_API_KEY`, `GOOGLE_CLIENT_SECRET`, `STRIPE_*`, `PUFFERFS_ADMIN_KEY_HASH`) must be set with `pulumi config set --secret`. `.env` and `Pulumi.*.yaml` are git-ignored — never commit them or print secret values; report key names and presence only.
- Backend image deploys should use an immutable git SHA image tag (`pufferfs:imageTag`) so ECS rolls through a task-definition change instead of relying on a mutable `prod` tag.
- The frontend is a separate static deploy: build `web/` (Node 20+), `aws s3 sync web/dist/client/` to the `webBucketName` bucket, then invalidate `webDistributionId`. The browser API base is baked in at build time via `VITE_API_URL`.
- Custom domains use Cloudflare DNS in a two-phase flow: first `pulumi up` creates the ACM certs, then add the exported validation + host CNAMEs in Cloudflare (DNS-only), wait for ACM `Issued`, then set `apiHttpsReady=true`/`webHttpsReady=true` and `pulumi up` again.

## Verification

After `pulumi up`, verify all of the following:

- `pulumi stack output apiUrl`
- `curl <apiUrl>/health` returns `200`
- API ECS service is steady
- Worker ECS services are steady
- All three NATS ECS services are steady
- `pulumi stack output webUrl`, and `curl -I <webUrl>` returns `200` with HTML (frontend uploaded)

If ALB returns `503`, do not assume failure immediately. Check ECS tasks and service events first. The common case is that tasks are still provisioning.

## Partial Deploys

If an update fails after creating some AWS resources:

- Inspect the failing Pulumi resource and patch the code or config.
- Rerun `pulumi up` on the same stack.
- Avoid `pulumi destroy` unless the user asks to tear the environment down.

## Destruction

Use `pulumi destroy` only when the user explicitly asks to remove the environment. This stack creates paid resources including NAT Gateway, ALB, ECS, EFS, ECR, S3, and CloudWatch.
