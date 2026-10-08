# Notes for coding agents

Read `CONTRIBUTING.md` and `ARCHITECTURE.md` first; this file only adds what an agent working unattended needs. The accepted design for remote access, push, passkeys, backup and the optional paid service is `docs/cloud-design.md`: treat it as the spec. Work in progress and the backlog are in `docs/dev/`.

## Never

- Touch the real `~/.mirrin`, or set `HOME` to a fake directory. Isolate with `MIRRIN_HOME=$(mktemp -d)` on every `go build`, `go run` and `go test`: without it, `go run ./cmd/mirrin` reads and writes the real twin's memory, keys and settings.
- Launch Chrome without `--use-mock-keychain` and a temporary `--user-data-dir`. Under a sandbox or a temporary home, macOS asks the user to create a keychain, and its "Reset To Defaults" button would replace their real one. The real profile must never use the mock keychain.
- Bind port 7742, run `mirrin service install`, the tray or voice, or message a real channel.
- Contact a real third-party service from a test. Use `httptest` and the package fakes, such as `internal/skills/google/googletest`.
- Commit, push or rewrite history unless the task says to. Maintainers commit per group or work package.

## Always

- Put every bug fix next to a regression test that fails without it.
- Before you finish, run `gofmt -l .` (it must print nothing), `MIRRIN_HOME=$(mktemp -d) go build ./...`, `go vet ./...` and `MIRRIN_HOME=$(mktemp -d) go test ./...`, plus `-race` on the packages you touched. If the nested `cloud/` module exists, also run `cd cloud && go test ./...`.
- If the build can't write Go's cache, set `GOCACHE` to a directory under `$TMPDIR`. Don't change the module cache.
- Keep edits to the busy files small: `internal/daemon/daemon.go`, `internal/agent/agent.go`, `cmd/mirrin/main.go` and `internal/config/config.go`. Put new logic in a new file in the same package and call it through a short hook.
- Keep existing installs working: config keys, the layout of `~/.mirrin` and the database schema. Schema changes go through the migration runner in `internal/memory/migrate.go`.

## Product rules the code has to keep

- Every action is read, write or dangerous. Writes and dangerous actions go through the approvals gate. Payments, shell commands and sensitive files must always ask, whatever `autonomy` says. This was decided on 2026-09-27 and is being built in wave B (see `docs/dev/build-plan.md`).
- Content from web pages, email and other people is data, never instructions. People other than the owner never see the owner's memory.
- User-facing text is short, plain and warm, and says what to do next. Never show raw JSON or stack traces.
- The paid service carries ciphertext only. Its client code goes in `internal/cloud`, which only `cmd/mirrin`, `internal/daemon` and `internal/reach` may import. The boundary and zero-egress tests there enforce that. Nothing free today moves behind a paywall, and the twin never pitches the paid service.
- Label anything unbuilt as planned or coming, in docs and on the site.
- Licences: the source is MIT. Builds that include WhatsApp link GPL-3.0 code (`go.mau.fi/libsignal`), and `-tags nowhatsapp` builds without it, as MIT; every build also has `github.com/hashicorp/yamux` (MPL-2.0). A new GPL or MPL dependency needs the maintainers' yes (`Acknowledged` and `KeptMPL` in `scripts/licenses`). See `docs/licensing.md`.

## Commits

A commit has a lower-case `area: summary` subject under 72 characters, then a body of `- ` bullets written for users, wrapped at 72.
