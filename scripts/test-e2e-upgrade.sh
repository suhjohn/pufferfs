#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
: "${GEMINI_API_KEY:?Real provider key required}"
: "${TURBOPUFFER_API_KEY:?Real provider key required}"
repository="$PWD"
project="pufferfs-upgrade-$$"
previous="$(mktemp -d)"
git archive v0.8.2 | tar -x -C "$previous"
# Preserve old production code; only configure the supported one-namespace topology.
python3 - "$previous" "$repository" <<'PY'
import json,sys
from pathlib import Path
old,repo=map(Path,sys.argv[1:])
services={name:{'environment':{'PUFFERFS_TP_NAMESPACE_SHARDS':'1'}} for name in
    ('api','transform','index','collector','reconciler','transform-consumer','index-consumer')}
services['e2e']={'build':{'context':str(repo),'dockerfile':'tests/e2e/Dockerfile','target':'runner'},
    'volumes':[str(repo/'tests/e2e/artifacts')+':/artifacts','e2e-state:/state','e2e-cli:/root/.tpfs',
               str(old/'old-pufferfs')+':/usr/local/bin/pufferfs:ro']}
(old/'upgrade.json').write_text(json.dumps({'services':services}))
PY
old=(docker compose --env-file /dev/null --profile test -p "$project" -f "$previous/compose.e2e.yml" -f "$previous/upgrade.json")
new=(docker compose --env-file /dev/null --profile test -p "$project" -f compose.e2e.yml)
active=old
started=0
mkdir -p tests/e2e/artifacts
finish() {
  result=$?
  trap - EXIT
  "${old[@]}" stop transform-consumer index-consumer transform index collector reconciler || true
  "${new[@]}" stop ingestion background || true
  if [[ "$started" == 1 ]] && ! "${new[@]}" run --rm --no-deps e2e cleanup; then
    echo "Cleanup failed; retained project $project and old context $previous." >&2
    exit 1
  fi
  "${new[@]}" logs --no-color | python3 tests/e2e/redact.py > tests/e2e/artifacts/upgrade.log
  "${new[@]}" down --volumes --remove-orphans
  rm -rf "$previous"
  exit "$result"
}
trap finish EXIT
"${old[@]}" build
# Exercise the old client/server contract before upgrading both. The current
# client uses the new incremental catalog API, absent from the old release.
docker build -f "$previous/tests/e2e/Dockerfile" --target cli -t "$project-old-cli" "$previous"
binary_container="$(docker create "$project-old-cli")"
docker cp "$binary_container:/pufferfs" "$previous/old-pufferfs"
docker rm "$binary_container"
"${old[@]}" up -d --wait postgres aws api api-ready transform index collector reconciler transform-consumer index-consumer
started=1
driver=("${old[@]}" run --rm --no-deps --entrypoint python e2e /e2e/upgrade.py)
"${driver[@]}" baseline
"${old[@]}" stop index-consumer index collector reconciler
"${driver[@]}" pending
"${old[@]}" stop transform-consumer transform api
# Keep the actual Postgres and S3 processes/data. Only application roles change.
"${new[@]}" build api ingestion e2e
driver=("${new[@]}" run --rm --no-deps --entrypoint python e2e /e2e/upgrade.py)
"${new[@]}" run --rm --no-deps aws-init
"${new[@]}" up -d --no-deps api
"${new[@]}" run --rm --no-deps api-ready
"${driver[@]}" migrated
"${new[@]}" up -d --no-deps --wait ingestion background
"${driver[@]}" verified
