# Get your pack listed

This page is for pack authors who want their pack to show up in `mirrin protocols search`, and for the maintainers who review those requests: it covers the registry entry, the pull request, the review, how new versions reach people, and what happens when a pack is taken down.

The registry is one file, [`registry/index.json`](../../registry/index.json), on the `main` branch of [github.com/MavrkAI/Mirrin](https://github.com/MavrkAI/Mirrin). It lists packs; the packs themselves stay in their authors' repositories. People find them with `mirrin protocols search <term>`, under **Find more** on the Routines page, or by asking their twin ("is there a protocol for the weather?"), and install one by name:

```sh
mirrin protocols install weather-wise
```

Make the pack first, and check that it installs and runs: [packs.md](packs.md).

## The entry

```json
{
  "name": "weather-wise",
  "description": "Weather-aware routines for weekday mornings.",
  "author": "your-handle",
  "repo": "https://github.com/your-handle/mirrin-pack-weather-wise",
  "tags": ["weather", "daily"],
  "version": "0.1.0",
  "commit": "9b8e7d6c5b4a39281706f5e4d3c2b1a098765432"
}
```

| Field | Required | Rules | What it does |
|---|---|---|---|
| `name` | yes | Lowercase, with no spaces, `/` or `#`. Not used by another entry. | What people type: `mirrin protocols install weather-wise`. Make it the same as `name` in your `pack.yaml`, so the Routines page can tell when the pack is installed. |
| `description` | yes | 200 bytes or fewer (about 200 characters in English). | Shown in search results, and searched. |
| `repo` | yes | Starts with `https://github.com/`, `https://gitlab.com/` or `https://codeberg.org/`, with no `#`. | Where the pack is cloned from. |
| `commit` | for review, yes | A full commit hash: 40 characters, or 64 in a SHA-256 repository. | The exact commit people get. See below. |
| `author` | no | | Who made it. |
| `tags` | no | | A few topic words. Searched, and shown in results. |
| `version` | no | | The version at that commit, for people. Nothing compares versions. |

Search looks for what's typed as one phrase, ignoring case, in `name`, `description` and `tags`. So `mirrin protocols search morning weather` wouldn't find the entry above, which has both words but not side by side. Write the description with the words people will type, and use single-word tags they're likely to search for.

`go test ./internal/protocols/` checks the rules in the third column (the test is `TestRegistryIndexIsValid`), and CI runs it on every pull request. It doesn't fetch your pack: people do that in review.

### Pin a commit, and why

With `commit`, `mirrin protocols install` gets exactly the commit that was reviewed, whatever you push afterwards. People who installed from the registry then follow the entry, not your branch: a new version reaches them only when a pull request moves `commit`, and a reviewer has read the change.

Without `commit`, installs and updates follow your default branch. Whatever you push next reaches everyone who has the pack at their next update, unreviewed. The test allows an entry without one, but reviewers ask for a commit on every new entry and every update.

Use the full hash (`git rev-parse HEAD`). A short one fails to install. Keep that commit reachable: don't force-push over it or delete the branch or tag that holds it, or installs fail with `that tag, branch or commit isn't there (a commit needs all 40 characters)`.

## Before you open the pull request

In your pack's repository, at the commit you'll list:

```sh
git checkout 9b8e7d6c5b4a39281706f5e4d3c2b1a098765432
mirrin protocols lint .
find . -type l          # prints nothing: no symbolic links
```

Lint must report no errors; paste its output into the pull request. Once Mirrin's first public release is out, the pack's lint workflow must be green at that commit too. Until then its install step fails, so it can't be ([The lint workflow](packs.md#the-lint-workflow)). Then go through [What reviewers check](#what-reviewers-check) yourself: it's the list the reviewer will use.

## Open the pull request

1. Fork [MavrkAI/Mirrin](https://github.com/MavrkAI/Mirrin) and make a branch.
2. Add one entry to the `packs` list in `registry/index.json`, at the end. Keep the file valid JSON (a comma after the entry before yours).
3. If you have Go, run the registry test from your clone of the repository. It's optional: CI runs the same test on your pull request.

   ```sh
   MIRRIN_HOME=$(mktemp -d) go test ./internal/protocols/ -run TestRegistryIndexIsValid
   ```

4. Open a pull request against `main` with the registry template. GitHub only fills it in when the address asks for it, so open this one, with your GitHub name and branch in place of the two placeholders:

   ```
   https://github.com/MavrkAI/Mirrin/compare/main...YOUR-NAME:YOUR-BRANCH?quick_pull=1&template=registry.md
   ```

   Or open the pull request as usual and copy the checklist from [`.github/PULL_REQUEST_TEMPLATE/registry.md`](../../.github/PULL_REQUEST_TEMPLATE/registry.md) into the description.
5. Fill it in and tick what you've checked.

One pack per pull request. A maintainer reviews every change under `registry/` (`CODEOWNERS` asks for it), and only a maintainer merges.

## What reviewers check

Reviewers clone the pack at the listed commit and read all of it. Authors: check these yourself first.

**The entry**

- [ ] One entry added or changed, and `registry/index.json` is still valid JSON. CI is green.
- [ ] `name` is lowercase, has no spaces, `/` or `#`, isn't taken, and equals `name` in the pack's `pack.yaml`.
- [ ] `repo` is on GitHub, GitLab or Codeberg over https, opens, and is public.
- [ ] The last part of `repo` isn't the same as another entry's. Two packs whose addresses end alike install into the same folder, so only one could be installed.
- [ ] `commit` is a full hash, exists in that repository, and is on a branch or tag.
- [ ] `description` says plainly what the pack does, in 200 bytes or fewer, and matches what the protocols really do.

**The pack, at that commit**

- [ ] `pack.yaml` has `name`, `description` and `repo`, and `repo` is this repository.
- [ ] `mirrin protocols lint .` reports no errors. Warnings are explained or fixed.
- [ ] Protocols are in `protocols/`, and there's no other YAML at the top level that would be read as one.
- [ ] No symbolic links anywhere (`find . -type l` prints nothing), and no file called `.mirrin-pack.json` (Mirrin writes that itself when it installs a pack).
- [ ] A `LICENSE` file that lets people use the pack, and a README that says what each protocol does, when it runs, what it needs and which vars to set.

**Every protocol**

- [ ] The prompt does what the description says, and nothing else. Read every word.
- [ ] `requires` lists every skill or tool the prompt relies on.
- [ ] It doesn't send what the twin reads (mail, calendar, files, memory, the owner's details) to an address in the prompt or a var default, including in a `fetch_url` address.
- [ ] It doesn't tell the twin to approve things, skip or rush approvals, add anything to `always_allow`, install packs, create protocols or tools, or keep anything from the owner.
- [ ] It doesn't tell the twin to follow instructions found in a page, email or file.
- [ ] Var defaults are harmless placeholders, with no credentials, personal data or addresses that collect data.
- [ ] A schedule runs no more often than the job needs, and a scheduled prompt replies `NOTHING_TO_REPORT` when there's nothing worth saying.
- [ ] Anything that sends, books, pays or deletes is something the owner would expect from the description. Those actions still ask, but nobody should be surprised by the question.
- [ ] No hidden text: nothing odd in invisible characters, very long lines, or a prompt far longer than the job needs.

