# Make a pack

This page is for anyone who has written a protocol and wants to share it: by the end you'll have a pack repository that people can install, pin to a version and update.

A pack is a git repository of protocols, and optionally personas, that someone else can add to their twin with one command. The protocol format itself is in [docs/protocols.md](../protocols.md). To have your pack show up in `mirrin protocols search`, list it in the registry once it works: [registry.md](registry.md).

## The layout

```
mirrin-pack-weather-wise/
  pack.yaml                    who made it and where it lives
  protocols/
    rain-check.yaml            one protocol per file
  personas/                    optional
    forecaster.yaml            one persona per file
  README.md                    for people; Mirrin doesn't read it
  CHANGELOG.md                 for people; Mirrin doesn't read it
  LICENSE
  .github/workflows/lint.yml   checks every push (from the template)
```

Name the repository `mirrin-pack-<name>`. The folder a pack installs into is named after the last part of its address, so a distinctive name avoids clashes (see [Where a pack lands](#where-a-pack-lands)).

## What Mirrin reads, and what it ignores

| In the pack | What Mirrin does with it |
|---|---|
| `pack.yaml` | Reads the pack's name and description. Lint checks it. |
| `protocols/*.yaml`, `protocols/*.yml` | Loads each file as a protocol. Only files directly in `protocols/` are read, not subfolders. |
| `personas/*.yaml`, `personas/*.yml` | Loads each file as a persona. |
| Everything else | Ignored: README, LICENSE, CHANGELOG, CI files, other folders, and any file whose name starts with a dot. |

A few details:

- A `vars.yaml` in a pack is ignored. People keep their own values in `~/.mirrin/protocols/vars.yaml`, so a pack can't fill them in for them.
- A pack with no `protocols/` folder has the YAML at its top level read instead (the older layout). Then any other YAML there, such as `mkdocs.yml`, would be read as a protocol too. Always use `protocols/`.
- Keys Mirrin doesn't know are ignored without a warning, in `pack.yaml` and in protocols. A typo such as `scheudle:` is simply dropped, so check spellings.
- A file that isn't a usable protocol is skipped, and the rest still load. `mirrin protocols check` lists skipped files and why, and the twin tells its owner once about each one.

## pack.yaml

```yaml
name: weather-wise
description: Weather-aware routines for weekday mornings.
author: your-handle
repo: https://github.com/your-handle/mirrin-pack-weather-wise
tags: [weather, daily]
version: 0.1.0
```

| Field | Lint requires it | What it's for |
|---|---|---|
| `name` | yes | The pack's name in `mirrin protocols list`. People can remove the pack by it: `mirrin protocols remove weather-wise`. Use the same name as your registry entry: the Routines page compares the two to mark a pack as installed. |
| `description` | yes | One line on what the pack is for. |
| `repo` | yes | The repository's https address. Lint uses it to tell you the name your personas will show up under. |
| `author` | no | You, or your handle. |
| `tags` | no | A few words for the topic. |
| `version` | no | The pack's version, for people. Nothing in Mirrin compares versions. |

When `pack.yaml` exists, lint reports `pack.yaml needs name, description and repo` if any of those three is missing. A pack without a `pack.yaml` still installs, but it shows up under its folder name with no description, so always include one.

## Where a pack lands

Each pack is installed into `~/.mirrin/protocols/packs/<folder>/`. The folder name comes from the address it was installed from, not from `pack.yaml`: the last part of the address, lowercased, with `.git` dropped and anything other than letters, digits, `.`, `_` and `-` turned into `-`.

| Installed from | Folder |
|---|---|
| `https://github.com/you/Mirrin-Pack-Weather-Wise.git` | `mirrin-pack-weather-wise` |
| `git@github.com:you/weather.git` | `weather` |
| `~/code/mirrin-pack-weather-wise` | `mirrin-pack-weather-wise` |

That folder name is what people see: protocols show as `pack:mirrin-pack-weather-wise` in `mirrin protocols list`, and personas as `mirrin-pack-weather-wise/<id>`. Two repositories that end in the same name can't both be installed: the second gets ``pack "<folder>" is already installed (use `mirrin protocols update`)``.

## Start from the template

[`examples/pack-template`](../../examples/pack-template) is a working pack with one protocol, one persona and a lint workflow. Copy it out of the Mirrin repository into a folder of its own, and make that folder a git repository with a first commit:

```sh
git clone --depth 1 https://github.com/MavrkAI/Mirrin
cp -R Mirrin/examples/pack-template ~/code/mirrin-pack-weather-wise
cd ~/code/mirrin-pack-weather-wise
git init
git add -A
git commit -m "first version"
```

`cp -R` brings the hidden `.github` folder with it, which holds the lint workflow. Saving the files one at a time from GitHub's website tends to leave it behind. You can delete the `Mirrin` clone afterwards.

Then make it yours:

1. Edit `pack.yaml`: your pack's name, a description, you as the author, and the repository's address as `repo`.
2. Replace `protocols/commute.yaml` with your own protocols, one per file. To start one from a scaffold, run `mirrin protocols new "rain check"`. It writes the file into your twin's own protocols folder and prints where; move it from there into your pack's `protocols/`. Move it rather than copy it: while a protocol with the same name stays in your own folder (`mirrin protocols list` shows it as `local`), it quietly wins over the pack's, and you would be testing the wrong one. To keep your own twin out of it altogether, start with a [throwaway twin](#a-throwaway-twin).
3. Keep `personas/` if your pack offers a character, and delete it if not.
4. Rewrite `README.md` for the people who will install it: what each protocol does, when it runs, what it needs and which vars to set.
5. Add a `LICENSE` file (see [Licensing your pack](#licensing-your-pack)).
6. Commit, create an empty repository called `mirrin-pack-<name>` on GitHub, GitLab or Codeberg, and push to it:

   ```sh
   git add -A
   git commit -m "weather-wise 0.1.0"
   git remote add origin https://github.com/you/mirrin-pack-weather-wise.git
   git push -u origin HEAD
   ```

   The lint workflow then runs on every push and pull request. Until Mirrin's first public release is out it can't pass yet: see [The lint workflow](#the-lint-workflow).

## Personas in a pack

A pack can offer characters as well as routines. The persona format, and how people switch to one, are in [docs/personas.md](../personas.md). In a pack:

- Each `personas/*.yaml` file is one persona. Only `name` and `character` are required; lint reports `name and character are required` otherwise, and warns when `tagline`, `version` or `author` is empty.
- Its id is the `id:` field, or the file name without `.yaml`. Installed, it shows up as `<folder>/<id>` in the Persona menu and in `mirrin persona list`, next to the person's own personas and the built-in ones.
- A pack persona never replaces the person's own or a built-in one. If your id matches a built-in one (`mirrin`, `pickoo`, `nyra`), lint warns, because a distinct id reads better.
- When a persona is clean, lint tells you how it will appear, for example `persona Nova shows up as mirrin-pack-my-pack/navigator in the Persona menu`.
- Keep personal data out. A persona describes a character, not a person.

## Test it on your machine

### A throwaway twin

Scaffolding a protocol, installing a pack and setting vars all write into your twin's home, `~/.mirrin`. To keep your own routines and settings out of the way, give the terminal a throwaway twin before you start:

```sh
export MIRRIN_HOME=$(mktemp -d)
```

Every `mirrin` command in that terminal then uses the new empty folder, and your own twin isn't touched. A throwaway twin has no model and nothing connected, so it can lint, install, list and check, but it can't run a protocol, and `check` reports `✗ … needs: …` for what your protocols require. That's expected. To run one, install the pack on your own twin, in a terminal without `MIRRIN_HOME` set.

### Lint

From the pack's folder:

```sh
mirrin protocols lint .
```

Each finding prints as `<file>: <level>: <message>`, then a count. Errors (a missing prompt, a schedule cron can't read, a `{{var}}` the protocol doesn't declare, anything that looks like a credential) make it exit with status 1. Warnings (no description, no `NOTHING_TO_REPORT` in a scheduled prompt, a var that's never used) don't, but fix them before you share.

Lint checks `requires` against skill names and the tools your twin has. A tool your twin doesn't have, such as `gmail_search` in a twin without Gmail, gets a warning. A throwaway twin has no accounts connected, so in `requires` prefer skill names (`email`, `calendar`), which lint always knows. If lint can't start your twin to ask, it checks everything else and skips `requires`. A config that won't load stops lint altogether, with the config's error; a throwaway twin avoids that.

### Install it

Then install it, as a copy, and see what your twin makes of it:

```sh
mirrin protocols add ~/code/mirrin-pack-weather-wise
mirrin protocols list
mirrin protocols check
```

- Give `add` the full path. A bare `.` installs the pack under the folder name `pack`.
- `check` prints one line per protocol: `✓ rain check  ok`, `✗ rain check  needs: calendar` for something this twin hasn't connected, or `! rain check  set vars in …/vars.yaml: city` for a var with no value and no default. It doesn't test schedules; lint does. It always exits 0, so read what it says.
- Set your vars the way your users will, under the protocol's exact name (case matters), in `protocols/vars.yaml` in your twin's home (`~/.mirrin/protocols/vars.yaml`, or under `$MIRRIN_HOME` in a throwaway twin):

  ```yaml
  rain check:
    city: Melbourne
  ```

- Run it on your own twin: say "run rain check" in chat (the twin asks before running it), or press **Run now** on the Routines page. A scheduled protocol also runs at its next due time.
- A folder is copied as a snapshot, without `.git` and without symbolic links, so it can't be updated. To try a change, remove it and add it again:

  ```sh
  mirrin protocols remove weather-wise
  mirrin protocols add ~/code/mirrin-pack-weather-wise
  ```

### Try an update the way your users get it

To see exactly what your users will see when you ship an update, install the pack from its git repository on disk, with a `file://` address. The folder must be a git repository with at least one commit (see [Start from the template](#start-from-the-template)), and only committed changes reach the installed copy. Remove the folder copy first: both install into the same folder, so `add` would refuse with ``pack "mirrin-pack-weather-wise" is already installed (use `mirrin protocols update`)``.

```sh
mirrin protocols remove weather-wise
mirrin protocols add file:///Users/you/code/mirrin-pack-weather-wise
```

Then change something, commit it, and update:

```sh
git commit -am "rain check: a shorter reply"
mirrin protocols update
```

`update` shows the old and new commit and the files that changed, as it will for your users, and applies the change when you say yes.

`add`, `remove` and `update` reload a running twin themselves. After editing a file by hand anywhere else, say `/reload` in chat.

## How people install it

| From | Command | Notes |
|---|---|---|
| An https address | `mirrin protocols add https://github.com/you/mirrin-pack-weather-wise` | The usual way. |
| An ssh address | `mirrin protocols add git@github.com:you/mirrin-pack-weather-wise.git` | Also `ssh://…`. git runs ssh in batch mode, so the host must already be known and the key must work without a prompt. |
| A folder | `mirrin protocols add ~/code/mirrin-pack-weather-wise` | A snapshot copy. It can't be pinned or updated. |
| The registry | `mirrin protocols install weather-wise` | Once it's listed: [registry.md](registry.md). |

People can also paste a pack's git address into **Find more** on the Routines page, or ask their twin ("is there a protocol for the weather?"), which searches the registry and can show what a pack would add before installing it.

Installing needs git for anything but a folder (`git is required to install packs from a repository`). Plain `http://` is refused: `<address> uses plain http, which can be tampered with on the way; use its https:// address`. A repository that needs a password over https fails with `the repository doesn't exist or isn't public`, since git is never allowed to stop and ask for one.

## Pinning

Add `#` and a tag, branch or full commit to the address:

```sh
mirrin protocols add https://github.com/you/mirrin-pack-weather-wise#v0.2.0
mirrin protocols add https://github.com/you/mirrin-pack-weather-wise#main
mirrin protocols add https://github.com/you/mirrin-pack-weather-wise#9b8e7d6c5b4a39281706f5e4d3c2b1a098765432
```

| Installed with | What `mirrin protocols update` does |
|---|---|
| A commit | Stays there, and says `pinned to commit 9b8e7d6; to move it, remove it and add it again with #<tag or commit>`. |
| A tag or branch | Follows it. |
| No pin | Follows the repository's default branch. |
| `mirrin protocols install` (the registry) | Follows the commit the registry entry lists. |

A commit has to be all 40 characters (64 in a SHA-256 repository). A short hash is taken as a tag or branch name and fails with `that tag, branch or commit isn't there (a commit needs all 40 characters)`. A name that git could mistake for an option is refused: `"<ref>" isn't a tag, branch or commit name`.

Whatever it was pinned to, a pack stays on the commit it was installed at until an update is applied.

## Updates: what your users see

`mirrin protocols update` checks every installed pack without changing anything, lists what differs, and asks before applying:

```
mirrin-pack-weather-wise: 3f2a1c9 → 9b8e7d6
    changed protocols/rain-check.yaml
    added protocols/heat-check.yaml
    full diff: git -C /Users/you/.mirrin/protocols/packs/mirrin-pack-weather-wise diff 3f2a1c9 9b8e7d6

apply these updates? [y/N]
```

- Nothing changes until they say yes. `--yes` (or `-y`) applies without asking. With no terminal to ask at, it prints ``nothing changed yet. Run `mirrin protocols update --yes` to apply.``
- On the Routines page, **Check for updates** shows "Update ready" with the commits and the changed files, and **Update** applies it.
- They see file names, not your reasons. Say what changed, in words, in your changelog.
- If someone changes the schedule of one of your protocols, or turns it off, from chat or the Routines page, their twin saves their own copy of that protocol. That copy wins over yours and no longer follows your updates. The approval and the page both tell them so.

Updates that don't break things:

- **Keep protocol names stable.** People's values in `vars.yaml` are keyed by the exact name, and their own copies shadow yours by name. Renaming a protocol loses both.
- **Give new vars a default.** A protocol with a `{{var}}` that has no value and no default doesn't run: the twin skips it and tells the person what to add to `vars.yaml`.
- **Removing a protocol removes it for everyone** at their next update, schedule and all.

## Symbolic links

Don't commit symbolic links. Mirrin never loads a protocol or persona through a link in a pack: a linked `protocols/` or `personas/` folder, or a linked file in either, is skipped with `symbolic links are not loaded from packs`, and lint reports a linked folder as an error. Lint doesn't catch a linked file, though, so before you push, run `find . -type l` in the pack's folder: it should print nothing. A copy from a folder leaves links out. Reviewers treat any link in a pack as a red flag ([security.md](security.md)).

## The lint workflow

`.github/workflows/lint.yml` in the template runs on every push and pull request. It:

1. installs the latest Mirrin release with the official install script, which checks the download against the release's checksums;
2. makes a throwaway twin with no keys and no channels;
3. runs `mirrin protocols lint .` in your repository.

It needs no secrets. It fails on errors and passes with warnings, which still show in the log. To keep it on one Mirrin release, add `MIRRIN_VERSION: v0.3.0` (say) under `env` in the install step.

**It can't pass yet.** The install step needs Mirrin's first public release, which isn't out, so until then the workflow fails at that step on every run. Keep the workflow anyway, and in the meantime run `mirrin protocols lint .` on your machine and paste its output into your registry pull request. Once the release is out, reviewers look for a green run at the commit you ask to list.

## Versions and changelogs

- Give each protocol and persona a `version`, and the pack one in `pack.yaml`. Use semver: a patch for wording, a minor version for a new protocol, a major version when people have to do something (a new var without a default, a renamed protocol, a different schedule). Lint warns when a protocol's or persona's version is empty. Nothing in Mirrin compares versions; they are for people.
- Tag each release (`v0.2.0`), so people can pin to it with `#v0.2.0`.
- Keep a `CHANGELOG.md` at the top of the repository. Mirrin ignores it, and it's where people find out what an update means.
- If your pack is in the registry, a new version reaches registry users only when you bump its `commit` in a pull request: [registry.md](registry.md#release-a-new-version).

## Licensing your pack

Mirrin's source is under the MIT licence, but that doesn't cover your pack. A pack in your own repository is under whatever licence you give it. With none, others get only what copyright law and the hosting site's terms allow, which usually isn't enough to copy or change it.

So add a `LICENSE` file before you share it, and say which licence in your README:

- **MIT** matches Mirrin's source. People can use, change and share your protocols, keeping your copyright notice. It's the one we suggest.
- **MIT-0** or **CC0-1.0** if you don't need even the notice.

GitHub, for one, offers a licence picker when you create a file called `LICENSE`. Only put text in your prompts and personas that you have the right to share. A protocol or example contributed into the Mirrin repository itself is MIT like the rest of it ([contributing.md](contributing.md#licensing-of-contributions)).
