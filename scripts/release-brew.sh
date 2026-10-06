#!/bin/sh
# Fill packaging/homebrew/mirrin.rb with a release's version and checksums.
# The checksums come from the release's SHA256SUMS: a local copy (the release
# workflow passes the one its publish job wrote) or the one published with the
# release.
# Usage: VERSION=v0.3.0 [SUMS=dist/SHA256SUMS] scripts/release-brew.sh > Formula/mirrin.rb
set -eu
V=${VERSION:-}
V=${V#v}
[ -n "$V" ] || { echo "release-brew: set VERSION, e.g. VERSION=v0.3.0" >&2; exit 1; }
SUMS=${SUMS:-}
# A relative SUMS means from where the script was run, not the repository root.
case "$SUMS" in "" | /*) ;; *) SUMS=$(pwd)/$SUMS ;; esac
cd "$(dirname "$0")/.."
if [ -z "$SUMS" ]; then
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  SUMS=$TMP/SHA256SUMS
  URL="https://github.com/${MIRRIN_REPO:-${ANTBOT_REPO:-MavrkAI/Mirrin}}/releases/download/v$V/SHA256SUMS" # rename:keep
  curl -fsSL "$URL" -o "$SUMS" || { echo "release-brew: could not download $URL" >&2; exit 1; }
fi

# sha prints the checksum listed for one asset, or stops the script.
sha() {
  s=$(awk -v f="$1" '$2 == f || $2 == "*" f { print $1; exit }' "$SUMS")
  if ! printf '%s' "$s" | grep -Eq '^[0-9a-f]{64}$' || [ "$s" = e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 ]; then
    echo "release-brew: $SUMS has no usable checksum for $1; is it in the v$V release?" >&2
    return 1
  fi
  printf '%s' "$s"
}

# Plain assignments, so a missing asset stops the script under set -e.
DARWIN_ARM64=$(sha mirrin-darwin-arm64)
DARWIN_AMD64=$(sha mirrin-darwin-amd64)
LINUX_ARM64=$(sha mirrin-linux-arm64)
LINUX_AMD64=$(sha mirrin-linux-amd64)

sed -e "s/VERSION/$V/g" \
    -e "s/SHA_DARWIN_ARM64/$DARWIN_ARM64/" -e "s/SHA_DARWIN_AMD64/$DARWIN_AMD64/" \
    -e "s/SHA_LINUX_ARM64/$LINUX_ARM64/" -e "s/SHA_LINUX_AMD64/$LINUX_AMD64/" \
    packaging/homebrew/mirrin.rb
