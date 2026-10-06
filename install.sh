#!/bin/sh
# Mirrin installer for macOS and Linux:
#   curl -fsSL https://raw.githubusercontent.com/MavrkAI/Mirrin/main/install.sh | sh
# Downloads the mirrin CLI for this machine from a GitHub release, checks it
# against the release's SHA256SUMS before installing anything, and on macOS
# adds Mirrin.app. On Windows, use install.ps1 instead.
#
# Optional settings (environment variables):
#   MIRRIN_BUILD=nowhatsapp   MIT without WhatsApp (default: GPL-3.0 with WhatsApp)
#   MIRRIN_VERSION=v0.3.0      a specific release instead of the latest
#   MIRRIN_BIN_DIR=DIR         where the CLI goes. By default an upgrade replaces the
#                              mirrin already installed, and a first install goes to
#                              ~/.local/bin, or to /usr/local/bin when only that one
#                              is on PATH and writable
#   XDG_BIN_HOME=DIR           used in place of ~/.local/bin
#   MIRRIN_NO_APP=1            macOS: skip Mirrin.app
#   MIRRIN_APPS_DIR=DIR        macOS: where Mirrin.app goes (default /Applications,
#                              or ~/Applications when that isn't writable)
#   MIRRIN_NO_MODIFY_PATH=1    never offer to add the CLI folder to your shell profile
#   MIRRIN_REPO=owner/name     install from a fork
#   MIRRIN_DOWNLOAD_URL=URL    where the releases live (a mirror)
# The same settings named ANTBOT_..., from before Mirrin was renamed, still
# work; the MIRRIN_ name wins when both are set.
set -eu

MIRRIN_BUILD=${MIRRIN_BUILD:-${ANTBOT_BUILD:-}}                   # rename:keep
MIRRIN_VERSION=${MIRRIN_VERSION:-${ANTBOT_VERSION:-}}             # rename:keep
MIRRIN_BIN_DIR=${MIRRIN_BIN_DIR:-${ANTBOT_BIN_DIR:-}}             # rename:keep
MIRRIN_NO_APP=${MIRRIN_NO_APP:-${ANTBOT_NO_APP:-}}                # rename:keep
MIRRIN_APPS_DIR=${MIRRIN_APPS_DIR:-${ANTBOT_APPS_DIR:-}}          # rename:keep
MIRRIN_NO_MODIFY_PATH=${MIRRIN_NO_MODIFY_PATH:-${ANTBOT_NO_MODIFY_PATH:-}} # rename:keep
MIRRIN_REPO=${MIRRIN_REPO:-${ANTBOT_REPO:-}}                      # rename:keep
MIRRIN_DOWNLOAD_URL=${MIRRIN_DOWNLOAD_URL:-${ANTBOT_DOWNLOAD_URL:-}} # rename:keep

