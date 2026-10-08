# Releasing Mirrin

For maintainers with permission to push tags. A release is one tag, `vX.Y.Z`, pushed to a commit on `main` whose CI is green. `.github/workflows/release.yml` does the rest: nobody uploads files by hand.

## What a release contains

| File | Who uses it | Built by |
|---|---|---|
| `mirrin-linux-amd64`, `mirrin-linux-arm64` | `install.sh`, Homebrew on Linux, the Docker image | `portable` job (Ubuntu, no cgo) |
| `mirrin-windows-amd64.exe`, `mirrin-windows-arm64.exe` | `install.ps1` | `portable` job |
| `mirrin-darwin-amd64`, `mirrin-darwin-arm64` | `install.sh`, Homebrew on macOS | `macos` job (cgo, so the menu bar is in) |
| `mirrin-<os>-<arch>-nowhatsapp[.exe]` | MIT CLI without WhatsApp, selected with `MIRRIN_BUILD=nowhatsapp` | same `portable` and `macos` jobs as the default GPL-3.0 builds |
| `mirrin-relay-linux-amd64`, `mirrin-relay-linux-arm64` | self-hosted relay | `portable` job |
| `Mirrin-vX.Y.Z-macos.dmg` | a universal `Mirrin.app` (Intel and Apple silicon) | `macos` job |
| `Mirrin-vX.Y.Z-windows-setup.exe` | Start menu entry and sign-in startup | `windows` job, wrapping the `portable` job's `mirrin-windows-amd64.exe` |
| `THIRD_PARTY_NOTICES.txt` | every third-party module in the programs, with its version and licence text | `bom` job (`scripts/licenses`) |
| `Mirrin-vX.Y.Z-source.tar.gz` | the tagged source with every dependency (`vendor/`), checked to build offline; what the GPL asks to go with programs that link GPL code (docs/licensing.md) | `bom` job (`scripts/source-archive.sh`) |
| `Mirrin-vX.Y.Z-sbom.spdx.json` | an SPDX SBOM of the programs, read by syft from the module list Go builds into each binary | `bom` job |
| `SHA256SUMS` | every file above; the installers and `mirrin update` refuse anything that doesn't match | `publish` job |
| `SHA256SUMS.sigstore.json` | Sigstore signature of `SHA256SUMS`, made keylessly by the release workflow | `publish` job |

The names are the program's name and Go's `GOOS-GOARCH`, and they don't change: `install.sh`, `install.ps1`, `mirrin update`, the Homebrew formula and `scripts/releasecheck` all depend on them. Before anything is published, `releasecheck` reads every binary's header and Go build information. It checks the command identity, requires the `nowhatsapp` tag and no `go.mau.fi/` dependencies for MIT builds, and requires `go.mau.fi/libsignal` for the default builds. It stops the release if a file isn't what its name says (a Linux build once shipped as `mirrin-darwin-x86_64`), if a platform is missing, or if a stray file is in the way. The `publish` job is the only one that uploads, so jobs can't race each other for the same file, and the `bom` job, which runs third-party tools (syft), has no permission to publish or sign anything.

Public repositories also get GitHub attestations: SLSA build provenance (which workflow run built it, from which commit) for every file in `SHA256SUMS`, and the SBOM for the programs it describes (the CLIs, the disk image and the Windows setup program). The `attestations` job then checks each file with `gh attestation verify`, as a user would. (`actions/attest` makes both; `actions/attest-build-provenance` has been a thin wrapper around it since v4.)

Every action in the workflows is pinned to a commit, with its version in a comment (`actions/checkout@3d3c42e… # v7.0.1`), so a moved tag upstream can't change what runs with the release's permissions. Dependabot updates the commit and the comment together; review those PRs like code.

## One-time setup

Secrets and variables (Settings → Secrets and variables → Actions):

