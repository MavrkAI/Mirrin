#!/bin/sh
# Checks that the stock age tool opens a Mirrin backup (docs/backup-format.md,
# "Check it yourself with stock age"): builds age at the version go.mod pins,
# runs TestStockAgeDecryptsASnapshot with it, and checks the test really used
# that binary rather than the age library.
# Usage: scripts/verify-backup-with-age.sh
set -eu
cd "$(dirname "$0")/.."
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT TERM

VERSION=$(go list -m -f '{{.Version}}' filippo.io/age)
GOBIN="$TMP/bin" go install "filippo.io/age/cmd/age@$VERSION"
AGE="$TMP/bin/age"
echo "stock age: $("$AGE" --version)"

# The test makes its own twin in a temporary folder; MIRRIN_HOME keeps
# anything else away from a real ~/.mirrin.
if ! OUT=$(MIRRIN_HOME="$TMP/home" AGE_BIN="$AGE" go test ./internal/backup \
	-run '^TestStockAgeDecryptsASnapshot$' -count=1 -v 2>&1); then
	echo "$OUT"
	exit 1
fi
echo "$OUT"
case $OUT in
*"decrypted with $AGE"*)
	echo "OK: stock age $VERSION opens a Mirrin backup."
	;;
*)
	echo "The test didn't use the stock age binary." >&2
	exit 1
	;;
esac