REPO=${MIRRIN_REPO:-MavrkAI/Mirrin}
RELEASES=${MIRRIN_DOWNLOAD_URL:-https://github.com/$REPO/releases}
# @main until the first Mirrin release is tagged: @latest is still v0.2.0,
# from before the module had this name.
FROM_SOURCE="go install github.com/$REPO/cmd/mirrin@main"
ISSUES="https://github.com/$REPO/issues"
HOST=${RELEASES#*://}
HOST=${HOST%%/*}
LOCAL_BIN=${XDG_BIN_HOME:-$HOME/.local/bin}
CODE=
SERVICE=
TMP=
MNT=
NEW=
WHY=

say() { printf '%s\n' "$*"; }
fail() {
  printf 'mirrin install: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [ -n "$MNT" ]; then hdiutil detach "$MNT" -quiet 2>/dev/null || true; fi
  if [ -n "$NEW" ]; then rm -f "$NEW"; fi
  if [ -n "$TMP" ]; then rm -rf "$TMP"; fi
}

# platform prints this machine the way release files name it: darwin/arm64, linux/amd64, ...
platform() {
  kernel=$(uname -s)
  case "$kernel" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    MINGW* | MSYS* | CYGWIN* | Windows_NT)
      fail "this looks like Windows. Open PowerShell and run:
  irm https://raw.githubusercontent.com/$REPO/main/install.ps1 | iex" ;;
    *) fail "there's no Mirrin build for $kernel yet. You can build it from source: $FROM_SOURCE" ;;
  esac
  machine=$(uname -m)
  case "$machine" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64 | armv8*) arch=arm64 ;;
    *) fail "there's no Mirrin build for $os on $machine yet. You can build it from source: $FROM_SOURCE" ;;
  esac
  # A shell running under Rosetta says x86_64 on Apple silicon; the native build is the right one.
  if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
    arch=arm64
  fi
  say "$os/$arch"
}

# latest sets VERSION to the newest release tag. The releases/latest page
# redirects to it, which needs no API token and has no rate limit.
latest() {
  out=$(curl -fsLI -o /dev/null -w '%{http_code} %{url_effective}' "$RELEASES/latest") || true
  CODE=${out%% *}
  tag=${out#* }
  tag=${tag%/}
  tag=${tag##*/}
  case "$CODE" in 2??) ;; *) return 1 ;; esac
  case "$tag" in "" | latest | releases) return 1 ;; esac
  VERSION=$tag
}

# download saves URL $1 as $2 and returns curl's exit status. CODE is the HTTP
# status of the answer, or 000 when nothing answered.
download() {
  CODE=$(curl -fsL --retry 2 -o "$2" -w '%{http_code}' "$1")
}

# trouble explains a download of $1 that failed for some reason other than the
# file not existing; $2 is curl's exit status.
trouble() {
  case "$CODE" in
    000 | "") say "couldn't reach $HOST for $1. Check your internet connection and try again." ;;
    2??) say "$1 didn't finish downloading (curl error $2). Try again." ;;
    *) say "$HOST answered HTTP $CODE for $1, so it may be busy or limiting downloads. Try again in a minute." ;;
  esac
}

