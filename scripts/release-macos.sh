#!/bin/sh
# Full macOS release, for Apple silicon and Intel alike:
#   dist/mirrin-darwin-arm64, dist/mirrin-darwin-amd64   the CLI, for install.sh and Homebrew
#   dist/mirrin-darwin-<arch>-nowhatsapp                 the MIT CLI, without WhatsApp
#   dist/Mirrin-$VERSION-macos.dmg                       a universal Mirrin.app
#
# Signing and notarization follow the Apple credentials in the environment:
#   APPLE_SIGN_ID       "Developer ID Application: Name (TEAMID)". When it is
#                       empty but such an identity is in the keychain (the
#                       release workflow imports one), that one is used.
#   APPLE_ID, APPLE_TEAM_ID, APPLE_APP_PASSWORD (an app-specific password)
#                       notarize with notarytool. All three, or none.
# With a Developer ID and the notary credentials, the CLIs and the app are
# notarized together, the app is stapled, and the disk image is signed,
# notarized, stapled and checked with Gatekeeper. With a Developer ID only,
# everything is signed but not notarized. With neither, everything is
# ad-hoc signed; the release still works (install.sh downloads carry no
# quarantine flag) but a browser download needs right-click → Open.
set -eu
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}
export VERSION
APPLE_SIGN_ID=${APPLE_SIGN_ID:-}
APPLE_ID=${APPLE_ID:-}
APPLE_TEAM_ID=${APPLE_TEAM_ID:-}
APPLE_APP_PASSWORD=${APPLE_APP_PASSWORD:-}

# say prints a line, and a GitHub Actions annotation when $2 is notice or warning.
say() {
  if [ -n "${GITHUB_ACTIONS:-}" ] && [ -n "${2:-}" ]; then echo "::$2::$1"; else echo "$1"; fi
}

if [ -z "$APPLE_SIGN_ID" ]; then
  APPLE_SIGN_ID=$(security find-identity -v -p codesigning 2>/dev/null |
    sed -n 's/.*"\(Developer ID Application: [^"]*\)".*/\1/p' | head -n 1)
fi
set -- "$APPLE_ID" "$APPLE_TEAM_ID" "$APPLE_APP_PASSWORD"
have=0
for v in "$@"; do [ -n "$v" ] && have=$((have + 1)); done
NOTARIZE=
case "$have" in
  0) ;;
  3) NOTARIZE=1 ;;
  *)
    say "Only some of APPLE_ID, APPLE_TEAM_ID and APPLE_APP_PASSWORD are set; notarizing needs all three." error
    exit 1
    ;;
esac
if [ -n "$NOTARIZE" ] && [ -z "$APPLE_SIGN_ID" ]; then
  say "The notary credentials are set, but there is no Developer ID Application identity to sign with (APPLE_CERT_P12 and APPLE_SIGN_ID), so nothing can be notarized." error
  exit 1
fi
if [ -z "$APPLE_SIGN_ID" ]; then
  say "No Developer ID: the Mac builds are ad-hoc signed and not notarized. See docs/maintainers-release.md to set up signing." notice
elif [ -z "$NOTARIZE" ]; then
  say "Signed with $APPLE_SIGN_ID, but not notarized: APPLE_ID, APPLE_TEAM_ID and APPLE_APP_PASSWORD aren't set." warning
fi
export APPLE_SIGN_ID

make dist-darwin VERSION="$VERSION"

# The CLI runs voice too, so it carries the app's entitlements.
for f in dist/mirrin-darwin-amd64 dist/mirrin-darwin-arm64 dist/mirrin-darwin-amd64-nowhatsapp dist/mirrin-darwin-arm64-nowhatsapp; do
  if [ -n "$APPLE_SIGN_ID" ]; then
    codesign --force --options runtime --timestamp --entitlements packaging/macos/entitlements.plist -s "$APPLE_SIGN_ID" -i com.mirrin.mavrk "$f"
  else
    codesign --force -s - -i com.mirrin.mavrk "$f"
  fi
done

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

lipo -create -output "$TMP/mirrin" dist/mirrin-darwin-amd64 dist/mirrin-darwin-arm64
MIRRIN_BIN=$TMP/mirrin scripts/build-app.sh

# notarize submits a file and waits. notarytool's exit status doesn't say
# whether Apple accepted it, so the status is read from its answer, and the
# log is shown when it was refused.
notarize() {
  xcrun notarytool submit "$1" --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" --password "$APPLE_APP_PASSWORD" \
    --wait --timeout 30m --output-format json >"$TMP/notary.json" || true
  status=$(plutil -extract status raw -o - "$TMP/notary.json" 2>/dev/null || echo unknown)
  if [ "$status" != Accepted ]; then
    id=$(plutil -extract id raw -o - "$TMP/notary.json" 2>/dev/null || true)
    cat "$TMP/notary.json" >&2 || true
    if [ -n "$id" ]; then
      xcrun notarytool log "$id" --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" --password "$APPLE_APP_PASSWORD" >&2 || true
    fi
    say "Apple didn't notarize $(basename "$1") (status: $status); the log is above." error
    exit 1
  fi
  echo "notarized $(basename "$1")"
}

if [ -n "$NOTARIZE" ]; then
  # One submission covers all four CLIs and the app. A bare binary can't carry a
  # stapled ticket, but Gatekeeper finds its notarization online; the app
  # gets its ticket stapled so it opens offline too.
  mkdir -p "$TMP/submit"
  cp dist/mirrin-darwin-amd64 dist/mirrin-darwin-arm64 dist/mirrin-darwin-amd64-nowhatsapp dist/mirrin-darwin-arm64-nowhatsapp "$TMP/submit/"
  ditto dist/Mirrin.app "$TMP/submit/Mirrin.app"
  ditto -c -k --keepParent "$TMP/submit" "$TMP/submit.zip"
  notarize "$TMP/submit.zip"
  xcrun stapler staple dist/Mirrin.app
fi

DMG=dist/Mirrin-$VERSION-macos.dmg
rm -f "$DMG"
hdiutil create -volname Mirrin -srcfolder dist/Mirrin.app -ov -format UDZO "$DMG"
if [ -n "$APPLE_SIGN_ID" ]; then
  codesign --force --timestamp -s "$APPLE_SIGN_ID" "$DMG"
fi
if [ -n "$NOTARIZE" ]; then
  notarize "$DMG"
  xcrun stapler staple "$DMG"
  # What a user's Mac will decide, checked before anything is published.
  xcrun stapler validate "$DMG"
  spctl --assess --type open --context context:primary-signature -vv "$DMG"
  spctl --assess --type execute -vv dist/Mirrin.app
  say "Mirrin.app and the disk image are notarized and stapled, and Gatekeeper accepts them." notice
fi
lipo -archs dist/Mirrin.app/Contents/MacOS/mirrin
echo "release artifacts in dist/"
ls -la dist
