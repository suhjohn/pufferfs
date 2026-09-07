#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Set GEMINI_API_KEY for real provider E2E}"
: "${TURBOPUFFER_API_KEY:?Set TURBOPUFFER_API_KEY for real provider E2E}"
project="pufferfs-provider-deletion-${GITHUB_RUN_ID:-local}-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-provider-recovery.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  cleanup_failed=0
  if [[ "$runner_started" == 1 ]]; then
    "${compose[@]}" stop transform-consumer index-consumer transform collector index-cpu index-vector reconciler provider-relay provider-manifest-relay || true
    if ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
      echo "Cleanup failed; retained Compose project $project for cleanup." >&2
      cleanup_failed=1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/provider-deletion.log
  if [[ "$cleanup_failed" == 1 ]]; then exit 1; fi
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build
"${compose[@]}" up -d --wait --scale api=2 --scale collector=2 postgres aws api api-ready transform provider-relay provider-manifest-relay index-cpu index-vector query reconciler collector
runner_started=1
"${compose[@]}" up -d --no-deps transform-consumer index-consumer
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/provider_deletion.py
