#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Explicit credentials only. Do not silently load production AWS/DATABASE_URL.
: "${GEMINI_API_KEY:?Set GEMINI_API_KEY for real Gemini Batch E2E}"
: "${TURBOPUFFER_API_KEY:?Set TURBOPUFFER_API_KEY for real Turbopuffer E2E}"
project="pufferfs-e2e-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  cleanup_failed=0
  if [[ "$runner_started" == 1 ]]; then
    "${compose[@]}" stop ingestion background || true
    # Keep the real provider cleanup report if it fails. Preserve disposable
    # containers/state in that case so cleanup can be retried, not guessed.
    if ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
      echo "Cleanup failed; retained Compose project $project. Retry cleanup before down --volumes." >&2
      cleanup_failed=1
    fi
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/compose.log
  "${compose[@]}" ps --all > tests/e2e/artifacts/containers.txt
  if [[ "$cleanup_failed" == 1 ]]; then
    exit 1
  fi
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build
# Register durable work with all execution stopped, then resume real processes.
"${compose[@]}" up -d --wait postgres aws api api-ready
runner_started=1
"${compose[@]}" run --rm --no-deps e2e native-capture
"${compose[@]}" up -d --wait ingestion
"${compose[@]}" run --rm --no-deps e2e native-transformed
"${compose[@]}" run --rm --no-deps e2e follow-backlog
"${compose[@]}" stop ingestion
"${compose[@]}" run --rm --no-deps e2e capture
"${compose[@]}" run --rm --no-deps e2e multipart-recovery
"${compose[@]}" up -d --wait ingestion background
"${compose[@]}" run --rm --no-deps e2e verify
"${compose[@]}" run --rm --no-deps e2e authorization
"${compose[@]}" stop ingestion background
"${compose[@]}" run --rm --no-deps e2e outage
"${compose[@]}" restart api
"${compose[@]}" run --rm --no-deps api-ready
"${compose[@]}" up -d --wait ingestion background
"${compose[@]}" run --rm --no-deps e2e resumed
