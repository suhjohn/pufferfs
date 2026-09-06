#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Real provider credentials required}"
: "${TURBOPUFFER_API_KEY:?Real provider credentials required}"
# Ordinary configurable cache policy; wait for real elapsed time and the
# unchanged production maintenance schedule, never edit timestamps in SQL.
export PUFFERFS_EMBEDDING_CACHE_RETENTION_SECONDS=120
export PUFFERFS_OBSOLETE_ARTIFACT_RETENTION_SECONDS=120
export PUFFERFS_SOURCE_RETENTION_SECONDS=120
project="pufferfs-retention-${GITHUB_RUN_ID:-local}-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  cleanup_failed=0
  if [[ "$runner_started" == 1 ]]; then
    "${compose[@]}" stop transform-consumer index-consumer transform collector index-cpu index-vector reconciler || true
    if ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
      echo "Cleanup failed; retained Compose project $project for recovery." >&2
      cleanup_failed=1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/retention.log
  "${compose[@]}" ps --all > tests/e2e/artifacts/retention-containers.txt
  if [[ "$cleanup_failed" == 1 ]]; then exit 1; fi
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build
"${compose[@]}" up -d --wait postgres aws api api-ready transform index-cpu index-vector query reconciler
runner_started=1
"${compose[@]}" up -d transform-consumer index-consumer
"${compose[@]}" run --rm --no-deps e2e retention-security
