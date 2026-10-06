# Contribute to protocols

This page is for anyone who wants to give something to Mirrin's protocols: it shows the three ways in (sharing a routine as a pack, improving the starter examples, and changing the engine that runs them) and what to expect once you've sent it.

By taking part you agree to the [Code of Conduct](../../CODE_OF_CONDUCT.md).

| You want to | Path | Needs Go? |
|---|---|---|
| Share a routine you wrote | [Your own pack, listed in the registry](#a-share-a-protocol-as-a-pack) | No (the registry check is optional) |
| Improve one of the protocols every new install starts with | [The starter examples](#b-improve-the-starter-examples) | A little |
| Fix or extend how protocols load, run, install or update | [The protocol engine](#c-improve-the-protocol-engine) | Yes |

Have an idea but not the time to write it? Open a **Protocol idea** issue and describe it as you'd brief a person.

## (a) Share a protocol as a pack

This is the main path, and it needs no Go. Your protocols live in your own repository, under your name and your licence, and the registry points people at them.

1. Write the protocol. `mirrin protocols new "your idea"` makes a scaffold; the format is in [docs/protocols.md](../protocols.md).
2. Put it in a pack, starting from `examples/pack-template`: [packs.md](packs.md).
3. Check it: `mirrin protocols lint .`, then install it on your own twin and run it.
4. List it in the registry with a pull request that adds one entry to `registry/index.json`: [registry.md](registry.md).

A maintainer reviews the entry and reads the pack at the commit you list, using the checklist in [registry.md](registry.md#what-reviewers-check). After that, people can find it with `mirrin protocols search`, on the Routines page, or by asking their twin.

## (b) Improve the starter examples

`examples/protocols/` holds the six starter protocols: morning briefing, evening wrap, inbox triage, reply like me, rebook and chase refund. They aren't only examples. The same text is built into Mirrin (the `Examples` map in `internal/protocols/protocols.go`), and every new install writes them into its empty protocols folder on first run. A change here changes what everyone starts with.

**The bar is high.** A starter should be:

- useful to nearly everyone, not just to people with a particular job, hobby or city;
- built only on what most people connect: email (both IMAP and Gmail), calendar, reminders and the web;
- quiet when there's nothing to say (`NOTHING_TO_REPORT` in anything scheduled);
- careful: anything that sends, books or pays goes through an approval the owner will expect;
- free of personal data and of anything that needs a var to work.

Most new ideas belong in a pack instead; a new starter is a product decision, so open an issue first. Fixes to the existing six (clearer wording, a tool they should use, a case they miss) are welcome as pull requests.

**How to change one:**

1. Edit the file in `examples/protocols/` and the matching entry in `Examples` in `internal/protocols/protocols.go`. They must stay identical, byte for byte; `TestRepoExamplesMatchStarters` fails when they differ, or when the number of files doesn't match.
2. Lint them, since no test does: `mirrin protocols lint examples/protocols`.
3. Run the tests, on a throwaway home:

   ```sh
   MIRRIN_HOME=$(mktemp -d) go test ./internal/protocols/
   ```

4. If you added or renamed a starter, update the `starter` entry's description in `registry/index.json`, which names them.

Starters are written only into an empty folder, so an improved starter reaches new installs. People who already have one keep their copy.

The pack template, `examples/pack-template`, has tests too: `TestPackTemplateLintsCleanWithItsPersona` expects exactly one protocol and one persona, linting with no warnings, and `TestPackTemplateLintWorkflow` (in `scripts/`) runs its lint workflow against it. Change the test with the template.

## (c) Improve the protocol engine

**Where the code lives:**

| Path | What it does |
|---|---|
| `internal/protocols/` | The format, loading, vars, lint, packs (install, pin, update, remove), the registry and its signature check |
| `internal/heartbeat/` | Schedules, late and missed runs, and running a protocol |
| `internal/skills/protocols/` | The chat tools: `list_protocols`, `run_protocol`, `create_protocol`, `update_protocol`, `find_protocols`, `install_pack` and its preview |
| `internal/api/protocols.go`, `protocols_updates.go`, `protocols.html` | The Routines page and its routes |
| `internal/daemon/api_protocols.go`, `problemfiles.go` | Connecting the page to the twin, and telling the owner about files it skipped |
| `internal/persona/` | Personas, including those from packs |
| `internal/identity/packs.go`, `afterimport.go` | Packs in identity export and import |
| `cmd/mirrin/main.go` (`protocolsCmd`), `cmd/mirrin/packs.go` | The `mirrin protocols` commands |
| `registry/` | The index, its public key, and the copy built into each release |

**Work on a throwaway twin**, so nothing you try reaches your own routines, memory or config:

```sh
export MIRRIN_HOME=$(mktemp -d)
go run ./cmd/mirrin protocols list
```

**Tests to run** (each on a throwaway home):

```sh
MIRRIN_HOME=$(mktemp -d) go test ./internal/protocols/ ./internal/skills/protocols/ ./internal/heartbeat/ ./internal/persona/
MIRRIN_HOME=$(mktemp -d) go test ./cmd/mirrin/ ./internal/api/ ./internal/daemon/ ./internal/identity/
MIRRIN_HOME=$(mktemp -d) go test ./scripts/ -run 'TestPackTemplateLintWorkflow|TestIssueTemplateLabelsExist'
```

Then, before the pull request, `make build`, `make test` and `make lint`, and `make test-race` if you touched the scheduler or anything else concurrent. Tests must never touch the real `~/.mirrin`: set `MIRRIN_HOME` with `t.Setenv` before anything reaches package `config` (see [CONTRIBUTING.md](../../CONTRIBUTING.md)).

**Things to keep in mind:**

- Every bug fix comes with a test that fails without it.
- Installing a pack runs git on an address someone else chose. The tests in `internal/protocols/packs_test.go` cover addresses that git could read as options, paths that escape the packs folder and symbolic links. Add a case there for anything new.
- A protocol is standing orders. Anything that adds, changes or runs one on the owner's behalf goes through an approval that shows what it will do.
- Keep existing installs working: the protocol format, the `~/.mirrin/protocols` layout and the provenance file in each installed pack are read by versions already out there.
- What the owner sees is short, plain and warm, and says what to do next. Never show raw errors or JSON.
- Anything unbuilt is labelled as planned or coming, in docs and on the site.

## The pull request

1. Fork the repository and make a branch.
2. Make the change, with its tests.
3. Add a line to `CHANGELOG.md` under Unreleased, written for the people who use Mirrin.
4. Open the pull request. Say what it does, how you tested it, and, for a new tool, its risk level (read, write or dangerous) and why. For a registry entry, use the registry template ([registry.md](registry.md#open-the-pull-request)).
5. CI runs on Linux, macOS and Windows. Please make it green.

## What to expect

- A maintainer reads every pull request. Small, focused ones are merged generously, and often lightly edited rather than sent back for another round.
- Changes under `registry/` always get a maintainer's review (`CODEOWNERS` asks for it).
- If you haven't heard back in two days, ping the pull request or issue.
- Security reports are acknowledged within 48 hours ([SECURITY.md](../../SECURITY.md)).
- Who merges, and what happens if the maintainer is away, is under "Community and governance" in the [README](../../README.md).

## Licensing of contributions

Everything you contribute to this repository is under the MIT licence, like the rest of the source: [CONTRIBUTING.md](../../CONTRIBUTING.md) says "MIT license applies to all contributions", and [LICENSE](../../LICENSE) is the text. That covers code, docs, the starter protocols, the pack template and registry entries. There's no separate contributor agreement and no sign-off line to add.

Your own pack is different. It lives in your repository, under the licence you give it there; listing it in the registry doesn't change that. See [Licensing your pack](packs.md#licensing-your-pack).

The source is MIT, and the default downloads, which include WhatsApp, are GPL-3.0 as a whole because of one library they link. MIT allows your code to be part of that. The details are in [docs/licensing.md](../licensing.md).
