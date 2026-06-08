#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACT_ENV_FILE="${ACT_ENV_FILE:-$ROOT_DIR/.env.act}"
ACT_SECRET_FILE="${ACT_SECRET_FILE:-$ROOT_DIR/.secrets.act}"
ACT_PLATFORM="${ACT_PLATFORM:-ubuntu-latest=catthehacker/ubuntu:act-latest}"
ACT_ARCH="${ACT_ARCH:-linux/amd64}"

usage() {
  cat <<'USAGE'
Usage: scripts/actions-local.sh <command>

Commands:
  list                   List local GitHub Actions jobs act can see.
  ci                     Run the CI workflow test job locally.
  deploy-backend-dryrun  Validate the production backend deploy dispatch locally without executing steps.
  deploy-backend         Run the production backend deploy workflow locally. Requires PUFFERFS_ACT_RUN_DEPLOY=1.
  release-dryrun         Validate the release workflow tag event locally without executing steps.

Optional files:
  .env.act       Local non-secret environment overrides for act. Ignored by git.
  .secrets.act   Local secrets for act. Ignored by git.

Install act:
  brew install act
USAGE
}

require_act() {
  if ! command -v act >/dev/null 2>&1; then
    echo "act is required to run GitHub Actions locally." >&2
    echo "Install with: brew install act" >&2
    exit 127
  fi
}

run_act() {
  local -a args
  args=(
    "--container-architecture" "$ACT_ARCH"
    "-P" "$ACT_PLATFORM"
  )
  if [ -f "$ACT_ENV_FILE" ]; then
    args+=("--env-file" "$ACT_ENV_FILE")
  fi
  if [ -f "$ACT_SECRET_FILE" ]; then
    args+=("--secret-file" "$ACT_SECRET_FILE")
  fi
  (cd "$ROOT_DIR" && act "${args[@]}" "$@")
}

command="${1:-}"
case "$command" in
  list)
    require_act
    run_act --list
    ;;
  ci)
    require_act
    run_act push -W .github/workflows/ci.yml -j test
    ;;
  deploy-backend-dryrun)
    require_act
    run_act workflow_dispatch -W .github/workflows/deploy.yml -j deploy \
      -e .github/act/deploy-backend-production.json \
      --dryrun
    ;;
  deploy-backend)
    require_act
    if [ "${PUFFERFS_ACT_RUN_DEPLOY:-}" != "1" ]; then
      echo "Refusing to run deploy steps locally without PUFFERFS_ACT_RUN_DEPLOY=1." >&2
      echo "Use deploy-backend-dryrun for safe workflow validation." >&2
      exit 2
    fi
    run_act workflow_dispatch -W .github/workflows/deploy.yml -j deploy \
      -e .github/act/deploy-backend-production.json
    ;;
  release-dryrun)
    require_act
    run_act push -W .github/workflows/release.yml -j goreleaser \
      -e .github/act/release-v0.0.0.json \
      --dryrun
    ;;
  -h|--help|help|"")
    usage
    ;;
  *)
    echo "unknown command: $command" >&2
    usage >&2
    exit 64
    ;;
esac
