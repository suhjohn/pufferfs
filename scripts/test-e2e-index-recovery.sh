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
    "${compose[@]}" stop transform-consumer index-consumer transform index-cpu index-vector reconciler index-relay || true
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
"${compose[@]}" up -d --wait postgres aws api api-ready transform index-relay index-cpu index-vector query reconciler
runner_started=1
"${compose[@]}" up -d transform-consumer index-consumer
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/index_recovery.py)
"${driver[@]}" lost-capture
# Kill the actual embedding/index process after real provider acceptance but
# before it receives the response. Do not mutate leases, receipts or DB rows.
"${compose[@]}" kill -s SIGKILL index-vector
"${driver[@]}" release
"${compose[@]}" up -d --no-deps --wait index-vector
"${driver[@]}" lost-recovered
# Keep stale physical rows available for the publication-filter assertions.
# This phase does not claim recurring stale-row cleanup is being verified.
"${compose[@]}" stop reconciler
"${driver[@]}" stale-capture
"${compose[@]}" kill -s SIGKILL index-cpu
"${compose[@]}" up -d --no-deps --wait index-cpu
"${driver[@]}" stale-current
"${driver[@]}" stale-released
"${driver[@]}" root-deleted
"${compose[@]}" up -d --no-deps --wait reconciler
"${driver[@]}" root-cleaned