| Name | What for | Without it |
|---|---|---|
| `APPLE_CERT_P12`, `APPLE_CERT_PASSWORD` | the Developer ID Application certificate (base64 .p12) and its password | Mac builds are ad-hoc signed, and the run says so in a notice. macOS ties permissions such as the microphone to the signature, so users are asked again after every update (`mirrin update` tells them) |
| `APPLE_SIGN_ID` | `Developer ID Application: Name (TEAMID)`; optional, since the script finds the imported identity | |
| `APPLE_ID`, `APPLE_TEAM_ID`, `APPLE_APP_PASSWORD` | notarization with `notarytool` (an app-specific password from appleid.apple.com). All three or none: some but not all stops the release, as does notarizing without the certificate | signed but not notarized, with a warning. `install.sh` and `mirrin update` still work, because their downloads carry no quarantine flag, but a disk image or binary downloaded in a browser needs right-click → Open the first time |
| `HOMEBREW_TAP_TOKEN` | a fine-grained token with Contents: write on the tap repository | the `homebrew` job only leaves a notice |
| `HOMEBREW_TAP` (variable) | the tap repository, if not `MavrkAI/homebrew-tap` | |

With the certificate and the notary credentials, `scripts/release-macos.sh` signs all four CLIs and the app with the hardened runtime, notarizes them in one submission, staples the app, then signs, notarizes and staples the disk image, and asks Gatekeeper (`spctl --assess`) about both before anything is uploaded. When Apple refuses a submission, the job prints notarytool's log and stops. To make the certificate secret: export the Developer ID Application certificate with its private key from Keychain Access as a .p12, then `base64 -i cert.p12 | pbcopy`.

Repository settings:

- Create the tap repository (`MavrkAI/homebrew-tap`, with a `Formula/` folder) so `brew install mavrkai/tap/mirrin` works.
- Rules: require the `ci` checks on `main`, and add a tag ruleset for `v*` that blocks deletion and updates, so a published tag can never move.
- Security: private vulnerability reporting (SECURITY.md points to it), secret scanning with push protection, Dependabot security updates.
- Moderation: turn on reported content (Settings → Moderation options), which is how the Code of Conduct asks people to report problems.
- Run the `labels` workflow once (Actions → labels → Run workflow) to create the labels in `.github/labels.yml`.

## Publishing the public repository

The public repository, `MavrkAI/Mirrin`, is a new one with a single fresh commit, made from the maintainers' private repository minus what stays private; the private one is never renamed or made public. `scripts/publish` makes that copy and checks it. It is itself never published, because it names what it keeps out.

```sh
go run ./scripts/publish main        # any commit, tag or branch; -out DIR picks the folder
go run ./scripts/publish -check DIR  # check a copy again
```

