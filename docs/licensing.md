# Licensing

This page says what licence covers Mirrin's source code, what the programs published with each release contain, and what follows from that. It describes the facts as `scripts/licenses` finds them and the project's reading of the licences. It isn't legal advice: if you plan to ship Mirrin inside a product, check with your own lawyer.

## In short

- **The source code in this repository is MIT** ([LICENSE](../LICENSE)). You can use, change and share it under the MIT licence, in open or closed projects.
- **The `mirrin` programs in the releases are built from that source plus about 85 third-party Go modules.** Go links everything into one executable. Almost all of those modules are under permissive licences (MIT, BSD, Apache-2.0, ISC, Zlib, public domain).
- **The WhatsApp channel is the main exception.** It uses [whatsmeow](https://pkg.go.dev/go.mau.fi/whatsmeow) (MPL-2.0), which uses `go.mau.fi/util` (MPL-2.0) and [`go.mau.fi/libsignal`](https://pkg.go.dev/go.mau.fi/libsignal) (**GPL-3.0**). So every program built with WhatsApp contains GPL-3.0 code, and is GPL-3.0 as a whole.
- **Every program also contains one MPL-2.0 module, with or without WhatsApp:** [`github.com/hashicorp/yamux`](https://pkg.go.dev/github.com/hashicorp/yamux), which carries the many connections of the relay tunnel (`internal/relay`, and `mirrin-relay` too). The MPL is a file-level copyleft: yamux's own files stay under it, with their source available, and it asks nothing of the rest of the program.
- **A program built with `-tags nowhatsapp` contains no GPL code, and is distributed under MIT.** Inside it, Mirrin's own code is MIT, yamux's files stay MPL-2.0, and Go and the other modules keep their permissive licences, whose notices go with it. CI checks this on every change.
- **Every release publishes both builds.** The default download includes WhatsApp and is GPL-3.0 as a whole. The extra `nowhatsapp` downloads are MIT, with yamux's files under MPL-2.0.

## What is linked, exactly

`go run ./scripts/licenses` asks the Go tool which packages each release build of `mirrin` and `mirrin-relay` links (`go list -deps` for Linux and Windows without cgo, and macOS with cgo, on amd64 and arm64), finds each module's licence files and names the licence. It leaves out modules only tests use. `THIRD_PARTY_NOTICES` in the repository is its output, with every licence text, and each release carries a copy that also gives the version of each module (`THIRD_PARTY_NOTICES.txt`).

The texts also travel with the programs themselves, however they're installed: every `mirrin` embeds LICENSE and THIRD_PARTY_NOTICES (`internal/notices`) and prints them with `mirrin licenses`, Mirrin.app keeps them in `Contents/Resources`, and the Windows setup program installs them beside `mirrin.exe`.

The GPL-3.0 code comes in like this:

```
cmd/mirrin → internal/daemon → internal/channels/whatsapp → go.mau.fi/whatsmeow → go.mau.fi/libsignal/...
```

It isn't a module that merely appears in `go.sum`: the Signal protocol code does WhatsApp's encryption, `go version -m mirrin` lists it among the modules of every WhatsApp-enabled release program, and a build with symbols holds about 600 of them from it (`go build -o mirrin ./cmd/mirrin && go tool nm mirrin | grep -c go.mau.fi/libsignal`). Its licence file is the GPL version 3 text. Its source files don't add an "or any later version" notice.

| Licence | Modules in the release programs | Only for WhatsApp |
|---|---|---|
| GPL-3.0 | `go.mau.fi/libsignal` | yes |
| MPL-2.0 | `go.mau.fi/whatsmeow`, `go.mau.fi/util` | yes |
| MPL-2.0 | `github.com/hashicorp/yamux` | no: it is in every build, and in `mirrin-relay` |
| Apache-2.0, BSD-2/3-Clause, MIT, ISC, Zlib, public domain | everything else, and Go itself (BSD-3-Clause) | a few (listed as "(WhatsApp)" in THIRD_PARTY_NOTICES) |

No LGPL, AGPL or unknown licence is linked. `scripts/licenses` fails CI if one appears, if a GPL module other than the one the project has acknowledged shows up, or if any MPL module but yamux comes into the build without WhatsApp. You can check a program yourself: `go version -m mirrin` lists every module in it, and each one is in THIRD_PARTY_NOTICES.

## What that means

**For the source (MIT).** Nothing changes. The MIT licence covers Mirrin's own files, and depending on a GPL module doesn't relicense them. People may fork Mirrin under MIT. If they build it with WhatsApp and share the program, the next point applies to them too.

**For programs built with WhatsApp.** In the Free Software Foundation's reading of the GPL, which is the common one, a program that statically links GPL code is a work based on it, and may only be passed on as a whole under GPL-3.0. Everything else in it is compatible with that: MIT, BSD, ISC, Zlib and Apache-2.0 code may be combined into a GPL-3.0 program, and the MPL-2.0 files carry no "Incompatible With Secondary Licenses" notice, so MPL section 3.3 allows it. Sharing such a program therefore means:

- recipients get the GPL-3.0 rights to the whole program, and no extra restrictions may be added (for example no terms that forbid reverse engineering, and no store that adds its own usage rules, which is why GPL programs are generally kept out of the Mac App Store);
- the GPL-3.0 text and the other notices go with it (THIRD_PARTY_NOTICES holds them);
- its complete corresponding source is available to recipients: every release attaches `Mirrin-<version>-source.tar.gz`, the tagged source with every dependency, which CI checks builds with no network;
- for the MPL-2.0 files, recipients are told where their source is (it is in that archive too).

**For programs built with `-tags nowhatsapp`.** They contain no GPL code, so they can be shared under MIT. Not everything in them is MIT, though: alongside Mirrin's own code they contain yamux, under MPL-2.0, and code under BSD, Apache-2.0, ISC, Zlib and public-domain terms. Sharing one means:

- keeping the notices each licence asks for (THIRD_PARTY_NOTICES again; Apache-2.0 also asks for its NOTICE files, which it includes);
- telling recipients where yamux's source is (the MPL asks this for its own files only; the release's source archive holds it). If you change yamux's files, your changes to those files are MPL-2.0 too.

Build one with:

```sh
make build TAGS=nowhatsapp        # or: go build -tags nowhatsapp ./cmd/mirrin
```

In that build the WhatsApp channel says, wherever it is used, that this build doesn't have it, and `mirrin update` won't replace it with a release that does.

**For Mirrin Cloud and other services.** The relay and the control plane described in [cloud-design.md](cloud-design.md) are separate programs that don't link whatsmeow, so the GPL doesn't touch them. `mirrin-relay` uses yamux, so what is said above about its MPL-2.0 files applies to it too; its other modules are permissive, and all of them are in THIRD_PARTY_NOTICES. Running Mirrin on your own machine isn't distribution, so it creates no obligations either.

## What each release publishes

Every release publishes both builds for Linux, macOS and Windows on amd64 and arm64:

- `mirrin-<os>-<arch>[.exe]`: includes WhatsApp, distributed as a whole under GPL-3.0. This is the default download, including Homebrew, Mirrin.app and the Windows setup program.
- `mirrin-<os>-<arch>-nowhatsapp[.exe]`: built with `-tags nowhatsapp`, distributed under MIT, without WhatsApp; yamux's files in it stay MPL-2.0. The `.exe` suffix is only for Windows.

Both builds and the Linux relay binaries appear in the same `SHA256SUMS`. The release also includes the licence notices and complete corresponding source. LICENSE stays MIT for Mirrin's own source.

To install the MIT build on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/MavrkAI/Mirrin/main/install.sh | MIRRIN_BUILD=nowhatsapp sh
```

On Windows (PowerShell):

```powershell
$env:MIRRIN_BUILD = 'nowhatsapp'
irm https://raw.githubusercontent.com/MavrkAI/Mirrin/main/install.ps1 | iex
```

The MIT installer on macOS installs the CLI only; run `mirrin tray` for the menu bar. The disk image contains the default GPL-3.0 app. To update the MIT build, run the installer again with `MIRRIN_BUILD=nowhatsapp`; `mirrin update` currently refuses to replace a build without WhatsApp.

## Other things inside the programs

- **No wake-word model.** Earlier builds embedded a "Hey Maverick" model the maintainers trained (see [wake-word.md](wake-word.md)) using openWakeWord's precomputed ACAV100M speech features, which are published under CC BY-NC-SA 4.0, and macOS system voices, whose terms are Apple's. The source's MIT licence doesn't cover it, and whether a model trained on such data inherits those terms is unsettled, so it isn't cleared for commercial use. It is left out of the public code, the releases and the source archives; without it, every persona's name, Mirrin's included, is heard in what whisper transcribes on your computer. Licence-clean models for Mirrin, Nyra and Pickoo, trained only on permissively licensed data (WP-24 in the cloud design), are trained but not released yet; [wake-word.md](wake-word.md) has their results and open caveats. A build made with `-tags wakemodel` from a checkout that has the file embeds the old model; no release is built that way.
- **The BIP-39 English wordlist** (`internal/backup/bip39_english.txt`), the 2048 words a backup's 12 recovery words come from, is copied unchanged from the BIP-39 specification, which is published under MIT. THIRD_PARTY_NOTICES carries its notice, after the Go modules.
- **The Python helpers** embedded for voice (`wake_helper.py`, `kokoro_tts.py`) are Mirrin's own code, under MIT.

## Downloaded later, not part of a release

`mirrin voice setup` fetches speech models and Python packages from their publishers onto your machine: the whisper.cpp speech-recognition model, the Kokoro voice, and openWakeWord with the feature models every wake-word model runs on. They aren't in the release files, so the release doesn't redistribute them, but their own licences apply to you once they're downloaded. Setup doesn't fetch openWakeWord's example wake-word models, which are published under CC BY-NC-SA 4.0 (earlier versions fetched "hey jarvis").

## Keeping this up to date

- After changing dependencies, run `make notices` and commit THIRD_PARTY_NOTICES and its copy in `internal/notices`. `make lint` and CI fail when the file no longer lists what the builds link, or when the copy differs, and pass when only a licence text changed (a new copyright year).
- A module under a licence `scripts/licenses` doesn't know, or a new GPL-family licence, stops CI. Look at it, and if the project agrees to link it, add it to `Acknowledged` in `scripts/licenses/main.go` with the reason and update this page.
- `go run ./scripts/licenses -tags nowhatsapp -mit` is the check that keeps the WhatsApp-free build one that can be shared under MIT: no GPL code, and no MPL code but the modules in `KeptMPL` (yamux today). A new MPL module in that build stops CI until it is added there, with the reason, and to this page.
- THIRD_PARTY_NOTICES opens by saying what each build is (GPL-3.0 as a whole with WhatsApp, MIT without it, and the MPL files in both), worked out from what the builds link, so `make notices` keeps that true too.
