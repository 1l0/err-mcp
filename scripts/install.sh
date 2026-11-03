#!/usr/bin/env bash
#
# Download and install a released err-mcp tarball.
#
# Usage:
#   scripts/install.sh             # latest release
#   VERSION=v0.10.1 scripts/install.sh
#   PREFIX=/usr/local scripts/install.sh
#
# The checksum in the release's SHA256SUMS is verified before installing.
set -euo pipefail

repo="${ERR_MCP_REPO:-1l0/err-mcp}"
version="${VERSION:-latest}"
prefix="${PREFIX:-$HOME/.local}"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "unsupported OS: $(uname -s); use 'go install github.com/1l0/err-mcp@latest'" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$version" = "latest" ]; then
  version="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
  if [ -z "$version" ]; then
    echo "cannot determine the latest release; pass VERSION=..." >&2
    exit 1
  fi
fi

asset="err-mcp_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "downloading $asset"
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"

if [ "$os" = "darwin" ] && command -v shasum >/dev/null 2>&1; then
  sha_check() { shasum -a 256 "$1" | awk '{print $1}'; }
elif command -v sha256sum >/dev/null 2>&1; then
  sha_check() { sha256sum "$1" | awk '{print $1}'; }
else
  sha_check() { shasum -a 256 "$1" | awk '{print $1}'; }
fi

# Older releases list checksums as "./name", so strip any leading ./ here.
expected="$(awk -v f="$asset" '{ n = $2; sub(/^\.\//, "", n) } n == f { print $1 }' "$tmp/SHA256SUMS")"
if [ -z "$expected" ]; then
  echo "no checksum for $asset in SHA256SUMS" >&2
  exit 1
fi
actual="$(sha_check "$tmp/$asset")"
if [ "$expected" != "$actual" ]; then
  echo "checksum mismatch for $asset" >&2
  exit 1
fi

mkdir -p "$tmp/extract"
tar -C "$tmp/extract" -xzf "$tmp/$asset"
src="$(find "$tmp/extract" -maxdepth 2 -type f -name err-mcp | head -1)"
if [ -z "$src" ]; then
  echo "err-mcp not found in $asset" >&2
  exit 1
fi

mkdir -p "$prefix/bin"
install -m 0755 "$src" "$prefix/bin/err-mcp"
echo "installed err-mcp to $prefix/bin"
