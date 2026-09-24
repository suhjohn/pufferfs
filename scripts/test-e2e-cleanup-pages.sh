#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Required for real provider cleanup}"
: "${TURBOPUFFER_API_KEY:?Required for real search}"
project="pufferfs-cleanup-pages-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-uploads.yml -f compose.e2e-cleanup.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  "${compose[@]}" stop ingestion background || true
  if [[ "$runner_started" == 1 ]]; then
    "${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/cleanup_pages.py release || true
    if ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
      echo "Cleanup failed; retained project $project for recovery." >&2
      exit 1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/cleanup-pages.log
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build api ingestion e2e
"${compose[@]}" up -d --wait postgres aws api api-ready
runner_started=1
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/cleanup_pages.py)
"${driver[@]}" prepare
"${compose[@]}" up -d --wait background
"${driver[@]}" partial
"${compose[@]}" restart background
"${driver[@]}" recovered
