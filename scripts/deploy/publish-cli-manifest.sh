#!/usr/bin/env bash
# Run from infra/pulumi; archives are verified before advertising the release.
set -euo pipefail
VERSION="${CLI_RELEASE_VERSION:-}"
VERSION="${VERSION#v}"
if [ -z "$VERSION" ]; then VERSION="$(gh release view --repo "$GITHUB_REPOSITORY" --json tagName --jq .tagName)"; VERSION="${VERSION#v}"; fi
TAG="v${VERSION}"
release_dir="$(mktemp -d)"
trap 'rm -rf "$release_dir"' EXIT
mkdir "$release_dir/assets"
gh release download "$TAG" --repo "$GITHUB_REPOSITORY" --dir "$release_dir/assets" \
  --pattern 'checksums.txt' \
  --pattern "pufferfs_${VERSION}_*.tar.gz"
test -s "$release_dir/assets/checksums.txt"
for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
  test -s "$release_dir/assets/pufferfs_${VERSION}_${target}.tar.gz"
done
(cd "$release_dir/assets" && sha256sum --check --ignore-missing checksums.txt)
WEB_BUCKET="$(pulumi stack output webBucketName)"
WEB_DISTRIBUTION="$(pulumi stack output webDistributionId)"
WEB_URL="$(pulumi stack output webUrl)"
python3 ../../scripts/deploy/release-manifest.py --version "$VERSION" \
  --checksums "$release_dir/assets/checksums.txt" --output "$release_dir/manifest.json" \
  --base-url "${WEB_URL%/}/releases" \
  --minimum "${PUFFERFS_CLI_MIN_VERSION:-0.7.0}"
aws s3 sync "$release_dir/assets/" "s3://${WEB_BUCKET}/releases/${TAG}/" \
  --delete \
  --cache-control 'public, max-age=31536000, immutable'
aws s3 cp "$release_dir/manifest.json" "s3://${WEB_BUCKET}/releases/manifest.json" \
  --content-type 'application/json' --cache-control 'public, max-age=60'
aws cloudfront create-invalidation --distribution-id "$WEB_DISTRIBUTION" --paths '/releases/manifest.json'
