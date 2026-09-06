#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Real Gemini Batch credentials required}"
: "${TURBOPUFFER_API_KEY:?Real Turbopuffer credentials required}"
project="pufferfs-formats-${GITHUB_RUN_ID:-local}-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  cleanup_failed=0
  if [[ "$runner_started" == 1 ]]; then
    "${compose[@]}" stop transform-consumer index-consumer transform collector index-cpu reconciler || true
    if ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
      echo "Cleanup failed; retained Compose project $project for recovery." >&2
      cleanup_failed=1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/formats.log
  "${compose[@]}" ps --all > tests/e2e/artifacts/formats-containers.txt
  if [[ "$cleanup_failed" == 1 ]]; then exit 1; fi
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
# All fixtures use ordinary vector-disabled roots. The production native CPU
# index role and real search provider still publish every file; GPU is separate.
"${compose[@]}" build api transform e2e
"${compose[@]}" up -d --wait postgres aws api api-ready transform index-cpu collector reconciler
runner_started=1
"${compose[@]}" up -d --no-deps transform-consumer index-consumer
"${compose[@]}" run --rm --no-deps e2e format-variants 2>&1 |
  python3 -u tests/e2e/redact.py | tee tests/e2e/artifacts/formats-results.log
