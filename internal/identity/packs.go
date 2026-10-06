package identity

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// PackSource is where an installed pack came from. Packs travel as plain
// files (a .git folder can carry hooks that run on the next pull, so it never
// travels); the source says how to reconnect them for updates.
type PackSource struct {
	Dir    string `yaml:"dir"`
	Repo   string `yaml:"repo"`
	Ref    string `yaml:"ref,omitempty"`
	Commit string `yaml:"commit,omitempty"`
	// Registry is the pack's name in the index it was installed from, and
	// Index that index ("" for the default): such a pack follows the commit
	// its entry lists (protocols.AddRegistryPack).
	Registry string `yaml:"registry,omitempty"`
	Index    string `yaml:"index,omitempty"`
}

// provenanceFile, at a pack's root, records where the pack came from as JSON
// (source, ref, commit), the way pack installs record it. Export reads it for
// packs without .git, and import writes it for each pack that arrives as a
// copy, so the source survives every move. It never travels as a file: the
// manifest carries the source, credentials removed. The protocols package
// owns the file; this package reads and writes it through that one type.
const provenanceFile = protocols.ProvenanceFile

type provenance = protocols.Provenance

var (
	// safeRepo is what a pack source may look like: nothing a shell would
	// interpret when the user pastes the reconnect command.
	safeRepo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~:/@%+=-]*$`)
	// scpRepo is git's scp-like form: git@github.com:you/pack.git.
	scpRepo    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:[A-Za-z0-9._~/][A-Za-z0-9._~/-]*$`)
	safeRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	safeCommit = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	safePack   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// remoteRepo reports whether a source is a git address another machine can
// reach, rather than a folder on this one, and one `mirrin protocols add`
// accepts (protocols.checkGitURL): https, ssh or scp-style, never plain
// http or git://, which can be tampered with on the way.
func remoteRepo(s string) bool {
	if scheme, _, ok := strings.Cut(s, "://"); ok {
		switch strings.ToLower(scheme) {
		case "https", "ssh":
			return true
		}
		return false
	}
	return scpRepo.MatchString(s)
}

// valid reports whether a source read from an archive is safe to write and show.
func (p PackSource) valid() bool {
	return p.Dir != "" && !strings.HasPrefix(p.Dir, ".") && !strings.ContainsAny(p.Dir, `/\`) &&
		filepath.IsLocal(p.Dir) && safeRepo.MatchString(p.Repo) && remoteRepo(p.Repo) &&
		(p.Ref == "" || safeRef.MatchString(p.Ref) && !strings.Contains(p.Ref, "..")) &&
		(p.Commit == "" || safeCommit.MatchString(p.Commit)) &&
		(p.Registry == "" || safePack.MatchString(p.Registry)) &&
		(p.Index == "" || safeRepo.MatchString(p.Index) && strings.HasPrefix(strings.ToLower(p.Index), "https://"))
}

// Reconnect is the command that makes a pack that arrived as a copy update
// again, pinned where it was: through its index entry for a registry pack,
// else from its repository at the ref (or commit) it was installed at.
func (p PackSource) Reconnect() string {
	if p.Registry != "" {
		return "mirrin protocols install " + p.Registry
	}
	src := p.Repo
	switch {
	case p.Ref != "":
		src += "#" + p.Ref
	case p.Commit != "":
		src += "#" + p.Commit
	}
	return "mirrin protocols add " + src
}

// packSources lists where the packs under protocolsDir came from: the
// provenance file, then git's origin and commit for packs installed with git,
// with credentials removed. Packs copied from a folder on this machine have
// no source another machine could use and are left out.
func packSources(protocolsDir string) []PackSource {
	entries, err := os.ReadDir(filepath.Join(protocolsDir, "packs"))
	if err != nil {
		return nil
	}
	_, gitErr := exec.LookPath("git")
	var out []PackSource
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		root := filepath.Join(protocolsDir, "packs", e.Name())
		var p PackSource
		if pv, ok := protocols.ReadProvenance(root); ok {
			p = PackSource{Repo: pv.Source, Ref: pv.Ref, Commit: pv.Commit, Registry: pv.Registry, Index: pv.Index}
		}
		if _, err := os.Stat(filepath.Join(root, ".git")); err == nil && gitErr == nil {
			if repo := git(root, "config", "--get", "remote.origin.url"); repo != "" {
				p.Repo = repo
			}
			if commit := git(root, "rev-parse", "HEAD"); commit != "" {
				p.Commit = commit
			}
		}
		p.Dir, p.Repo = e.Name(), stripURLCredentials(p.Repo)
		if p.valid() {
			out = append(out, p)
		}
	}
	return out
}

// git runs git in a pack's folder. It never looks above it: a pack whose
// .git is damaged must not report the outer repository's origin and commit
// (a home kept in git) as its own.
func git(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	if abs, err := filepath.Abs(dir); err == nil {
		cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(abs))
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// provenanceJSON is the provenance file for a pack that arrived as a copy.
func (p PackSource) provenanceJSON() []byte {
	b, _ := json.MarshalIndent(p.provenance(), "", "  ")
	return append(b, '\n')
}

// provenance is where the pack came from, as its provenance file says it.
func (p PackSource) provenance() provenance {
	return provenance{Source: p.Repo, Ref: p.Ref, Commit: p.Commit, Registry: p.Registry, Index: p.Index}
}
