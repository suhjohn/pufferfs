#!/bin/sh
set -eu

require_env() {
  key="$1"
  eval "value=\${$key:-}"
  if [ -z "$value" ]; then
    echo "Missing required environment variable: $key" >&2
    exit 1
  fi
}

set_config_if_present() {
  key="$1"
  value="$2"
  if [ -n "$value" ]; then
    pulumi config set "$key" "$value"
  fi
}

set_secret_if_present() {
  key="$1"
  value="$2"
  if [ -n "$value" ]; then
    pulumi config set --secret "$key" "$value"
  fi
}

for key in \
  DATABASE_URL \
  JWT_SECRET \
  TURBOPUFFER_API_KEY \
  MODAL_CHUNK_ENDPOINT \
  MODAL_EMBED_ENDPOINT \
  MODAL_QUERY_EMBED_ENDPOINT \
  MODAL_CHUNK_SHARD_ENDPOINT \
  MODAL_EMBED_SHARD_ENDPOINT \
  MODAL_INDEX_SHARD_ENDPOINT
do
  require_env "$key"
done

default_availability_zones() {
  case "$1" in
    us-west-2) printf '%s\n' '["us-west-2a","us-west-2b"]' ;;
    us-west-1) printf '%s\n' '["us-west-1a","us-west-1c"]' ;;
    *) printf '%s\n' "" ;;
  esac
}

DEPLOY_REGION="${AWS_REGION:-us-west-2}"
DEPLOY_AZS="${AVAILABILITY_ZONES:-$(default_availability_zones "$DEPLOY_REGION")}"
if [ -z "$DEPLOY_AZS" ]; then
  echo "Set AVAILABILITY_ZONES for unsupported deploy region: $DEPLOY_REGION" >&2
  exit 1
fi
pulumi config set aws:region "$DEPLOY_REGION"
pulumi config set pufferfs:projectName "${PROJECT_NAME:-pufferfs}"
pulumi config set pufferfs:availabilityZones "$DEPLOY_AZS"
pulumi config set pufferfs:imageTag "${IMAGE_TAG:-${GITHUB_SHA:-prod}}"

pulumi config set --secret pufferfs:databaseUrl "$DATABASE_URL"
pulumi config set --secret pufferfs:jwtSecret "$JWT_SECRET"
pulumi config set --secret pufferfs:turbopufferApiKey "$TURBOPUFFER_API_KEY"

pulumi config set pufferfs:modalChunkEndpoint "$MODAL_CHUNK_ENDPOINT"
pulumi config set pufferfs:modalEmbedEndpoint "$MODAL_EMBED_ENDPOINT"
pulumi config set pufferfs:modalQueryEmbedEndpoint "$MODAL_QUERY_EMBED_ENDPOINT"
pulumi config set pufferfs:modalChunkShardEndpoint "$MODAL_CHUNK_SHARD_ENDPOINT"
pulumi config set pufferfs:modalEmbedShardEndpoint "$MODAL_EMBED_SHARD_ENDPOINT"
pulumi config set pufferfs:modalIndexShardEndpoint "$MODAL_INDEX_SHARD_ENDPOINT"

set_secret_if_present pufferfs:adminKeyHash "${PUFFERFS_ADMIN_KEY_HASH:-}"

set_config_if_present pufferfs:frontendUrl "${FRONTEND_URL:-}"
set_config_if_present pufferfs:cookieDomain "${COOKIE_DOMAIN:-}"
set_config_if_present pufferfs:apiDomain "${API_DOMAIN:-}"
set_config_if_present pufferfs:webDomain "${WEB_DOMAIN:-}"
set_config_if_present pufferfs:oauthRedirectUrl "${OAUTH_REDIRECT_URL:-}"
set_config_if_present pufferfs:googleClientId "${GOOGLE_CLIENT_ID:-}"
set_secret_if_present pufferfs:googleClientSecret "${GOOGLE_CLIENT_SECRET:-}"

set_config_if_present pufferfs:apiHttpsReady "${API_HTTPS_READY:-}"
set_config_if_present pufferfs:webHttpsReady "${WEB_HTTPS_READY:-}"

set_config_if_present pufferfs:enableBilling "${ENABLE_BILLING:-}"
set_secret_if_present pufferfs:stripeSecretKey "${STRIPE_SECRET_KEY:-}"
set_secret_if_present pufferfs:stripeWebhookSecret "${STRIPE_WEBHOOK_SECRET:-}"
set_config_if_present pufferfs:stripePriceId "${STRIPE_PRICE_ID:-}"

set_config_if_present pufferfs:cliLatestVersion "${PUFFERFS_CLI_LATEST_VERSION:-}"
set_config_if_present pufferfs:cliMinVersion "${PUFFERFS_CLI_MIN_VERSION:-}"
set_config_if_present pufferfs:cliDownloadBaseUrl "${PUFFERFS_CLI_DOWNLOAD_BASE_URL:-}"
