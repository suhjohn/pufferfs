#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Set GEMINI_API_KEY for real provider E2E}"
: "${TURBOPUFFER_API_KEY:?Set TURBOPUFFER_API_KEY for real provider E2E}"
project="pufferfs-provider-discovery-${GITHUB_RUN_ID:-local}-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-provider-recovery.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  cleanup_failed=0
  if [[ "$runner_started" == 1 ]]; then
    "${compose[@]}" stop ingestion background provider-relay provider-manifest-relay || true
    if ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
      echo "Cleanup failed; retained Compose project $project for cleanup." >&2
      cleanup_failed=1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/provider-discovery.log
  if [[ "$cleanup_failed" == 1 ]]; then exit 1; fi
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build
"${compose[@]}" up -d --wait --scale api=2 postgres aws api api-ready ingestion provider-relay provider-manifest-relay
runner_started=1
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/provider_submission_recovery.py)
"${driver[@]}" capture
"${compose[@]}" kill -s SIGKILL ingestion
"${compose[@]}" up -d --no-deps --wait --scale background=2 background
"${driver[@]}" scanned
"${compose[@]}" kill -s SIGKILL background
"${driver[@]}" release
"${compose[@]}" up -d --no-deps --wait --scale background=2 ingestion background
"${driver[@]}" verify