# fetch downloads URL $1 as $2, or stops: with $3 when the release has no such
# file, and with what went wrong otherwise.
fetch() {
  status=0
  download "$1" "$2" || status=$?
  [ "$status" -eq 0 ] && return 0
  [ "$CODE" = 404 ] && fail "$3"
  fail "$(trouble "${1##*/}" "$status")"
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | awk '{ print $NF }'
  else
    return 1
  fi
}

# checksum_ok reports whether FILE matches NAME's line in SHA256SUMS; WHY says why not.
checksum_ok() {
  want=$(awk -v f="$2" '$2 == f || $2 == "*" f { print $1; exit }' "$TMP/SHA256SUMS")
  if [ -z "$want" ]; then
    WHY="the $VERSION checksum list doesn't include $2, so it can't be checked"
    return 1
  fi
  if ! got=$(sha256 "$1"); then
    WHY="there's no sha256sum, shasum or openssl here to check downloads with"
    return 1
  fi
  if [ "$got" != "$want" ]; then
    WHY="$2 doesn't match its published checksum, so it may be damaged or tampered with"
    return 1
  fi
}

# verify_signature checks SHA256SUMS against its Sigstore signature when
# cosign is installed. Every release since checksums began is signed, so a
# missing signature is refused like a bad one.
verify_signature() {
  command -v cosign >/dev/null 2>&1 || return 0
  fetch "$BASE/SHA256SUMS.sigstore.json" "$TMP/SHA256SUMS.sigstore.json" \
    "the $VERSION checksum list has no Sigstore signature (SHA256SUMS.sigstore.json), so nothing was installed.
Every Mirrin release is signed, so this may mean someone changed the release. Please report it: $ISSUES"
  if ! err=$(cosign verify-blob --bundle "$TMP/SHA256SUMS.sigstore.json" \
    --certificate-identity "https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$VERSION" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "$TMP/SHA256SUMS" 2>&1 >/dev/null); then
    # cosign's last line says why: a bad signature, or no answer from Sigstore.
    why=$(printf '%s\n' "$err" | awk 'NF { last = $0 } END { print last }')
    fail "the Sigstore signature on the $VERSION checksum list didn't verify with cosign, so nothing was installed.
cosign said: ${why:-nothing}
If your cosign is older than 2.4, update it and try again; if it still fails, please report it: $ISSUES"
  fi
  say "signature verified with cosign (Sigstore)"
}

# verify_attestation checks FILE against GitHub's build attestation when the
# GitHub CLI is installed and signed in. It's an extra check on top of the
# checksum, so it never stops the install: it only says what it found.
verify_attestation() {
  if ! command -v gh >/dev/null 2>&1; then
    say "note: with the GitHub CLI (gh) installed and signed in, the installer also checks GitHub's build attestation."
    return 0
  fi
  if ! gh auth status >/dev/null 2>&1 </dev/null; then
    say "note: sign in to the GitHub CLI (gh auth login) and the installer also checks GitHub's build attestation."
    return 0
  fi
  if gh attestation verify "$1" -R "$REPO" --signer-workflow "$REPO/.github/workflows/release.yml" >/dev/null 2>&1 </dev/null; then
    say "build attestation verified with gh"
  else
    say "note: gh couldn't verify GitHub's build attestation for $2 (the checksum matched, so the install goes on)."
  fi
  return 0
}

# unquarantine clears macOS Gatekeeper's quarantine flag, and says so, when one
# is set. A notarized Mirrin.app keeps it: Gatekeeper opens the app normally
# and goes on checking it. Gatekeeper's assessment only accepts apps (it
# rejects any bare command-line program, notarized or not), so the CLI's flag
# is cleared either way; its checksum matched. curl doesn't set the flag, so
# this is only a safeguard.
unquarantine() {
  xattr -p com.apple.quarantine "$1" >/dev/null 2>&1 || return 0
  case "$1" in
    *.app)
      if spctl --assess --type execute "$1" >/dev/null 2>&1; then
        say "$1 is notarized by Apple, so it keeps its quarantine flag and opens normally"
        return 0
      fi
      say "clearing the macOS quarantine flag on $1, which isn't notarized; its checksum matched"
      ;;
    *) say "clearing the macOS quarantine flag on $1; its checksum matched" ;;
  esac
  xattr -dr com.apple.quarantine "$1"
}

# bin_dir picks where the CLI goes. An upgrade replaces the mirrin already
# installed, because a service keeps running the path it was installed from,
# unless that one is a link (Homebrew's are) or inside an app. A first install
# goes to the first of ~/.local/bin and /usr/local/bin that is on PATH and
# writable, else ~/.local/bin.
bin_dir() {
  if [ -n "${MIRRIN_BIN_DIR:-}" ]; then
    say "$MIRRIN_BIN_DIR"
    return
  fi
  for f in "$(command -v mirrin 2>/dev/null || true)" "$LOCAL_BIN/mirrin"; do
    case "$f" in *.app/*) continue ;; /?*) ;; *) continue ;; esac
    if [ -f "$f" ] && [ ! -L "$f" ] && [ -w "${f%/*}" ]; then
      say "${f%/*}"
      return
    fi
  done
  for d in "$LOCAL_BIN" /usr/local/bin; do
    case ":$PATH:" in
      *":$d:"*) if [ -d "$d" ] && [ -w "$d" ] && [ ! -L "$d/mirrin" ]; then say "$d" && return; fi ;;
    esac
  done
  say "$LOCAL_BIN"
}

# ask reads a yes/no from the terminal, which works even when this script is piped into sh.
ask() {
  (: </dev/tty) 2>/dev/null || return 1
  printf '%s [Y/n] ' "$1" >/dev/tty
  read -r answer </dev/tty || return 1
  case "$answer" in "" | y | Y | yes | Yes) return 0 ;; esac
  return 1
}

# shell_profile prints the startup file, under home folder $1, where the
# user's shell would pick up a PATH line.
shell_profile() {
  case "${SHELL:-sh}" in
    */zsh) say "${ZDOTDIR:-$1}/.zshrc" ;;
    */bash)
      if [ "$OS" != darwin ]; then
        say "$1/.bashrc"
        return
      fi
      # Terminal starts login shells, and bash reads only the first of these
      # that exists: creating .bash_profile would hide an existing .profile.
      for f in .bash_profile .bash_login .profile; do
        if [ -f "$1/$f" ]; then say "$1/$f" && return; fi
      done
      say "$1/.bash_profile"
      ;;
    */fish) say "$1/.config/fish/config.fish" ;;
    *) say "$1/.profile" ;;
  esac
}

