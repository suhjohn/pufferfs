#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Set GEMINI_API_KEY for real provider recovery E2E}"
: "${TURBOPUFFER_API_KEY:?Set TURBOPUFFER_API_KEY for real index E2E}"
project="pufferfs-provider-recovery-${GITHUB_RUN_ID:-local}-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-provider-recovery.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  cleanup_failed=0
  if [[ "$runner_started" == 1 ]]; then
    "${compose[@]}" stop ingestion background provider-relay provider-manifest-relay index-relay || true
    if ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
      echo "Cleanup failed; retained Compose project $project. Retry cleanup before down --volumes." >&2
      cleanup_failed=1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/provider-recovery.log
  "${compose[@]}" ps --all > tests/e2e/artifacts/provider-recovery-containers.txt
  if [[ "$cleanup_failed" == 1 ]]; then exit 1; fi
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build
# Keep collection stopped until the accepted response has been lost.
"${compose[@]}" up -d --wait --scale api=2 postgres aws api api-ready ingestion provider-relay provider-manifest-relay index-relay
runner_started=1
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/provider_recovery.py)
"${driver[@]}" manifest-capture
"${compose[@]}" kill -s SIGKILL ingestion
"${driver[@]}" manifest-release
"${compose[@]}" up -d --no-deps --wait --scale background=2 ingestion background
"${driver[@]}" manifest-recovered
"${compose[@]}" stop background
"${driver[@]}" lost-capture
"${compose[@]}" kill -s SIGKILL ingestion
"${driver[@]}" release
"${compose[@]}" up -d --no-deps --wait --scale background=2 ingestion background
"${driver[@]}" lost-recovered
"${compose[@]}" stop background
"${driver[@]}" partial-capture
"${driver[@]}" result-arm
"${compose[@]}" up -d --no-deps --wait --scale background=2 background
"${driver[@]}" result-held
"${compose[@]}" kill -s SIGKILL background
"${driver[@]}" manifest-release
"${compose[@]}" up -d --no-deps --wait --scale background=2 background
"${driver[@]}" partial-collected
"${driver[@]}" partial-recovered
