#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Required for real provider cleanup}"
: "${TURBOPUFFER_API_KEY:?Required for real search}"
project="pufferfs-api-access-$$"
compose=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml -f compose.e2e-api.yml)
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
  "${compose[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/api-access.log
  "${compose[@]}" down --volumes --remove-orphans
  exit "$result"
}
trap finish EXIT
"${compose[@]}" build api transform e2e
"${compose[@]}" up -d --wait --scale api=2 postgres aws api api-ready transform index-cpu reconciler
runner_started=1
"${compose[@]}" up -d --no-deps transform-consumer index-consumer
driver=("${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_access.py)
"${driver[@]}" verify
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_search.py verify
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_keys.py verify
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_groups.py verify
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_reads.py
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_membership.py
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/browser_session.py verify
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_roots.py verify
"${compose[@]}" restart api
"${compose[@]}" run --rm --no-deps api-ready
"${driver[@]}" restarted
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_search.py restarted
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_keys.py restarted
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_groups.py restarted
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/browser_session.py restarted
"${compose[@]}" run --rm --no-deps --entrypoint python e2e /e2e/api_roots.py restarted
