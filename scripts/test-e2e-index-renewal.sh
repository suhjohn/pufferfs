#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Required for real provider cleanup}"
: "${TURBOPUFFER_API_KEY:?Required for real search}"
project="pufferfs-index-renewal-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-api.yml)
mkdir -p tests/e2e/artifacts
runner_started=0
finish() {
  result=$?
  trap - EXIT
  "${compose[@]}" start --wait postgres || true
  "${compose[@]}" stop transform-consumer index-consumer transform index-cpu reconciler index-relay || true
  if [[ "$runner_started" == 1 ]] && ! "${compose[@]}" run --rm --no-deps e2e cleanup; then
    echo "Cleanup failed; retained project $project for recovery." >&2
    exit 1
  fi
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/index-renewal.log
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build api transform e2e
"${compose[@]}" up -d --wait --scale api=2 postgres aws api api-ready transform index-cpu reconciler
runner_started=1
"${compose[@]}" up -d --no-deps transform-consumer index-consumer
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/index_renewal.py)
"${driver[@]}" capture
started=$(docker inspect --format '{{.State.StartedAt}}' "$("${compose[@]}" ps -q index-cpu)")
"${compose[@]}" stop postgres
# The production heartbeat period is 60s and pool acquisition timeout is 30s.
# Keep the real provider response held while both deadlines actually elapse.
sleep 50
sleep 50
"${compose[@]}" start --wait postgres
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/index_checkpoints.py release
"${driver[@]}" verify
test "$started" = "$(docker inspect --format '{{.State.StartedAt}}' "$("${compose[@]}" ps -q index-cpu)")"
