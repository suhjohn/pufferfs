#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Set GEMINI_API_KEY for the shared real-provider cleanup client}"
: "${TURBOPUFFER_API_KEY:?Set TURBOPUFFER_API_KEY for real index recovery E2E}"
project="pufferfs-index-recovery-${GITHUB_RUN_ID:-local}-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-index-recovery.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  cleanup_failed=0
  if [[ "$runner_started" == 1 ]]; then
    "${compose[@]}" stop ingestion background index-relay || true
    if ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
      echo "Cleanup failed; retained Compose project $project. Retry cleanup before down --volumes." >&2
      cleanup_failed=1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/index-recovery.log
  "${compose[@]}" ps --all > tests/e2e/artifacts/index-recovery-containers.txt
  if [[ "$cleanup_failed" == 1 ]]; then exit 1; fi
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build
"${compose[@]}" up -d --wait --scale api=2 postgres aws api api-ready ingestion index-relay background
runner_started=1
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/index_recovery.py)
"${driver[@]}" lost-capture
"${compose[@]}" kill -s SIGKILL background
"${driver[@]}" release
"${compose[@]}" up -d --no-deps --wait background
"${driver[@]}" lost-recovered
"${compose[@]}" restart postgres
"${driver[@]}" database-recovered
"${driver[@]}" admission-capture
"${driver[@]}" admission-bounded
"${driver[@]}" live-superseded
"${driver[@]}" pause-deletions
"${driver[@]}" stale-capture
"${compose[@]}" kill -s SIGKILL background
"${compose[@]}" up -d --no-deps --wait background
"${driver[@]}" stale-current
"${driver[@]}" stale-released
"${driver[@]}" root-deleted
"${driver[@]}" root-cleaned
