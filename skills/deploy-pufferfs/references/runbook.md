# PufferFS AWS Deploy Runbook

## Secret hygiene (read first)

- `.env` and `Pulumi.*.yaml` are git-ignored. Never commit them, never paste
  secret values into terminal output, PR descriptions, or summaries. Report key
  names and presence only.
- Every secret goes in via `pulumi config set --secret …` so it is stored
  encrypted. The plaintext-eligible keys below are non-sensitive (domains,
  URLs, public OAuth client id, feature flags).
- Secrets here: `DATABASE_URL`, `JWT_SECRET`, `TURBOPUFFER_API_KEY`,
  `GOOGLE_CLIENT_SECRET`, `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`,
  `PUFFERFS_ADMIN_KEY_HASH`. Everything else is plaintext config.

## Required env

Expect these keys in `/Users/johnsuh/pufferfs/.env`:

```text
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
DATABASE_URL
JWT_SECRET
TURBOPUFFER_API_KEY
PULUMI_CONFIG_PASSPHRASE
MODAL_CHUNK_ENDPOINT
MODAL_EMBED_ENDPOINT
MODAL_QUERY_EMBED_ENDPOINT
MODAL_CHUNK_SHARD_ENDPOINT
MODAL_EMBED_SHARD_ENDPOINT
MODAL_INDEX_SHARD_ENDPOINT
```

Frontend + Google login (required for the web app to work):

```text
WEB_DOMAIN              # apex the frontend serves on, e.g. pufferfs.com
API_DOMAIN              # api host, e.g. api.pufferfs.com
FRONTEND_URL           # https://pufferfs.com
COOKIE_DOMAIN          # .pufferfs.com  (spans apex + api)
OAUTH_REDIRECT_URL     # https://api.pufferfs.com/auth/callback
GOOGLE_CLIENT_ID
GOOGLE_CLIENT_SECRET   # secret
VITE_API_URL           # https://api.pufferfs.com (web build-time)
```

Optional:

```text
AWS_REGION
AWS_PROFILE
AWS_SESSION_TOKEN
PUFFERFS_ADMIN_KEY_HASH
API_HTTPS_READY        # set true only in phase 2 (after ACM cert ISSUED)
WEB_HTTPS_READY        # set true only in phase 2 (after ACM cert ISSUED)
ENABLE_BILLING         # default false
VITE_ENABLE_BILLING    # match ENABLE_BILLING (web build-time)
STRIPE_SECRET_KEY      # secret, only when ENABLE_BILLING=true
STRIPE_WEBHOOK_SECRET  # secret, only when ENABLE_BILLING=true
STRIPE_PRICE_ID        # only when ENABLE_BILLING=true
```

## Preflight

Run:

```sh
set -a
source /Users/johnsuh/pufferfs/.env
set +a

aws sts get-caller-identity --output json
pulumi version
node --version
```

If `node --version` is below 20, prepend the bundled Codex runtime Node:

```sh
export PATH="/Users/johnsuh/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin:$PATH"
node --version
```

## Stack config

Run from `/Users/johnsuh/pufferfs/infra/pulumi`:

```sh
pulumi stack select prod || pulumi stack init prod
pulumi config set aws:region "${AWS_REGION:-us-east-1}"
pulumi config set pufferfs:projectName pufferfs
pulumi config set pufferfs:availabilityZones '["us-east-1a","us-east-1b"]'
pulumi config set pufferfs:imageTag prod
pulumi config set --secret pufferfs:databaseUrl "$DATABASE_URL"
pulumi config set --secret pufferfs:jwtSecret "$JWT_SECRET"
pulumi config set --secret pufferfs:turbopufferApiKey "$TURBOPUFFER_API_KEY"
pulumi config set pufferfs:modalChunkEndpoint "$MODAL_CHUNK_ENDPOINT"
pulumi config set pufferfs:modalEmbedEndpoint "$MODAL_EMBED_ENDPOINT"
pulumi config set pufferfs:modalQueryEmbedEndpoint "$MODAL_QUERY_EMBED_ENDPOINT"
pulumi config set pufferfs:modalChunkShardEndpoint "$MODAL_CHUNK_SHARD_ENDPOINT"
pulumi config set pufferfs:modalEmbedShardEndpoint "$MODAL_EMBED_SHARD_ENDPOINT"
pulumi config set pufferfs:modalIndexShardEndpoint "$MODAL_INDEX_SHARD_ENDPOINT"
if [ -n "${PUFFERFS_ADMIN_KEY_HASH:-}" ]; then
  pulumi config set --secret pufferfs:adminKeyHash "$PUFFERFS_ADMIN_KEY_HASH"
fi

# --- Frontend + Google login (plaintext config; client secret is --secret) ---
if [ -n "${FRONTEND_URL:-}" ];       then pulumi config set pufferfs:frontendUrl "$FRONTEND_URL"; fi
if [ -n "${COOKIE_DOMAIN:-}" ];      then pulumi config set pufferfs:cookieDomain "$COOKIE_DOMAIN"; fi
if [ -n "${API_DOMAIN:-}" ];         then pulumi config set pufferfs:apiDomain "$API_DOMAIN"; fi
if [ -n "${WEB_DOMAIN:-}" ];         then pulumi config set pufferfs:webDomain "$WEB_DOMAIN"; fi
if [ -n "${OAUTH_REDIRECT_URL:-}" ]; then pulumi config set pufferfs:oauthRedirectUrl "$OAUTH_REDIRECT_URL"; fi
if [ -n "${GOOGLE_CLIENT_ID:-}" ];   then pulumi config set pufferfs:googleClientId "$GOOGLE_CLIENT_ID"; fi
if [ -n "${GOOGLE_CLIENT_SECRET:-}" ]; then pulumi config set --secret pufferfs:googleClientSecret "$GOOGLE_CLIENT_SECRET"; fi

# --- HTTPS phase-2 flags (default false; flip to true after ACM cert ISSUED) ---
pulumi config set pufferfs:apiHttpsReady "${API_HTTPS_READY:-false}"
pulumi config set pufferfs:webHttpsReady "${WEB_HTTPS_READY:-false}"

# --- Billing (optional; Stripe secrets only set when enabled) ---
pulumi config set pufferfs:enableBilling "${ENABLE_BILLING:-false}"
if [ "${ENABLE_BILLING:-false}" = "true" ]; then
  pulumi config set --secret pufferfs:stripeSecretKey "$STRIPE_SECRET_KEY"
  pulumi config set --secret pufferfs:stripeWebhookSecret "$STRIPE_WEBHOOK_SECRET"
  if [ -n "${STRIPE_PRICE_ID:-}" ]; then pulumi config set pufferfs:stripePriceId "$STRIPE_PRICE_ID"; fi
fi
```

