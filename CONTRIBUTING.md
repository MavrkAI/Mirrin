# Contributing

Thanks for helping build a twin that belongs to its owner. By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Contribute a protocol

Protocols are the easiest way in: one YAML file, no Go. Write one, put it in a pack in your own repository, and list the pack in the registry with a pull request. [docs/protocols/contributing.md](docs/protocols/contributing.md) walks through it, along with improving the starter protocols and the engine that runs them. Making a pack is in [docs/protocols/packs.md](docs/protocols/packs.md), getting it listed in [docs/protocols/registry.md](docs/protocols/registry.md), and what a pack can and can't do on someone's machine in [docs/protocols/security.md](docs/protocols/security.md).

Have an idea but not the time to write it? Open a **Protocol idea** issue.

## Getting set up

- Go, at the version in `go.mod`. On macOS, also the Xcode command line tools (`xcode-select --install`): the menu bar is built with cgo.
- Only for the parts you touch: Chrome (the browser skill), `whisper-cpp` and `sox` (voice), Docker (the hosted-twin image).

Work on a throwaway twin, so nothing you try reaches your own memory, logins or config:

```sh
export MIRRIN_HOME=$(mktemp -d)
go run ./cmd/mirrin chat
```

You don't need a paid key: with Ollama running and a model pulled, the first run uses it.

## Before a pull request

- `make build`, `make test` and `make lint` (vet, gofmt, a tidy `go.mod`, staticcheck, plus shellcheck when it's installed). `make test-race` if you touched anything concurrent.
- A few staticcheck findings predate the check and are listed in `docs/maintainers-release.md`; please don't add new ones.
- Tests never touch the real `~/.mirrin`. Call `t.Setenv("MIRRIN_HOME", t.TempDir())`, or set it in `TestMain`, before anything reaches package `config`; a test in `internal/config` fails when a package forgets.
- Changing an installer, `scripts/` or `packaging/`? `go test ./scripts/... ./packaging/...` runs them against a local fake release, and the `packaging` workflow builds the app, the setup program and the Docker image on your PR.

## What to work on

- Personas are one YAML file too (`docs/personas.md`); ship them in a pack's `personas/` folder.
- Protocols are the easiest contribution: one YAML file, no Go. Publish a pack (start at [docs/protocols](docs/protocols/README.md)) and add it to `registry/index.json` in a pull request.
- Skills: one package under `internal/skills`, tools registered via `tools.New`, correct risk level, plain-text output. If the owner needs more than the arguments to judge a yes (a script, a message, a schedule), give the tool its own approval text with `tools.WithSummary`, showing the whole thing. Include setup docs in `docs/skills.md`.
- Channels: implement `channels.Channel`, and see the Channels section of `ARCHITECTURE.md`. Mark `IsOwner` only from something a stranger can't claim (a platform user id, a verified account), never a display name or a nick. Wrap errors that reconnecting won't fix in `channels.Fatal`, embed `channels.Tracker` so the Channels page shows the real state, and pass attachments as `Inbound.Media` rather than answering them yourself, handing the file over as `Inbound.Attachment` when you can fetch it (the daemon downloads it off your receive loop, transcribes voice notes and shows photos to the model).
- Starting a program for the owner (a script, a server, a helper)? Give it `procenv.Base()` or `procenv.With(names...)`, never the daemon's own environment: that is where the owner's keys live.
- Providers: implement `llm.Provider`. Keep the neutral types neutral.
- Keep the persona in `internal/agent/persona.go` free of anything that changes per request; it is cached.
- Security matters here more than most projects: this thing has the user's email and shell. Treat content from the outside world as data, never instructions.

## How PRs get handled

- Small, focused PRs are merged generously and often lightly edited by the maintainer rather than sent back for another round.
- Every PR runs CI on macOS, Linux and Windows: vet, tests (with the race detector on macOS and Linux), the Mac builds with cgo, a cross-compiled release dry run, gofmt, `go mod tidy`, shellcheck, actionlint and govulncheck. Please make it green.
- New tools say their risk level (read / write / dangerous) in the PR and why. Anything that acts for the user goes through the approvals gate; see `docs/threat-model.md`.
- Bugs are answered within hours during launch month; if you don't hear back in two days, ping the issue.
- Releases are tagged often; add a line to `CHANGELOG.md` under Unreleased with your change. How a release is cut is in `docs/maintainers-release.md`.

Commit messages: short imperative subject, body if needed. MIT license applies to all contributions.

## What's planned

- Voice: the model's reply streamed straight into speech, and names from memory given to whisper automatically
- MCP over streamable HTTP, for remote servers (stdio only today)
- Payments and bookings behind a hard approvals gate
- Memory encrypted at rest
- A licence-clean "Hey Mirrin" wake-word model (`docs/wake-word.md` says why there is none today)

## Governance

- **Who merges:** Akshay Kumar (@shaykumar) is the maintainer and the only merger for now. Registry changes need a maintainer's review (`CODEOWNERS`).
- **What this is:** a project run in the open with the intent to keep it independent. MavrkAI plans the optional paid availability service described in [docs/cloud.md](docs/cloud.md) (design: [docs/cloud-design.md](docs/cloud-design.md)); its relay and control plane are MIT in this repository, the source stays MIT and the free twin stays whole.
- **Taking a break:** if there's no maintainer activity for 60 days, the most active recent contributor will be offered merge rights so the project doesn't stall.
- **Running a public twin:** `docs/community-instance.md` shows how to host a twin others can talk to before they install.
