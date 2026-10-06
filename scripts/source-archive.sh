#!/bin/sh
# Writes the committed source of this checkout, with every Go module it
# depends on (in vendor/), as one .tar.gz. A release attaches it as
# Mirrin-<version>-source.tar.gz, so the programs it publishes come with the
# complete source they were built from, which the GPL asks of programs that
# link GPL code (docs/licensing.md). It builds with no network:
#   GOFLAGS=-mod=vendor go build ./cmd/mirrin
# Usage: scripts/source-archive.sh v0.3.0 dist/Mirrin-v0.3.0-source.tar.gz
set -eu
USAGE="usage: scripts/source-archive.sh <version> <out.tar.gz>"
VERSION=${1:-}
OUT=${2:-}
if [ -z "$VERSION" ] || [ -z "$OUT" ]; then
  echo "$USAGE" >&2
  exit 2
fi
# A relative path means from where the script was run.
case "$OUT" in /*) ;; *) OUT=$(pwd)/$OUT ;; esac
cd "$(dirname "$0")/.."
name=Mirrin-$VERSION-source
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
git archive --format=tar --prefix="$name/" HEAD >"$TMP/source.tar"
go mod vendor -o "$TMP/$name/vendor"
tar -rf "$TMP/source.tar" -C "$TMP" "$name/vendor"
gzip -9n <"$TMP/source.tar" >"$OUT"
echo "wrote $OUT"
