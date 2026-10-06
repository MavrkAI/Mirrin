# my-pack

A short line about what these protocols are for. Protocols and a persona for [Mirrin](https://mirrin.app).

## Install

```sh
mirrin protocols add https://github.com/you/mirrin-pack-my-pack
```

Pin it to a release with `#v0.1.0` on the end of the address. Once the pack is in the registry, `mirrin protocols install my-pack` works too, and `mirrin protocols update` shows what changed before applying a new version.

## What's inside

| Protocol | When | Needs | Set in vars.yaml |
|---|---|---|---|
| commute check | weekdays at 7:45 | web, calendar | `origin`, `destination` |

Before it can run, add your values to `~/.mirrin/protocols/vars.yaml`, under the protocol's exact name:

```yaml
commute check:
  origin: 12 Example St, Brunswick
  destination: the office
```

The persona Nova, a calm co-pilot, shows up as `mirrin-pack-my-pack/navigator` in the Persona menu. It never replaces your own personas or the built-in ones.

## Licence

Say which licence the pack is under, and add the `LICENSE` file (step 6 below).

---

## Making this pack your own

This folder is a working pack: `mirrin protocols lint .` passes on it as it is. The full guide is "Make a pack" in the Mirrin docs (`docs/protocols/packs.md`). Delete this section once you're done.

1. **Copy it** into a folder called `mirrin-pack-<name>`, including the hidden `.github` folder, and make that a git repository with a first commit:

   ```sh
   git clone --depth 1 https://github.com/MavrkAI/Mirrin
   cp -R Mirrin/examples/pack-template ~/code/mirrin-pack-<name>
   cd ~/code/mirrin-pack-<name>
   git init
   git add -A
   git commit -m "first version"
   ```

   People will see your protocols as `pack:mirrin-pack-<name>`, because a pack's folder is named after the last part of its address.
2. **Edit `pack.yaml`**: your pack's name (lowercase), a one-line description, you as the author, and this repository's https address as `repo`.
3. **Write your protocols** in `protocols/`, one YAML file each, and delete `commute.yaml`. Only YAML directly in `protocols/` is read. `mirrin protocols new "my idea"` writes a scaffold into your own protocols folder; move it here rather than copy it: while a protocol with the same name stays in your own folder (`mirrin protocols list` shows it as `local`), it quietly wins over the pack's. To keep your own twin out of it, run `export MIRRIN_HOME=$(mktemp -d)` in the terminal first. The format is `docs/protocols.md` in the Mirrin docs.
4. **Keep or delete `personas/`.** Each file there is one persona; `name` and `character` are required. Delete the folder if your pack has none.
5. **Lint it** before every push:

   ```sh
   mirrin protocols lint .
   ```

   Errors fail it; warnings are worth fixing too. The workflow in `.github/workflows/lint.yml` runs the same check on every push and pull request, with the latest Mirrin release and a throwaway twin, so it needs no keys or secrets. Add `MIRRIN_VERSION: v0.3.0` (say) under its `env` to keep it on one release. Until Mirrin's first public release is out, its install step fails; lint on your machine and paste the output into your registry pull request.
6. **Add a `LICENSE` file.** Without one, others get very little permission to copy or change your pack. MIT, the licence of Mirrin's own source, is a good default.
7. **Try it on your own twin**, from the full path (a bare `.` installs it as `pack`):

   ```sh
   mirrin protocols add ~/code/mirrin-pack-<name>
   mirrin protocols check
   ```

   It's a snapshot copy: after a change, `mirrin protocols remove <name>` and add it again. A throwaway twin (`MIRRIN_HOME`, step 3) can't run protocols, and its `check` lists `needs:` lines; run a protocol in chat on your own twin.

   To try an update the way your users get it, install from the git repository instead. Remove the folder copy first, since both install into the same folder, then commit a change and update:

   ```sh
   mirrin protocols remove <name>
   mirrin protocols add file://$HOME/code/mirrin-pack-<name>
   git commit -am "a change"
   mirrin protocols update
   ```
8. **Rewrite this README** for the people who'll install it: what each protocol does, when it runs, what it needs and which vars to set.
9. **Commit and push** to a new, empty repository called `mirrin-pack-<name>` on GitHub, GitLab or Codeberg (`git remote add origin <its address>`, then `git push -u origin HEAD`). **Tag a release** (`v0.1.0`), keep a `CHANGELOG.md`, and don't commit symbolic links: `find . -type l` should print nothing.
10. **List it in the registry** with a pull request to the Mirrin repository, pinned to that commit (`docs/protocols/registry.md`).