1. It exports the commit with `git archive` into a new folder outside the repository, leaving out everything in `scripts/publish/exclude.txt`, the one list of what isn't published: the maintainer's own wake model (it was trained on non-commercial data, so no `.onnx` file is published until WP-24 makes a licence-clean one), `docs/dev/` (planning, the backlog and business planning), `docs/launch/`, `.claude/` and the tool itself.
2. It checks the copy and stops on any problem: a path from the list is present; a file's name or text has a private detail (a name from the private name search, an internal code name, the maintainer's own email or phone number, or the persona's old name); gitleaks, when installed, finds a secret that `.gitleaksignore` doesn't list; or `go build ./...` or `go vet ./...` fails, with no build tags, in any module of the copy. It also notes lines that point at paths a public reader won't have.
3. When everything passes, it prints, and never runs, the commands that commit the copy as one commit by `Mirrin <hello@mirrin.app>`, create the repository (private until you've looked at it there), push `main`, make it public, and turn on the settings under "Repository settings" with `gh api`: private vulnerability reporting, secret scanning with push protection, Dependabot alerts and security updates, a ruleset for `main` (pull requests with the `ci` checks green, no force pushes or deletion) and one for `v*` tags, and approval before a fork's pull request runs a workflow.

`.gitleaksignore` lists what gitleaks flags that is no secret, each checked by hand: published test vectors (PASETO, RFC 8291, RFC 9421, AWS SigV4), keys derived from BIP-39's test phrases and public seeds, fakes the tests plant, and names of settings. An entry names a file, a rule and a line, so moving a vector makes the scan fail again; check the finding is the same vector before you update its line. A real key is never listed: rotate it. Fix every problem in the private repository and commit before you run `publish` again, rather than editing the copy.

## Before the first public release

The last published release, v0.2.0, came out under the program's earlier name and module path, so none of today's install paths work until a new release exists.

1. Publish `MavrkAI/Mirrin` as above before tagging. The Sigstore signature and the attestations name the repository the workflow ran in, and `install.sh`, `mirrin update` and the commands below expect `MavrkAI/Mirrin`. Add the secrets and variables under "One-time setup" to it, since a new repository has none.
2. Rename the starter pack's repository to `MavrkAI/mirrin-pack-starter` (with its `pack.yaml` and `protocols/`). Then, in a pull request to `main`, point the starter entry in `registry/index.json` at it and say Mirrin in its description and author; the `registry-sign` workflow signs the new index. Never change `index.json` any other way: it is signed.
3. Cut the first Mirrin release as below.
4. From a machine that isn't signed in to GitHub, check that each of these works:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/MavrkAI/Mirrin/main/install.sh | sh
   curl -fsI https://github.com/MavrkAI/Mirrin/releases/latest
   curl -fsI https://raw.githubusercontent.com/MavrkAI/Mirrin/main/registry/index.json
   GOPROXY=https://proxy.golang.org go list -m github.com/MavrkAI/Mirrin@latest
   ```

5. Make a repository from `examples/pack-template` and push it. Its `lint protocols` workflow installs the released mirrin with `install.sh`, so it can only go green once steps 1 and 3 are done; if it is red, packs made from the template will be too.

## Cutting a release

1. Check that `main` is green, including the `packaging` workflow if the installers or scripts changed since the last release.
2. In `CHANGELOG.md`, move the Unreleased notes under `## X.Y.Z — YYYY-MM-DD`. `scripts/release-notes.sh` takes the release notes from that section and adds install and verify instructions. Commit that to `main`.
3. Optional local dry run: `make clean release` builds and checks everything this machine can build (Linux and Windows anywhere, plus both Macs on a Mac). `scripts/release-macos.sh` makes the universal app and the disk image on a Mac. `go test ./scripts/... ./packaging/...` runs both installers and the release scripts against a local fake release, and `go test ./cmd/mirrin -run Update` does the same for `mirrin update`.
4. Tag and push:

   ```sh
   git tag -a vX.Y.Z -m "Mirrin vX.Y.Z"
   git push origin vX.Y.Z
   ```

   A tag with a hyphen (`v0.4.0-rc.1`) is published as a pre-release. GitHub's latest-release link skips pre-releases, so the installers only fetch one when `MIRRIN_VERSION` asks for it, and the Homebrew tap isn't updated.
5. Watch the workflow: `portable`, `macos` and `windows` build; `bom` writes the licence notices, the source archive (and builds it offline) and the SBOM; `publish` checks, sums, signs, attests and uploads; then `homebrew` updates the tap, `attestations` verifies every file's attestation, and `smoke` installs the release on Linux (x64 and arm64), both kinds of Mac and Windows (x64 and arm64), runs `mirrin version`, and checks that `mirrin update --check` agrees with GitHub about the latest release (this one, or, for a pre-release or a patch to an older line, the one GitHub marks as latest).
6. When `smoke` is green, try `brew upgrade mirrin` or `brew install mavrkai/tap/mirrin` on a Mac, then announce.

## Checking a release by hand

```sh
gh release download vX.Y.Z -R MavrkAI/Mirrin -p 'SHA256SUMS*' -p mirrin-linux-amd64
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity https://github.com/MavrkAI/Mirrin/.github/workflows/release.yml@refs/tags/vX.Y.Z \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS          # macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
gh attestation verify mirrin-linux-amd64 -R MavrkAI/Mirrin
gh attestation verify mirrin-linux-amd64 -R MavrkAI/Mirrin --predicate-type https://spdx.dev/Document/v2.3   # the SBOM
```

`install.sh` and `mirrin update` do the checksum check every time and the signature check whenever `cosign` is installed. With cosign installed they also refuse a release whose `SHA256SUMS.sigstore.json` is missing, since every release is signed.

## How users update and uninstall

- `mirrin update` installs the latest release, and only when someone runs it: Mirrin never updates itself. It checks the download against `SHA256SUMS` (and its Sigstore signature when cosign is installed), tries the new program, swaps it in with a rename, updates `Mirrin.app` from the disk image on a Mac, and restarts the background service. `mirrin update --check` only says whether there is a newer release. A Homebrew install is left to `brew upgrade mirrin`, and a build from source says how to rebuild or switch. `mirrin update --version vX.Y.Z` installs a particular release, including an older one.
- `mirrin uninstall` removes the background service, the program wherever the installers put it, `Mirrin.app`, and leftovers of updates. It removes only what is Mirrin's: a program on PATH must answer `version` as Mirrin does, and an app must have Mirrin's bundle identifier (`com.mirrin.mavrk`). When another twin's service (installed with a different `MIRRIN_HOME`) still runs a copy, only the running program goes, and it says so. It keeps the twin in `~/.mirrin` unless its owner types `delete` (and never offers that for a folder that isn't a twin's, such as the home folder), and offers to save a copy with `mirrin identity export` first. `--yes` removes the program and the service without asking and keeps the twin. Asked in a terminal, it offers to remove the PATH lines the installers added, keeping a copy of the file; with `--yes`, or on Windows, it only says where they are.

## Licences

[docs/licensing.md](licensing.md) explains the licences in the programs. The part that affects releases: the WhatsApp channel links `go.mau.fi/libsignal` (GPL-3.0), so the published programs contain GPL-3.0 code, and each release carries `THIRD_PARTY_NOTICES.txt` and the complete source (`Mirrin-vX.Y.Z-source.tar.gz`). Every release publishes the default programs with WhatsApp under GPL-3.0 as a whole, and an additional MIT CLI per platform named `mirrin-<os>-<arch>-nowhatsapp[.exe]` (yamux's files in it stay MPL-2.0, as in every build). Set `MIRRIN_BUILD=nowhatsapp` when running either installer to choose MIT. On macOS this skips the GPL-3.0 app bundle; run `mirrin tray` instead. Use the same installer setting for MIT updates; the current updater refuses builds without WhatsApp.

- After changing dependencies, `make notices` updates `THIRD_PARTY_NOTICES` and the copy every program embeds (`internal/notices`, printed by `mirrin licenses`); CI fails until they list what the builds link. Mirrin.app (`scripts/build-app.sh`) and the Windows setup program (`packaging/windows/mirrin.iss`) carry LICENSE and THIRD_PARTY_NOTICES too.
- `make build TAGS=nowhatsapp` builds a program without WhatsApp and so without GPL code; it still has yamux (MPL-2.0), whose files keep their licence. CI keeps that build compiling, tested and free of any other copyleft code (`scripts/licenses -mit`).

## When something goes wrong

- Never move, delete and recreate, or reuse a tag. Anyone who fetched the first one has different source under the same version, and the Go checksum database remembers the first. (v0.2.0 was moved once after a failed run.) Fix forward with the next patch version.
- A build job failed and nothing was published: if it was a flake, use "Re-run failed jobs"; the run builds the same commit again. If the code needs a fix, merge it and tag the next patch version.
- `publish` failed partway: if a draft or partial release exists, delete the release (not the tag) and re-run the failed jobs.
- The release is out, but `smoke` failed or a file is bad: edit the release and tick "Set as a pre-release" so the latest-release link, and so the installers, go back to the previous version. Then fix it and release the next patch. Don't replace files in a published release: `SHA256SUMS`, its signature, its attestations and the Homebrew formula would no longer match them. Anyone who already updated can go back with `mirrin update --version vPREVIOUS`.
- `homebrew` failed: re-run the job, or by hand from the tap repository: `VERSION=vX.Y.Z path/to/Mirrin/scripts/release-brew.sh > Formula/mirrin.rb`. The script takes every checksum from the release's `SHA256SUMS` and stops if one is missing, rather than writing a checksum that can never match.

## The Docker image

The hosted-twin image isn't published to a registry yet. To build it for both architectures:

```sh
make dist-portable
docker buildx build --platform linux/amd64,linux/arm64 -f packaging/docker/Dockerfile -t mirrin-twin .
```

The `packaging` workflow builds it on every change and checks that it turns healthy.

## Lint

staticcheck (0.8.1, pinned) fails both CI and `make lint` on any finding. The backlog it once had (the voice channel's `strings.Title` and `strings.Index` calls, and three numeric HTTP status codes) was cleared on 2026-09-27, so a new finding is fixed where it is added.

ST1005, lower-case error strings, is off in `staticcheck.conf`, because many errors here are shown to the user as they are and are written as sentences.

govulncheck is pinned in CI, but its vulnerability database is live, so a newly published Go vulnerability turns every PR red until it is fixed on `main`. For one in the standard library, raise the `go` line in `go.mod` to the patched Go release (CI installs the version `go.mod` names); for one in a module, `go get module@fixed-version && go mod tidy`. Bump the govulncheck pin now and then like any other tool.
