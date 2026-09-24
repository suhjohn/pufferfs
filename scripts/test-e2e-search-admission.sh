#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Required for real provider cleanup}"
: "${TURBOPUFFER_API_KEY:?Required for real search}"
project="pufferfs-search-admission-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-api.yml -f compose.e2e-search-admission.yml)
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
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/search-admission.log
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build api ingestion e2e
"${compose[@]}" up -d --wait --scale api=2 postgres aws api api-ready ingestion background
runner_started=1
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/search_admission.py)
"${driver[@]}" prepare
"${driver[@]}" verify
"${driver[@]}" crash-query &
query_pid=$!
"${driver[@]}" crash-held
"${compose[@]}" kill -s SIGKILL api
wait "$query_pid"
"${compose[@]}" up -d --no-deps --wait --scale api=2 api
"${driver[@]}" recovered