# path_note makes sure the next `mirrin` typed finds the CLI, or says how.
path_note() {
  case ":$PATH:" in *":$DEST:"*) return 0 ;; esac
  line="export PATH=\"$DEST:\$PATH\""
  case "${SHELL:-sh}" in */fish) line="fish_add_path $DEST" ;; esac
  rc=$(shell_profile "$HOME")
  if [ -f "$rc" ] && grep -Fq "$DEST" "$rc"; then
    say "$DEST is already on your PATH in $rc; open a new terminal to use mirrin."
  elif [ -z "${MIRRIN_NO_MODIFY_PATH:-}" ] && ask "Add $DEST to your PATH in $rc?"; then
    mkdir -p "$(dirname "$rc")"
    printf '\n# Added by the Mirrin installer\n%s\n' "$line" >>"$rc"
    say "added $DEST to your PATH in $rc; open a new terminal to use mirrin."
  else
    say "$DEST isn't on your PATH yet. Add this line to $rc:"
    say "  $line"
  fi
}

# old_service_note says how a service from before the rename, set up for
# AntBot ($1 = antbot) or openHuman ($1 = openhuman), is turned off: it would
# otherwise keep relaunching the old program beside Mirrin. mirrin retires
# it itself (service.RetireLegacy), moving the keys its definition holds
# into the secrets file first, so the definition is never deleted here: it
# may be the only copy of those keys. Only stopping it isn't advised: its
# definition starts it again at the next login, and until that is gone the
# twin's home stays in its old folder.
old_service_note() {
  case "$1" in
    antbot) who="AntBot (Mirrin's name before)" ;; # rename:keep
    *) who="openHuman (Mirrin's first name)" ;;
  esac
  if [ "$OS" = darwin ]; then
    old=$HOME/Library/LaunchAgents/$1.plist
    [ -f "$old" ] || launchctl print "gui/$uid/$1" >/dev/null 2>&1 || return 0
  else
    old=$HOME/.config/systemd/user/$1.service
    [ -f "$old" ] || systemctl --user cat "$1.service" >/dev/null 2>&1 || return 0
  fi
  say "$who is still set up as a service and restarts itself whenever it stops."
  say "Run \`mirrin service install\` to turn it off for good (it keeps the keys in $old first) and run Mirrin in the background in its place. Stopping it alone isn't enough: it starts again at your next login."
}

# service_note says how to put a running Mirrin service on the new version,
# and how a service from before the rename is turned off.
service_note() {
  uid=$(id -u)
  if [ "$OS" = darwin ]; then
    running=$(launchctl print "gui/$uid/mirrin" 2>/dev/null || true)
  else
    running=$(systemctl --user cat mirrin.service 2>/dev/null || true)
  fi
  if [ -n "$running" ]; then
    SERVICE=1
    if [ "$BUILD" = nowhatsapp ] && printf '%s' "$running" | grep -Fq '/Mirrin.app/'; then
      say "The service still runs Mirrin.app with WhatsApp. To use the MIT CLI, reinstall the service from $DEST/mirrin:"
      say "  \"$DEST/mirrin\" service uninstall && \"$DEST/mirrin\" service install"
    else
      case "$running" in
        *"$DEST/mirrin"* | *"/Applications/Mirrin.app/"*)
          say "Mirrin runs as a service here; restart it to use $VERSION:"
          say "  mirrin service restart"
          ;;
        *)
          say "The Mirrin service runs a mirrin from another folder. To run this one instead:"
          say "  mirrin service uninstall && mirrin service install"
          ;;
      esac
    fi
  fi
  for name in antbot openhuman; do # rename:keep
    old_service_note "$name"
  done
}

