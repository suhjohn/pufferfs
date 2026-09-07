#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Required for real provider cleanup}"
: "${TURBOPUFFER_API_KEY:?Required for real search}"
project="pufferfs-transform-handoff-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-api.yml -f compose.e2e-transform-handoff.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  "${compose[@]}" stop transform-consumer index-consumer transform index-cpu reconciler || true
  if [[ "$runner_started" == 1 ]] && ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
    echo "Cleanup failed; retained project $project for recovery." >&2
    exit 1
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/transform-handoff.log
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build api transform e2e
"${compose[@]}" up -d --wait --scale api=2 --scale transform=2 postgres aws api api-ready transform index-cpu
runner_started=1
"${compose[@]}" up -d --no-deps --scale transform-consumer=2 transform-consumer
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/transform_handoff.py)
"${driver[@]}" capture
"${compose[@]}" stop transform-consumer transform
"${compose[@]}" up -d --no-deps --wait reconciler
"${driver[@]}" recovered
"${compose[@]}" stop reconciler
"${compose[@]}" restart api
"${compose[@]}" run --rm --no-deps api-ready
"${compose[@]}" up -d --no-deps --wait --scale transform=2 transform
"${compose[@]}" up -d --no-deps --scale transform-consumer=2 transform-consumer index-consumer
"${driver[@]}" updated