To confirm config without revealing secrets (`--show-secrets` is omitted on
purpose — secret values render as `[secret]`):

```sh
pulumi config
```

## Deploy (API + infra)

```sh
npm install
npm run build
pulumi preview --diff
pulumi up --yes
```

## Custom domains (Cloudflare DNS, two-phase)

DNS is managed in Cloudflare; Pulumi does **not** write DNS. CloudFront/ALB can
only attach an *issued* cert, so domains come up in two phases.

**Phase 1** is the `pulumi up` above with `WEB_DOMAIN`/`API_DOMAIN` set and the
`*HttpsReady` flags `false`. It creates the ACM certs. Now add these records in
Cloudflare — all **DNS only (grey cloud), not proxied**:

| Type  | Name                | Content (stack output)        |
| ----- | ------------------- | ----------------------------- |
| CNAME | `@` (apex)          | `pulumi stack output webDistributionDomain` |
| CNAME | `api`               | `pulumi stack output apiAlbHostname` |
| CNAME | _acm web validation_| `pulumi stack output webCertValidation` |
| CNAME | _acm api validation_| `pulumi stack output apiCertValidation` |

Wait until both certs read **Issued** in the ACM console, then **Phase 2**:

```sh
pulumi config set pufferfs:apiHttpsReady true
pulumi config set pufferfs:webHttpsReady true
pulumi up --yes
```

(Skip this whole section to run the API on plain HTTP and the frontend on the
default `*.cloudfront.net` domain — leave `WEB_DOMAIN`/`API_DOMAIN` unset.)

## Deploy the frontend (static site)

Build the web app (needs Node 20+) and upload to the web bucket + invalidate the
CDN. `VITE_API_URL` is baked in at build time, so rebuild + resync if it changes.

```sh
( cd ../../web \
  && npm install \
  && VITE_API_URL="$VITE_API_URL" VITE_ENABLE_BILLING="${VITE_ENABLE_BILLING:-false}" npm run build )
aws s3 sync ../../web/dist/client/ "s3://$(pulumi stack output webBucketName)" --delete
aws cloudfront create-invalidation \
  --distribution-id "$(pulumi stack output webDistributionId)" --paths '/*'
```

## Known-good verification

Read outputs:

```sh
pulumi stack output apiUrl
pulumi stack output webUrl
pulumi stack output artifactBucket
pulumi stack output natsUrl
```

Health (API) and a frontend fetch (expect the prerendered landing HTML):

```sh
curl -sS -i --max-time 10 "$(pulumi stack output apiUrl)/health"
curl -sS -I --max-time 10 "$(pulumi stack output webUrl)"
```

ECS services:

```sh
aws ecs describe-services \
  --cluster "$(pulumi stack output ecsClusterArn | awk -F/ '{print $NF}')" \
  --services $(aws ecs list-services --cluster "$(pulumi stack output ecsClusterArn | awk -F/ '{print $NF}')" --region "${AWS_REGION:-us-east-1}" --query 'serviceArns[]' --output text | tr '\t' ' ') \
  --region "${AWS_REGION:-us-east-1}" \
  --query 'services[].{name:serviceName,desired:desiredCount,running:runningCount,pending:pendingCount}' \
  --output json
```

## Common failure handling

### Node 18 runtime errors

Symptom:

```text
TypeError: tracingChannel is not a function
```

Fix:

- Use Node 20+.
- In Codex desktop, prepend the bundled runtime Node to `PATH`.

### Docker context hashing failures

Symptom:

```text
unable to hash build context
```

Fix:

- Verify `.dockerignore` excludes `.git`, `.env`, and local build artifacts.

### ALB returns `503`

Check ECS state before assuming a crash:

```sh
aws ecs describe-services \
  --cluster <cluster-name> \
  --services <api-service-name> \
  --region "${AWS_REGION:-us-east-1}" \
  --query 'services[].{desired:desiredCount,running:runningCount,pending:pendingCount,events:events[0:3].message}' \
  --output json
```

If tasks are still `PENDING` or `PROVISIONING`, wait. If tasks stop or never become healthy, inspect service events and task/container status.

### Partial `pulumi up`

Default response:

- Patch the code or config.
- Rerun `pulumi up --yes`.
- Do not destroy the stack unless the user asks.

## Destroy

```sh
pulumi destroy
```
