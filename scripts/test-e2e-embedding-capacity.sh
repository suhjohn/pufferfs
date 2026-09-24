#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Required for real provider cleanup}"
: "${TURBOPUFFER_API_KEY:?Required for real native embeddings}"
project="pufferfs-embedding-capacity-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-api.yml -f compose.e2e-embedding-capacity.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  "${compose[@]}" stop ingestion background index-relay || true
  if [[ "$runner_started" == 1 ]] && ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
    echo "Cleanup failed; retained project $project for recovery." >&2
    exit 1
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/embedding-capacity.log
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build
"${compose[@]}" up -d --no-build --wait --scale api=2 postgres aws api api-ready index-relay ingestion
runner_started=1
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/embedding_capacity.py)
"${driver[@]}" prepare
"${compose[@]}" up -d --no-build --wait --scale api=2 --scale background=2 background
"${driver[@]}" held
"${driver[@]}" release
"${driver[@]}" verify
"${compose[@]}" restart api background
"${compose[@]}" run --rm --no-deps api-ready
"${driver[@]}" restarted
"${driver[@]}" throttled
