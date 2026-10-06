# Protocol registry

`index.json` is the list `mirrin protocols search` reads. To list a pack, open a pull request that adds one entry:

```json
{
  "name": "commute",
  "description": "Drive-time check before your first meeting.",
  "author": "you",
  "repo": "https://github.com/you/mirrin-pack-commute",
  "tags": ["travel", "daily"],
  "version": "0.1.0",
  "commit": "3f2a1c9e8b7d6e0f1a2b3c4d5e6f7a8b9c0d1e2f"
}
```

Rules, enforced by `go test ./internal/protocols/` in CI: lowercase name without spaces, description under 200 characters, an https repository URL on GitHub, GitLab or Codeberg, unique names. `commit` is optional and, when given, is a full commit hash: `mirrin protocols install` then gets exactly the commit that was reviewed. Bump it in a pull request to release a new version; `mirrin protocols update` then offers the new commit, with the files that changed, to everyone who installed the pack from the registry. Without `commit`, installs and updates follow the repository's default branch.

A pack is a repository with `pack.yaml` and a `protocols/` folder, plus an optional `personas/` folder; start from `examples/pack-template`, which includes a lint workflow. Only YAML in `protocols/` is read as protocols. Pack personas show up as `<pack>/<id>` and never replace the user's own or the bundled ones. Keep prompts free of credentials and personal data: users add those through `vars`.

This file is also built into Mirrin (`registry.go`), so search works offline and before the published copy is reachable. Each release carries the index as it was when it was built.

## Signature

The published `index.json` is signed: the `registry-sign` workflow signs it with minisign each time it changes on `main` and commits `index.json.minisig` beside it. Mirrin fetches both and uses the published index only when the signature checks out against the public key built into it (`minisign.pub`). A missing or bad signature, or a version built before the key was added, answers from the built-in index instead. A registry you set yourself with `protocol_registry` is yours and isn't checked.

Maintainers set the key up once. Delete the placeholder `minisign.pub` first (it holds only comments, and minisign won't write over an existing file without `-f`), then run `minisign -G -W -p registry/minisign.pub -s mirrin-registry.key`, commit the two-line `minisign.pub` it writes, and store the secret key file's contents as the `REGISTRY_MINISIGN_KEY` repository secret. Check a signature by hand with `minisign -V -p registry/minisign.pub -m registry/index.json`.

Until the key is committed, every release answers `protocols search`, `install` and `update` from its built-in index only: new packs and commit bumps in the published index don't reach anyone. Add the key, and let the workflow commit `index.json.minisig`, before tagging the next release; `MIRRIN_RELEASE_CHECK=1 go test ./internal/protocols/ -run TestReleaseHasRegistryKey` fails until then. The workflow pushes to `main`, so branch protection there must let `github-actions` push.
