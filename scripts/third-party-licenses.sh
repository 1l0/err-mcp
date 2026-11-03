#!/usr/bin/env bash
#
# Write THIRD_PARTY_LICENSES.txt with the full license text of every Go module
# linked into err-mcp.
#
# Usage:
#   scripts/third-party-licenses.sh [output-file]
#
# It reads the license files from the module cache, so run it after
# "go mod download" (or any build). A module without a license file is an
# error: shipping a binary without its license text is not acceptable.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-$repo_root/dist/THIRD_PARTY_LICENSES.txt}"
mkdir -p "$(dirname "$out")"

cd "$repo_root"
{
  echo "Third-party licenses for err-mcp"
  echo "Generated from go.mod by scripts/third-party-licenses.sh"
  echo
} >"$out"

status=0
modules="$(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' . | sort -u)"
for path in $modules; do
  dir="$(go list -m -f '{{.Dir}}' "$path" 2>/dev/null || true)"
  if [ -z "$dir" ] || [ ! -d "$dir" ]; then
    continue
  fi
  found=""
  for candidate in "$dir"/LICENSE* "$dir"/COPYING* "$dir"/NOTICE* "$dir"/UNLICENSE*; do
    [ -f "$candidate" ] || continue
    found="$candidate"
    {
      echo "=============================================================================="
      echo "MODULE: $path"
      echo "FILE:   $(basename "$candidate")"
      echo "=============================================================================="
      echo
      cat "$candidate"
      echo
    } >>"$out"
  done
  if [ -z "$found" ]; then
    echo "no license file found for module $path ($dir)" >&2
    status=1
  fi
done

if [ "$status" -ne 0 ]; then
  echo "one or more modules have no discoverable license file" >&2
  exit 1
fi

echo "wrote $out"
