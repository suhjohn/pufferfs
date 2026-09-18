#!/usr/bin/env bash
# One-time, coordinated cutover. Run from infra/pulumi after preview/build.
set -euo pipefail
outputs="$(pulumi stack output --json)"
if [[ "$(jq -r '.pipelineVersion // 0' <<< "$outputs")" == 3 ]]; then exit 0; fi
cluster="$(jq -r '.ecsClusterArn // empty' <<< "$outputs")"
if [[ -z "$cluster" ]]; then exit 0; fi # Fresh deployment.
# An OIDC provider is account-wide. Only remove a managed provider when no
# other role trusts it; externally supplied providers are not deleted by Pulumi.
provider="$(jq -r '.modalOidcProvider // empty' <<< "$outputs")"
old_role="$(jq -r '.modalWorkerRoleArn // empty' <<< "$outputs")"
state="$(pulumi stack export)"
if [[ -n "$provider" ]] && jq -e --arg arn "$provider" \
  '.deployment.resources[] | select(.type == "aws:iam/openIdConnectProvider:OpenIdConnectProvider" and .outputs.arn == $arn)' <<< "$state" >/dev/null; then
  roles="$(aws iam list-roles)"
  if jq -e --arg provider "$provider" --arg old_role "$old_role" \
    '.Roles[] | select(.Arn != $old_role) | .AssumeRolePolicyDocument.Statement[]? |
     .Principal.Federated? | if type == "array" then .[] else . end | select(. == $provider)' \
    <<< "$roles" >/dev/null; then
    echo 'Another IAM role trusts the managed Modal OIDC provider. Transfer its ownership before cutover.' >&2
    exit 1
  fi
fi
: "${MODAL_TOKEN_ID:?Old Modal apps must be stopped before schema migration}"
: "${MODAL_TOKEN_SECRET:?Old Modal apps must be stopped before schema migration}"
: "${MODAL_ENVIRONMENT:?Set the old Modal environment}"
python3 -m pip install 'modal>=1.5.5,<2'
apps="$(python3 -m modal app list --env "$MODAL_ENVIRONMENT" --json)"
for app in pufferfs-transform pufferfs-batch-collector pufferfs-index pufferfs-reconciliation; do
  mapfile -t ids < <(jq -r --arg app "$app" '.[] | select(.Description == $app and .State != "stopped") | .["App ID"]' <<< "$apps")
  for id in "${ids[@]}"; do
    python3 -m modal app stop "$id" --env "$MODAL_ENVIRONMENT" --yes
  done
done
mapfile -t candidates < <(jq -r '(.workerServiceArns[]?), .apiServiceArn // empty' <<< "$outputs")
services="$(aws ecs describe-services --cluster "$cluster" --services "${candidates[@]}")"
if jq -e '.failures[]? | select(.reason != "MISSING")' <<< "$services" >/dev/null; then
  echo 'Cannot inspect old services; refusing the schema cutover.' >&2
  exit 1
fi
mapfile -t old_services < <(jq -r '.services[] | select(.status == "ACTIVE") | .serviceArn' <<< "$services")
for service in "${old_services[@]}"; do
  aws ecs update-service --cluster "$cluster" --service "$service" --desired-count 0 >/dev/null
done
if [[ ${#old_services[@]} -gt 0 ]]; then
  aws ecs wait services-stable --cluster "$cluster" --services "${old_services[@]}"
fi
echo 'Old writers stopped. New API migration and ECS roles can now start.'
