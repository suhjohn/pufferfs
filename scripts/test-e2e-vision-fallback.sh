#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Set GEMINI_API_KEY}"
: "${TURBOPUFFER_API_KEY:?Set TURBOPUFFER_API_KEY}"
: "${MODAL_PROXY_TOKEN:?Set MODAL_PROXY_TOKEN}"
: "${PUFFERFS_VISION_BASE_URL:?Set PUFFERFS_VISION_BASE_URL}"
project="pufferfs-vision-${GITHUB_RUN_ID:-local}-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-provider-recovery.yml -f compose.e2e-vision.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  cleanup_failed=0
  if [[ "$runner_started" == 1 ]]; then
    "${compose[@]}" stop transform-consumer index-consumer transform collector index reconciler provider-relay provider-manifest-relay || true
    if ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
      echo "Cleanup failed; retained Compose project $project." >&2
      cleanup_failed=1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/vision.log
  "${compose[@]}" ps --all > tests/e2e/artifacts/vision-containers.txt
  if [[ "$cleanup_failed" == 1 ]]; then exit 1; fi
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build
"${compose[@]}" up -d --wait --scale api=2 postgres aws api api-ready transform provider-relay provider-manifest-relay index reconciler
runner_started=1
"${compose[@]}" up -d --no-deps transform-consumer index-consumer
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e)
"${driver[@]}" /e2e/vision_fallback.py capture
"${driver[@]}" /e2e/provider_recovery.py result-arm
"${compose[@]}" up -d --no-deps --wait --scale collector=2 collector
"${driver[@]}" /e2e/vision_fallback.py result-held
"${compose[@]}" kill -s SIGKILL collector
"${driver[@]}" /e2e/provider_recovery.py manifest-release
"${compose[@]}" up -d --no-deps --wait --scale collector=2 collector
"${driver[@]}" /e2e/vision_fallback.py verify
