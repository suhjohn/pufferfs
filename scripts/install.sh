#!/bin/sh
set -eu

REPO="${PUFFERFS_REPO:-suhjohn/pufferfs}"
MANIFEST_URL="${PUFFERFS_MANIFEST_URL:-https://api.pufferfs.com/cli/version}"
DOWNLOAD_BASE_URL="${PUFFERFS_DOWNLOAD_BASE_URL:-https://github.com/${REPO}/releases/download}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

fail() {
  echo "pufferfs install: $*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

need curl
need tar

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  darwin|linux) ;;
  *) fail "unsupported OS: $OS" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) fail "unsupported architecture: $ARCH" ;;
esac

latest_from_manifest() {
  curl -fsSL "$MANIFEST_URL" 2>/dev/null |
    sed -n 's/.*"latest"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
    head -n 1
}

latest_from_github() {
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
    head -n 1
}

VERSION="${PUFFERFS_VERSION:-}"
if [ -z "$VERSION" ]; then
  VERSION="$(latest_from_manifest || true)"
fi
if [ -z "$VERSION" ] || [ "$VERSION" = "dev" ]; then
  VERSION="$(latest_from_github || true)"
fi
[ -n "$VERSION" ] || fail "could not determine latest release version"

VERSION="${VERSION#v}"
TAG="v${VERSION}"
ARCHIVE="pufferfs_${VERSION}_${OS}_${ARCH}.tar.gz"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT INT TERM

ARCHIVE_URL="${DOWNLOAD_BASE_URL}/${TAG}/${ARCHIVE}"
CHECKSUM_URL="${DOWNLOAD_BASE_URL}/${TAG}/checksums.txt"

echo "Downloading ${ARCHIVE_URL}"
curl -fL "$ARCHIVE_URL" -o "$TMPDIR/$ARCHIVE"
curl -fsSL "$CHECKSUM_URL" -o "$TMPDIR/checksums.txt"

EXPECTED="$(grep "[[:space:]]${ARCHIVE}\$" "$TMPDIR/checksums.txt" | awk '{print $1}' | head -n 1)"
[ -n "$EXPECTED" ] || fail "checksum for ${ARCHIVE} not found"

if command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "$TMPDIR/$ARCHIVE" | awk '{print $1}')"
elif command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "$TMPDIR/$ARCHIVE" | awk '{print $1}')"
else
  fail "missing shasum or sha256sum"
fi
[ "$ACTUAL" = "$EXPECTED" ] || fail "checksum mismatch"

tar -xzf "$TMPDIR/$ARCHIVE" -C "$TMPDIR"
[ -x "$TMPDIR/pufferfs" ] || fail "archive did not contain pufferfs binary"

mkdir -p "$INSTALL_DIR" 2>/dev/null || true
if [ -w "$INSTALL_DIR" ]; then
  install -m 755 "$TMPDIR/pufferfs" "$INSTALL_DIR/pufferfs"
else
  command -v sudo >/dev/null 2>&1 || fail "${INSTALL_DIR} is not writable and sudo is unavailable"
  sudo install -m 755 "$TMPDIR/pufferfs" "$INSTALL_DIR/pufferfs"
fi

echo "Installed pufferfs ${TAG} to ${INSTALL_DIR}/pufferfs"
"$INSTALL_DIR/pufferfs" --version
