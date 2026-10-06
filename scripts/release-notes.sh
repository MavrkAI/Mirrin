#!/bin/sh
# Prints the notes for a release: its CHANGELOG.md section, then how to
# install and verify it.
# Usage: scripts/release-notes.sh v0.3.0 > notes.md
set -eu
cd "$(dirname "$0")/.."
TAG=${1:-}
[ -n "$TAG" ] || { echo "usage: scripts/release-notes.sh <tag>" >&2; exit 2; }
REPO=${MIRRIN_REPO:-${ANTBOT_REPO:-MavrkAI/Mirrin}} # rename:keep
CHANGELOG=${CHANGELOG:-CHANGELOG.md}

# The section under "## 0.3.0 — date" (or "## [0.3.0]"), up to the next "## ".
notes=$(awk -v v="${TAG#v}" '
  /^## / {
    if (found) exit
    h = $2; gsub(/[][]/, "", h); sub(/^v/, "", h)
    if (h == v) { found = 1; next }
  }
  found { print }
' "$CHANGELOG")
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
  notes="See CHANGELOG.md for what changed."
fi

cat <<EOF
$notes

### Install

macOS and Linux:

    curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sh

Windows (PowerShell):

    irm https://raw.githubusercontent.com/$REPO/main/install.ps1 | iex

The installers check every download against \`SHA256SUMS\` before installing anything. Already running Mirrin? \`mirrin update\` installs this release with the same checks (if your version doesn't have it yet, run the installer again).

### Verify a manual download

\`SHA256SUMS\` lists every file in this release and is signed with Sigstore by this repository's release workflow:

    grep ' mirrin-linux-amd64\$' SHA256SUMS | sha256sum -c -      # macOS: shasum -a 256 -c -
    cosign verify-blob --bundle SHA256SUMS.sigstore.json \\
      --certificate-identity https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$TAG \\
      --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
EOF

# The release workflow sets ATTESTED when it made GitHub attestations, which
# needs a public repository.
if [ -n "${ATTESTED:-}" ]; then
  cat <<EOF

GitHub also holds a build provenance attestation for every file, saying which workflow run built it from which commit:

    gh attestation verify mirrin-linux-amd64 -R $REPO
EOF
fi

cat <<EOF

\`Mirrin-$TAG-sbom.spdx.json\` lists every module compiled into the programs (an SPDX SBOM).

### Licences

Mirrin's own source code is MIT. The default downloads include WhatsApp and are distributed as a whole under GPL-3.0, including Mirrin.app and the Windows setup program.

The extra \`mirrin-<os>-<arch>-nowhatsapp[.exe]\` downloads are MIT builds without WhatsApp. Every build, and the relay, also contains yamux, whose own files stay under MPL-2.0. To install one on macOS or Linux:

    curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | MIRRIN_BUILD=nowhatsapp sh

On Windows, set \`\$env:MIRRIN_BUILD = 'nowhatsapp'\` before running install.ps1. The MIT Mac install includes the CLI only; run \`mirrin tray\` for the menu bar. To update an MIT build, run the installer again with the same setting; the current \`mirrin update\` refuses builds without WhatsApp.

The Linux relay is available as \`mirrin-relay-linux-amd64\` and \`mirrin-relay-linux-arm64\`. Both CLI variants and the relay are covered by the single \`SHA256SUMS\`.

\`THIRD_PARTY_NOTICES.txt\` includes the full third-party licence texts, and \`Mirrin-$TAG-source.tar.gz\` is the complete source with every dependency. [docs/licensing.md](https://github.com/$REPO/blob/$TAG/docs/licensing.md) explains the licences, and why no wake-word model is included.
EOF
