package protocols

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A pack that arrives with an identity import is a copy: .git never travels,
// since it can carry hooks. Its provenance file still says where it came
// from, so it can be cloned again from there and swapped in for the copy,
// after which it updates like a pack installed here.

// checkCopiedPack is checkPack for a copy whose provenance names a git
// address: To is the commit it would be cloned at, and applying the update
// swaps the clone in for the copy.
func checkCopiedPack(ctx context.Context, u PackUpdate, pv Provenance, index func(string) (*Registry, error)) PackUpdate {
	u.Reconnect, u.Source, u.From = true, pv.Source, pv.Commit
	if pv.Commit != "" && !reCommit.MatchString(pv.Commit) {
		u.From = ""
	}
	ref := pv.Ref
	switch {
	case pv.Registry != "":
		if ref, u.Note, u.Err = registryRef(pv, index); u.Note != "" || u.Err != nil {
			return u
		}
	case !validRef(ref):
		u.Err = fmt.Errorf("its %s names %q, which isn't a tag, branch or commit; remove the pack and add it again", ProvenanceFile, ref)
		return u
	}
	if reCommit.MatchString(ref) {
		u.To = ref
	} else {
		u.To, u.Err = remoteCommit(ctx, pv.Source, ref)
	}
	if u.Err == nil {
		u.Changes = []string{"reconnects it to " + pv.Source + ", so it updates again"}
	}
	return u
}

// remoteCommit is the commit ref ("" for the default branch) names in repo,
// read without cloning it.
func remoteCommit(ctx context.Context, repo, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	out, err := runGit(ctx, "", "ls-remote", "--", repo, ref)
	if err != nil {
		return "", err
	}
	var first string
	for _, line := range strings.Split(out, "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || !reCommit.MatchString(sha) {
			continue
		}
		if strings.HasSuffix(name, "^{}") {
			return sha, nil // an annotated tag, peeled to its commit
		}
		if first == "" {
			first = sha
		}
	}
	if first == "" {
		return "", errors.New("that tag, branch or commit isn't there (a commit needs all 40 characters)")
	}
	return first, nil
}

// ReconnectPack clones pv.Source at commit (or, when commit is "", at pv.Ref
// or the default branch) and puts the clone in place of dir/packs/<name>,
// recording pv's ref and registry entry, so a pack that arrived as a copy
// updates again. The copy stays until the clone is complete, so a failure
// leaves it as it was. A registry pack is installed the way AddRegistryPack
// does, any other the way AddPack does.
func ReconnectPack(ctx context.Context, dir, name string, pv Provenance, commit string) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) || !filepath.IsLocal(name) {
		return fmt.Errorf("%q isn't a pack folder", name)
	}
	if err := checkGitURL(pv.Source); err != nil {
		return err
	}
	if !validRef(pv.Ref) {
		return fmt.Errorf("%q isn't a tag, branch or commit name", pv.Ref)
	}
	if commit != "" && !reCommit.MatchString(commit) {
		return fmt.Errorf("%q isn't a commit", commit)
	}
	if pv.Registry != "" {
		pv.Ref = "" // the registry entry decides, not a pin of the user's
	}
	target := commit
	if target == "" {
		target = pv.Ref
	}
	if err := os.MkdirAll(PacksDir(dir), 0o700); err != nil {
		return err
	}
	dest := filepath.Join(PacksDir(dir), name)
	stage := filepath.Join(PacksDir(dir), "."+name+".installing")
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	got, err := cloneAt(ctx, pv.Source, target, stage)
	if err == nil && commit != "" && got != commit {
		err = fmt.Errorf("got commit %s instead of %s", ShortCommit(got), ShortCommit(commit))
	}
	now := time.Now().UTC()
	if pv.Installed.IsZero() {
		pv.Installed = now
	}
	pv.Commit, pv.Updated = got, now
	if err == nil {
		err = writeProvenance(stage, pv)
	}
	if err != nil {
		os.RemoveAll(stage)
		return err
	}
	return swapIn(stage, dest)
}

// swapIn puts the folder stage at dest, replacing what is there only once
// the move can't leave dest missing.
func swapIn(stage, dest string) error {
	old := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".replaced")
	if err := os.RemoveAll(old); err != nil {
		os.RemoveAll(stage)
		return err
	}
	had := true
	if err := os.Rename(dest, old); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			os.RemoveAll(stage)
			return err
		}
		had = false
	}
	if err := os.Rename(stage, dest); err != nil {
		if had {
			_ = os.Rename(old, dest)
		}
		os.RemoveAll(stage)
		return err
	}
	return os.RemoveAll(old)
}
