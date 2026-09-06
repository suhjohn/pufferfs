#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Set GEMINI_API_KEY for real media Batch E2E}"
: "${TURBOPUFFER_API_KEY:?Set TURBOPUFFER_API_KEY for real media index E2E}"
project="pufferfs-media-${GITHUB_RUN_ID:-local}-$$"
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
      echo "Cleanup failed; retained Compose project $project. Retry cleanup before down --volumes." >&2
      cleanup_failed=1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/media.log
  "${compose[@]}" ps --all > tests/e2e/artifacts/media-containers.txt
  if [[ "$cleanup_failed" == 1 ]]; then exit 1; fi
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
# These roots are vector-disabled; do not start the unrelated query/vector
# pools. Corpus and index-recovery suites cover the real Nomic path separately.
"${compose[@]}" build api transform e2e
"${compose[@]}" up -d --wait postgres aws api api-ready transform index-cpu collector reconciler
runner_started=1
"${compose[@]}" up -d --no-deps transform-consumer index-consumer
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/media.py 2>&1 |
  python3 -u tests/e2e/redact.py | tee tests/e2e/artifacts/media-results.log
