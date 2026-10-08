## Add or update a pack in the registry

<!-- One pack per pull request. The guide, with the reviewers' checklist: docs/protocols/registry.md -->

- Pack name (as in the entry and in `pack.yaml`):
- Repository:
- Commit (all 40 characters):
- New pack, or a new version? For a new version, the commit it replaces:
- What it does, in a sentence or two:
- For a new version, what changed:
- Output of `mirrin protocols lint .` at that commit:

### The entry

- [ ] I added or changed one entry in `registry/index.json`, and it is still valid JSON
- [ ] `name` is lowercase, with no spaces, `/` or `#`, isn't used by another entry, and is the same as `name` in the pack's `pack.yaml`
- [ ] `repo` is the public https address on GitHub, GitLab or Codeberg, with no `#`, and its last part isn't the same as another entry's
- [ ] `commit` is the full hash of the commit I want reviewed, on a branch or tag I won't rewrite
- [ ] `description` says plainly what the pack does, in 200 bytes or fewer
- [ ] If I have Go, `MIRRIN_HOME=$(mktemp -d) go test ./internal/protocols/ -run TestRegistryIndexIsValid` passes (CI runs it on this pull request anyway)

### The pack, at that commit

- [ ] `pack.yaml` has `name`, `description` and `repo`
- [ ] `mirrin protocols lint .` reports no errors, and I pasted its output above. Once Mirrin's first public release is out, the pack's lint workflow is green at that commit too (until then its install step fails)
- [ ] Protocols are in `protocols/`, and each one declares in `requires` every skill or tool it uses, and in `vars` every input
- [ ] Each prompt does only what its description says, and nothing it reads is sent to an address in the prompt or a var default
- [ ] No prompt tells the twin to approve things, skip approvals, follow instructions it reads, install packs, create protocols or tools, or keep anything from the owner
- [ ] Scheduled protocols run no more often than they need to and reply `NOTHING_TO_REPORT` when there's nothing to say
- [ ] No credentials or personal data in any prompt, var default or persona
- [ ] No symbolic links (`find . -type l` prints nothing) and no `.mirrin-pack.json`
- [ ] A `LICENSE` file, and a README that says what each protocol does, when it runs, what it needs and which vars to set

<!-- Reviewers: clone the pack at the listed commit and read every file, using "What reviewers check" in docs/protocols/registry.md. For a new version, read `git diff <old> <new>` in full. -->
