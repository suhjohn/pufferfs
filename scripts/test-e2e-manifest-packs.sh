#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Required for real provider cleanup}"
: "${TURBOPUFFER_API_KEY:?Required for real search}"
export PUFFERFS_SOURCE_RETENTION_SECONDS=120
project="pufferfs-manifest-packs-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-api.yml -f compose.e2e-manifests.yml)
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
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/manifest-packs.log
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build api transform e2e
"${compose[@]}" up -d --no-build --wait --scale api=2 postgres aws api api-ready transform index-cpu reconciler
runner_started=1
"${compose[@]}" up -d --no-deps transform-consumer index-consumer
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/manifest_packs.py)
"${driver[@]}" packed
batch=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/capture_batches.py)
"${batch[@]}" verify
"${compose[@]}" restart api
"${compose[@]}" run --rm --no-deps api-ready
"${batch[@]}" restarted