# old_install_note points out the program and app from before the rename,
# which stay: a service from then may still run them. Only a program that
# answers as AntBot did ("antbot v0.3.0" or "antbot dev") counts; another
# product has the same name.
old_install_note() {
  old=$(command -v antbot 2>/dev/null || true) # rename:keep
  case "$old" in /?*) ;; *) old= ;; esac
  if [ -n "$old" ] && "$old" version </dev/null 2>/dev/null | head -n 1 | grep -Eq '^antbot (v|dev)'; then # rename:keep
    say "note: the old AntBot program is still at $old. Mirrin doesn't use it; once \`mirrin service install\` has moved the background service to Mirrin, you can delete it." # rename:keep
  fi
  [ "$OS" = darwin ] || return 0
  for app in /Applications/AntBot.app "$HOME/Applications/AntBot.app"; do # rename:keep
    if [ -d "$app" ] && [ "$(plutil -extract CFBundleIdentifier raw -o - "$app/Contents/Info.plist" 2>/dev/null || true)" = com.antbot.mavrk ]; then # rename:keep
      say "note: $app is the old AntBot app. Mirrin.app takes its place; once \`mirrin service install\` has moved the background service to Mirrin, you can move it to the Trash." # rename:keep
    fi
  done
}

install_app() {
  dmg=Mirrin-$VERSION-macos.dmg
  if ! awk -v f="$dmg" '$2 == f { found = 1 } END { exit !found }' "$TMP/SHA256SUMS"; then
    say "(this release has no Mirrin.app; \`mirrin tray\` runs the same menu bar app)"
    return 0
  fi
  say "downloading Mirrin.app"
  if ! download "$BASE/$dmg" "$TMP/$dmg"; then
    say "couldn't download Mirrin.app, so it was skipped. The CLI is installed; \`mirrin tray\` runs the menu bar app."
    return 0
  fi
  if ! checksum_ok "$TMP/$dmg" "$dmg"; then
    printf 'mirrin install: %s, so Mirrin.app was skipped. The CLI is installed.\n' "$WHY" >&2
    return 0
  fi
  apps=${MIRRIN_APPS_DIR:-/Applications}
  [ -n "${MIRRIN_APPS_DIR:-}" ] || [ -w "$apps" ] || apps=$HOME/Applications
  mkdir -p "$apps" "$TMP/mnt"
  if ! hdiutil attach "$TMP/$dmg" -nobrowse -readonly -quiet -mountpoint "$TMP/mnt"; then
    say "couldn't open the Mirrin disk image, so the app was skipped. The CLI is installed."
    return 0
  fi
  MNT=$TMP/mnt
  rm -rf "$apps/Mirrin.app"
  cp -R "$MNT/Mirrin.app" "$apps/" || fail "couldn't copy Mirrin.app into $apps. The CLI is installed at $DEST/mirrin."
  hdiutil detach "$MNT" -quiet 2>/dev/null || true
  MNT=
  unquarantine "$apps/Mirrin.app"
  say "installed Mirrin.app in $apps"
  if ! spctl --assess --type execute "$apps/Mirrin.app" >/dev/null 2>&1; then
    say "note: this Mirrin.app isn't notarized by Apple yet. It opens normally when installed this way, but macOS may ask again for the microphone and other permissions after each update."
  fi
}

