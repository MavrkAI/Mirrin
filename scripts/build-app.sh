#!/bin/sh
# Builds dist/Mirrin.app from the mirrin binary.
# Uses $MIRRIN_BIN when set (the release passes a universal binary), else builds
# for this Mac. Signs with $APPLE_SIGN_ID (a "Developer ID Application: …"
# identity) when set, else with the local mirrin-dev identity if present (or
# antbot-dev, made before the rename), else ad-hoc.
set -e
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
APP=dist/Mirrin.app
rm -rf "$APP"; mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
if [ -n "$MIRRIN_BIN" ]; then
  cp "$MIRRIN_BIN" "$APP/Contents/MacOS/mirrin"
else
  CGO_ENABLED=1 go build -trimpath -tags "${TAGS:-}" -ldflags "-s -w -X main.version=$VERSION" -o "$APP/Contents/MacOS/mirrin" ./cmd/mirrin
fi
sed "s/__VERSION__/$VERSION/g" packaging/macos/Info.plist > "$APP/Contents/Info.plist"
echo "APPL????" > "$APP/Contents/PkgInfo"
if [ -f packaging/macos/Mirrin.icns ]; then cp packaging/macos/Mirrin.icns "$APP/Contents/Resources/"; fi
# The licences travel with the program (`mirrin licenses` prints them too).
cp LICENSE THIRD_PARTY_NOTICES "$APP/Contents/Resources/"
LOCAL_ID=
if [ -z "$APPLE_SIGN_ID" ]; then
  ids=$(security find-identity -v -p codesigning 2>/dev/null || true)
  for n in mirrin-dev antbot-dev; do # rename:keep
    if printf '%s\n' "$ids" | grep -q "\"$n\""; then LOCAL_ID=$n && break; fi
  done
fi
if [ -n "$APPLE_SIGN_ID" ]; then
  codesign --deep --force --options runtime --timestamp --entitlements packaging/macos/entitlements.plist -s "$APPLE_SIGN_ID" "$APP"
  echo "signed with $APPLE_SIGN_ID"
elif [ -n "$LOCAL_ID" ]; then
  codesign --deep --force -s "$LOCAL_ID" -i com.mirrin.mavrk "$APP"
  echo "signed with local $LOCAL_ID identity"
else
  codesign --deep --force -s - -i com.mirrin.mavrk "$APP"
  echo "ad-hoc signed (macOS asks for the microphone again after each rebuild; \`make sign-identity\` makes a lasting identity)"
fi
echo "built $APP"
