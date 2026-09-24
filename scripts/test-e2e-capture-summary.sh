#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Required for real provider cleanup}"
: "${TURBOPUFFER_API_KEY:?Required for real search}"
project="pufferfs-capture-summary-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-api.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  "${compose[@]}" stop ingestion background || true
  if [[ "$runner_started" == 1 ]] && ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
    echo "Cleanup failed; retained project $project for recovery." >&2
    exit 1
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/capture-summary.log
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build
"${compose[@]}" up -d --no-build --wait --scale api=2 postgres aws api
"${compose[@]}" run --rm --no-deps api-ready
runner_started=1
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/capture_summary.py)
"${driver[@]}" prepare
"${compose[@]}" up -d --no-build --wait --scale api=2 ingestion background
"${driver[@]}" verify
"${compose[@]}" restart api
"${compose[@]}" run --rm --no-deps api-ready
"${driver[@]}" verify