main() {
  BUILD=$(printf '%s' "${MIRRIN_BUILD:-default}" | tr '[:upper:]' '[:lower:]')
  case "$BUILD" in
    default) SUFFIX=; BUILD_LABEL= ;;
    nowhatsapp) SUFFIX=-nowhatsapp; BUILD_LABEL="MIT (without WhatsApp) " ;;
    *) fail "unknown MIRRIN_BUILD: $BUILD. Choose default (GPL-3.0, with WhatsApp) or nowhatsapp (MIT)." ;;
  esac
  command -v curl >/dev/null 2>&1 || fail "this installer needs curl. Install it and run the same command again."
  PLATFORM=$(platform)
  OS=${PLATFORM%/*}
  ASSET=mirrin-${OS}-${PLATFORM#*/}$SUFFIX

  VERSION=${MIRRIN_VERSION:-}
  if [ -z "$VERSION" ] && ! latest; then
    case "$CODE" in
      2?? | 404) fail "couldn't find a published Mirrin release at $RELEASES. You can build from source: $FROM_SOURCE" ;;
      *) fail "$(trouble "the latest release" 0)" ;;
    esac
  fi
  BASE=$RELEASES/download/$VERSION

  TMP=$(mktemp -d 2>/dev/null || mktemp -d -t mirrin)
  trap cleanup EXIT
  trap 'exit 130' INT TERM

  say "installing Mirrin $VERSION for $PLATFORM"
  fetch "$BASE/SHA256SUMS" "$TMP/SHA256SUMS" \
    "Mirrin $VERSION has no checksum list (SHA256SUMS), so its downloads can't be checked. Nothing was installed.
Pick a newer release from $RELEASES, or build from source: $FROM_SOURCE"
  verify_signature
  fetch "$BASE/$ASSET" "$TMP/$ASSET" \
    "Mirrin $VERSION has no ${BUILD_LABEL}build for $PLATFORM ($ASSET). Choose a release that includes this build. See $RELEASES for what's available, or build from source: $FROM_SOURCE"
  checksum_ok "$TMP/$ASSET" "$ASSET" ||
    fail "$WHY. Nothing was installed. Try again; if it keeps happening, please report it: $ISSUES"
  say "checksum matches"
  verify_attestation "$TMP/$ASSET" "$ASSET"

  DEST=$(bin_dir)
  mkdir -p "$DEST" 2>/dev/null || fail "couldn't create $DEST. Choose another folder with MIRRIN_BIN_DIR=... and run again."
  REPLACED=
  if [ -f "$DEST/mirrin" ]; then REPLACED=1; fi
  # Stage next to the old copy and try it before replacing anything.
  NEW=$DEST/.mirrin.new
  mv -f "$TMP/$ASSET" "$NEW" 2>/dev/null ||
    fail "couldn't write to $DEST. Choose another folder with MIRRIN_BIN_DIR=... or check its permissions."
  chmod +x "$NEW"
  "$NEW" version >/dev/null 2>&1 ||
    fail "the downloaded mirrin won't start on this machine ($PLATFORM). Nothing was installed. Please report it: $ISSUES"
  mv -f "$NEW" "$DEST/mirrin"
  NEW=
  if [ "$OS" = darwin ]; then
    unquarantine "$DEST/mirrin"
    if [ "$BUILD" = nowhatsapp ]; then
      say "The MIT build is installed without Mirrin.app, which includes WhatsApp. Run mirrin tray for the menu bar."
    else
      [ -n "${MIRRIN_NO_APP:-}" ] || install_app
    fi
  fi

  say ""
  say "Mirrin $VERSION is installed: $DEST/mirrin${REPLACED:+ (it replaced the one that was there)}"
  path_note
  found=$(command -v mirrin 2>/dev/null || true)
  if [ -n "$found" ] && [ "$found" != "$DEST/mirrin" ]; then
    say "note: another mirrin at $found comes first on your PATH; remove it or put $DEST ahead of it."
  fi
  service_note
  old_install_note
  say ""
  say "Next:"
  say "  export ANTHROPIC_API_KEY=...   # or OPENAI_API_KEY / GEMINI_API_KEY, or just have Ollama running"
  say "  mirrin chat                    # works with zero config; \`mirrin init\` if you'd rather be asked"
  if [ -z "$SERVICE" ]; then
    say "  mirrin service install         # let it live: menu bar icon, wake word, channels"
  fi
}

# Everything runs from here, so a download cut off halfway runs nothing.
main "$@"