**Every persona**

- [ ] `name` and `character` are there, the id doesn't clash with a built-in one, and it describes a character, not a real person.
- [ ] The character doesn't instruct the twin to act, only how to speak.

**For an update**, reviewers read the difference between the old and new commits in full (`git diff <old> <new>`), with the same lists.

The full safety model, and more on what to look for, is in [security.md](security.md#red-flags-for-reviewers).

## Release a new version

1. Push your changes, update your changelog, and tag the release (`v0.2.0`).
2. Open a pull request that changes `commit` (and `version`) in your entry. Say what changed.
3. Once it's merged and in a Mirrin release (see [When a merged change reaches people](#when-a-merged-change-reaches-people)), `mirrin protocols update` offers the new commit to everyone who installed your pack from the registry, with the list of files that changed, and applies it when they say yes. The Routines page shows it as an update too.

People who installed your pack by its address, rather than from the registry, follow their own pin instead (see [packs.md](packs.md#pinning)).

Don't rename your entry: people who installed it under the old name are told it is no longer in the registry. If you move the repository, change `repo` in a pull request; people who have the pack are told `the registry now lists it at <repo>; remove it and install it again to follow`.

### When a merged change reaches people

The published index is meant to be signed, and Mirrin uses it only when the signature checks out (see [Signing](#signing)). The signing key isn't in the repository yet, so for now every release answers search, install and update from the copy of the index built into it. Until the key is added, a new entry or a commit bump reaches people with the next Mirrin release, not the moment it's merged.

## Signing

This part is for maintainers. Contributors never sign anything and never touch the signature file.

- **What is signed.** The `registry-sign` workflow signs `registry/index.json` with [minisign](https://jedisct1.github.io/minisign/) whenever the file changes on `main`, and commits the signature beside it as `registry/index.json.minisig`. The signature covers the exact bytes of `index.json`, plus a trusted comment, `mirrin pack index <short commit>`.
- **What checks it.** Mirrin fetches both files and uses the published index only when the signature checks out against the public key built into it (`registry/minisign.pub`). A missing or bad signature, or a key it doesn't know, means it answers from its built-in copy instead.
- **What it doesn't cover.** Packs and protocols aren't signed. What ties a registry name to reviewed content is the `commit` in the signed index: git checks that what it fetches is that commit. That's why reviewers ask for one.
- **A registry you set yourself** with `protocol_registry` isn't checked against any key.
- **Setting it up, once.** Delete the placeholder `registry/minisign.pub` first (it holds only comments, and minisign won't write over an existing file without `-f`). Then run `minisign -G -W -p registry/minisign.pub -s mirrin-registry.key`, commit the two-line `registry/minisign.pub` it writes, and store the secret key file's contents as the `REGISTRY_MINISIGN_KEY` repository secret. The workflow pushes to `main`, so branch protection there must let `github-actions` push. Check a signature by hand with `minisign -V -p registry/minisign.pub -m registry/index.json`.
- **Status.** The key hasn't been added yet (planned before the next release). `MIRRIN_RELEASE_CHECK=1 go test ./internal/protocols/ -run TestReleaseHasRegistryKey` fails until it is.

## Offline, and on first run

Every Mirrin release carries a copy of `index.json` as it was when it was built. When the published index can't be reached, or its signature doesn't check out, search, install and update answer from that copy. So search works offline and on first run, and a tampered index is never used.

## Run your own index

Point `protocol_registry` in `~/.mirrin/config.yaml` at another index:

```yaml
protocol_registry: https://example.org/mirrin/index.json
```

- It's the same shape as `registry/index.json`: `{"packs": [ … ]}` with the fields above.
- It must be `https://`. Plain `http://` is allowed only to this computer (`localhost`, `127.0.0.1`), and a redirect away from https is refused.
- It's read with a 15-second timeout, up to 4 MiB.
- It isn't checked against any signature: you trust whoever serves it.
- If it can't be read, search says `couldn't read the pack registry at <address> (<why>); check protocol_registry in config.yaml`. It doesn't fall back to the default index.
- Packs installed from it remember it, and updates follow its entries, even if you change `protocol_registry` later.

Leave it empty (`protocol_registry: ""`) to use the Mirrin registry.

## Removal and deprecation

To retire your pack, open a pull request that removes its entry. Maintainers also remove entries for packs that are harmful, whose repository has gone, or that no longer do what they say.

Removing an entry doesn't uninstall anything. Mirrin has no way to switch a pack off on someone's machine. People who have the pack keep it, and their next `mirrin protocols update` says: `no longer in the pack registry, so it was left as it is; remove it if you don't want it any more`. Releases already out still carry it in their built-in copy, which answers whenever the published index can't be used, until people update Mirrin.

When only the latest version is the problem, maintainers can point `commit` back at the last good one instead: `update` then offers everyone the way back, with the files that change.

## Report a harmful pack

- **If a pack could hurt someone** (it sends people's data somewhere, tries to get round approvals, or acts without asking), report it privately: **Security → Report a vulnerability** on [MavrkAI/Mirrin](https://github.com/MavrkAI/Mirrin/security/advisories/new). See [SECURITY.md](../../SECURITY.md).
- **If it's broken or misleading**, open a **Report a pack** issue.

What maintainers do with a report is in [security.md](security.md#takedown).
