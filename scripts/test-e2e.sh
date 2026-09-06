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
    "${compose[@]}" stop transform-consumer index-consumer transform collector index-cpu index-vector reconciler || true
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
# Start the API and HTTP workers without consumers or scheduled maintenance.
# Only reconciliation will repair the intentionally failed initial SQS send.
"${compose[@]}" up -d --wait postgres aws api api-ready transform index-cpu index-vector query
runner_started=1
"${compose[@]}" run --rm --no-deps e2e handoff-outage
"${compose[@]}" up -d --no-deps reconciler
failed_tick=0
for ((attempt=0; attempt<45; attempt++)); do
  if "${compose[@]}" logs --no-color --tail=100 reconciler |
      grep -F 'reconciler scheduled invocation failed:' >/dev/null; then
    failed_tick=1
    break
  fi
  sleep 1
done
if [[ "$failed_tick" != 1 ]]; then
  echo "Reconciler did not attempt delivery during the SQS outage." >&2
  exit 1
fi
"${compose[@]}" run --rm --no-deps e2e handoff-recovered
"${compose[@]}" up -d --no-deps --wait reconciler
"${compose[@]}" run --rm --no-deps e2e native-capture
"${compose[@]}" up -d transform-consumer
"${compose[@]}" run --rm --no-deps e2e native-transformed
"${compose[@]}" run --rm --no-deps e2e follow-backlog
"${compose[@]}" stop transform
"${compose[@]}" restart transform-consumer
"${compose[@]}" run --rm --no-deps e2e native-replay
"${compose[@]}" stop transform-consumer
"${compose[@]}" up -d --no-deps --wait transform
"${compose[@]}" up -d --wait collector
# Capture must finish with work durably in SQS while no executor can claim it.
"${compose[@]}" run --rm --no-deps e2e capture
"${compose[@]}" run --rm --no-deps e2e multipart-recovery
"${compose[@]}" up -d transform-consumer index-consumer
"${compose[@]}" run --rm --no-deps e2e verify
# Actual network unavailability. Consumers keep polling and must not ack failed
# handoffs; the CLI must still capture, and readers retain the published version.
"${compose[@]}" stop transform index-cpu
"${compose[@]}" run --rm --no-deps e2e outage
"${compose[@]}" up -d --wait transform index-cpu
"${compose[@]}" restart api transform-consumer index-consumer
"${compose[@]}" run --rm --no-deps api-ready
"${compose[@]}" run --rm --no-deps e2e resumed
# Keep intentional malformed delivery after all empty-queue/DLQ assertions.
# It is never manually acknowledged; isolated cleanup removes the queue.
"${compose[@]}" stop transform-consumer index-consumer
"${compose[@]}" run --rm --no-deps e2e malformed-capture
"${compose[@]}" up -d --no-deps transform-consumer
"${compose[@]}" run --rm --no-deps e2e malformed-transformed
if ! "${compose[@]}" logs --no-color transform-consumer |
    grep -F 'not valid job JSON; left unacknowledged (receive batch size=3)' >/dev/null; then
  echo "Malformed delivery scenario did not exercise a mixed three-message receive batch." >&2
  exit 1
fi
"${compose[@]}" up -d --no-deps index-consumer
"${compose[@]}" run --rm --no-deps e2e malformed-published
