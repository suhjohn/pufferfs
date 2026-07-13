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
    *) printf '%s\n' "" ;;
  esac
}

DEPLOY_REGION="${AWS_REGION:-us-west-2}"
if [ "$DEPLOY_REGION" != "us-west-2" ]; then
  echo "Unsupported deploy region: $DEPLOY_REGION. PufferFS production deploys only use us-west-2." >&2
  exit 1
fi
DEPLOY_AZS="${AVAILABILITY_ZONES:-$(default_availability_zones "$DEPLOY_REGION")}"
if [ -z "$DEPLOY_AZS" ]; then
  echo "Set AVAILABILITY_ZONES for deploy region: $DEPLOY_REGION" >&2
  exit 1
fi
pulumi config set aws:region "$DEPLOY_REGION"
pulumi config set pufferfs:projectName "${PROJECT_NAME:-pufferfs}"
pulumi config set pufferfs:availabilityZones "$DEPLOY_AZS"
pulumi config set pufferfs:imageTag "${IMAGE_TAG:-${GITHUB_SHA:-prod}}"
pulumi config set pufferfs:queueBackend "${PUFFERFS_QUEUE_BACKEND:-sqs}"

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

set_config_if_present pufferfs:enableEmailLogin "${ENABLE_EMAIL_LOGIN:-}"
set_config_if_present pufferfs:transactionalEmailFrom "${TRANSACTIONAL_EMAIL_FROM:-${INVITE_EMAIL_FROM:-}}"
set_config_if_present pufferfs:transactionalEmailFromName "${TRANSACTIONAL_EMAIL_FROM_NAME:-${INVITE_EMAIL_FROM_NAME:-}}"
set_config_if_present pufferfs:transactionalEmailReplyTo "${TRANSACTIONAL_EMAIL_REPLY_TO:-${INVITE_EMAIL_REPLY_TO:-}}"
set_config_if_present pufferfs:transactionalEmailAppUrl "${TRANSACTIONAL_EMAIL_APP_URL:-${INVITE_EMAIL_APP_URL:-}}"
set_config_if_present pufferfs:transactionalEmailSesRegion "${TRANSACTIONAL_EMAIL_SES_REGION:-${INVITE_EMAIL_SES_REGION:-${SES_REGION:-}}}"
set_config_if_present pufferfs:transactionalEmailConfigurationSet "${TRANSACTIONAL_EMAIL_CONFIGURATION_SET:-${SES_CONFIGURATION_SET:-}}"
set_config_if_present pufferfs:transactionalEmailIdentity "${TRANSACTIONAL_EMAIL_IDENTITY:-${INVITE_EMAIL_IDENTITY:-}}"
set_config_if_present pufferfs:transactionalEmailIdentityArn "${TRANSACTIONAL_EMAIL_IDENTITY_ARN:-${INVITE_EMAIL_IDENTITY_ARN:-}}"
set_config_if_present pufferfs:transactionalEmailFeedbackEmail "${TRANSACTIONAL_EMAIL_FEEDBACK_EMAIL:-${SES_FEEDBACK_EMAIL:-}}"
set_config_if_present pufferfs:transactionalEmailFeedbackIdentityArn "${TRANSACTIONAL_EMAIL_FEEDBACK_IDENTITY_ARN:-${SES_FEEDBACK_IDENTITY_ARN:-}}"
set_config_if_present pufferfs:transactionalEmailSesEndpointUrl "${TRANSACTIONAL_EMAIL_SES_ENDPOINT_URL:-${SES_ENDPOINT_URL:-}}"

set_config_if_present pufferfs:apiHttpsReady "${API_HTTPS_READY:-}"
set_config_if_present pufferfs:webHttpsReady "${WEB_HTTPS_READY:-}"

set_config_if_present pufferfs:enableBilling "${ENABLE_BILLING:-}"
set_secret_if_present pufferfs:stripeSecretKey "${STRIPE_SECRET_KEY:-}"
set_secret_if_present pufferfs:stripeWebhookSecret "${STRIPE_WEBHOOK_SECRET:-}"
set_config_if_present pufferfs:stripePriceId "${STRIPE_PRICE_ID:-}"

set_config_if_present pufferfs:posthogEnabled "${POSTHOG_ENABLED:-}"
set_secret_if_present pufferfs:posthogKey "${POSTHOG_KEY:-}"
set_config_if_present pufferfs:posthogHost "${POSTHOG_HOST:-}"
set_config_if_present pufferfs:alarmTopicArn "${PUFFERFS_ALARM_TOPIC_ARN:-}"

set_config_if_present pufferfs:cliLatestVersion "${PUFFERFS_CLI_LATEST_VERSION:-}"
set_config_if_present pufferfs:cliMinVersion "${PUFFERFS_CLI_MIN_VERSION:-}"
set_config_if_present pufferfs:cliDownloadBaseUrl "${PUFFERFS_CLI_DOWNLOAD_BASE_URL:-}"
