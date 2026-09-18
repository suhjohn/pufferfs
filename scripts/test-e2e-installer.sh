#!/usr/bin/env bash
# Real installer/current CLI, a real published archive, isolated HTTP + container.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
release_version="${PUFFERFS_E2E_RELEASE_VERSION:-0.8.2}"
release_base="${PUFFERFS_E2E_RELEASE_BASE:-https://pufferfs.com/releases}"
fixture="$(mktemp -d)"
project="pufferfs-installer-$$"
export PUFFERFS_E2E_RELEASE_FIXTURE="$fixture"
export PUFFERFS_E2E_RELEASE_BASE="$release_base"
compose=(docker compose --env-file /dev/null -p "$project" -f compose.e2e-installer.yml)
finish() {
  result=$?
  trap - EXIT
  mkdir -p tests/e2e/artifacts
  "${compose[@]}" logs --no-color > tests/e2e/artifacts/installer.log
  "${compose[@]}" down --volumes --remove-orphans
  rm -rf "$fixture"
  exit "$result"
}
trap finish EXIT
curl -fsSL "$release_base/v$release_version/checksums.txt" -o "$fixture/checksums.txt"
python3 scripts/deploy/release-manifest.py --version "$release_version" --minimum 0.7.0 \
  --checksums "$fixture/checksums.txt" --base-url "$release_base" --output "$fixture/manifest.json"
CGO_ENABLED=0 GOOS=linux go build -o "$fixture/pufferfs-current" ./cmd/pufferfs
cp scripts/install.sh "$fixture/install.sh"
printf '%s\n' "$release_version" > "$fixture/expected-version"
"${compose[@]}" up -d --wait release-server
"${compose[@]}" run --rm client
