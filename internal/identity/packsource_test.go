package identity

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A pack installed from the registry keeps following its index entry after a
// move, and a pinned pack stays pinned: the reconnect command says so.
func TestPackSourcesKeepRegistryAndPin(t *testing.T) {
	src := twinHome(t, nil)
	packs := filepath.Join(src, "protocols", "packs")
	commit := strings.Repeat("cd", 20)
	writeFile(t, filepath.Join(packs, "starter", "a.yaml"), "name: a\nprompt: y\n")
	writeFile(t, filepath.Join(packs, "starter", provenanceFile), `{"source": "https://github.com/x/starter.git", "registry": "starter", "index": "https://example.com/index.yaml", "commit": "`+commit+`"}`)
	writeFile(t, filepath.Join(packs, "pinned", "b.yaml"), "name: b\nprompt: y\n")
	writeFile(t, filepath.Join(packs, "pinned", provenanceFile), `{"source": "https://github.com/x/pinned.git", "ref": "v2.0.0", "commit": "`+commit+`"}`)
	writeFile(t, filepath.Join(packs, "atcommit", "c.yaml"), "name: c\nprompt: y\n")
	writeFile(t, filepath.Join(packs, "atcommit", provenanceFile), `{"source": "git@github.com:x/atcommit.git", "commit": "`+commit+`"}`)
	// Addresses `mirrin protocols add` refuses don't travel as sources.
	writeFile(t, filepath.Join(packs, "plain", "d.yaml"), "name: d\nprompt: y\n")
	writeFile(t, filepath.Join(packs, "plain", provenanceFile), `{"source": "http://example.com/plain.git"}`)
	writeFile(t, filepath.Join(packs, "gitproto", "e.yaml"), "name: e\nprompt: y\n")
	writeFile(t, filepath.Join(packs, "gitproto", provenanceFile), `{"source": "git://example.com/gitproto.git"}`)
	writeFile(t, filepath.Join(packs, "badindex", "f.yaml"), "name: f\nprompt: y\n")
	writeFile(t, filepath.Join(packs, "badindex", provenanceFile), `{"source": "https://github.com/x/f.git", "registry": "f", "index": "http://evil/index.yaml"}`)

	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	m, err := Export(src, out, false)
	if err != nil {
		t.Fatal(err)
	}
	byDir := map[string]PackSource{}
	for _, p := range m.Packs {
		byDir[p.Dir] = p
	}
	for _, gone := range []string{"plain", "gitproto", "badindex"} {
		if _, ok := byDir[gone]; ok {
			t.Errorf("%s's source travelled: %+v", gone, byDir[gone])
		}
	}
	for dir, want := range map[string]string{
		"starter":  "mirrin protocols install starter",
		"pinned":   "mirrin protocols add https://github.com/x/pinned.git#v2.0.0",
		"atcommit": "mirrin protocols add git@github.com:x/atcommit.git#" + commit,
	} {
		if got := byDir[dir].Reconnect(); got != want {
			t.Errorf("%s: %q, want %q", dir, got, want)
		}
	}

	dst := twinHome(t, nil)
	if _, err := Import(dst, out); err != nil {
		t.Fatal(err)
	}
	var pv provenance
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dst, "protocols", "packs", "starter", provenanceFile))), &pv); err != nil {
		t.Fatal(err)
	}
	if want := (provenance{Source: "https://github.com/x/starter.git", Registry: "starter", Index: "https://example.com/index.yaml", Commit: commit}); !reflect.DeepEqual(pv, want) {
		t.Fatalf("provenance %+v, want %+v", pv, want)
	}
}

// A pack whose .git is damaged doesn't pass off the repository around the
// twin's home (a home kept in git) as its own source.
func TestPackSourceNeverComesFromAnOuterRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := twinHome(t, nil)
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitRun(src, "init", "-q")
	gitRun(src, "remote", "add", "origin", "https://github.com/me/dotfiles.git")
	pack := filepath.Join(src, "protocols", "packs", "broken")
	writeFile(t, filepath.Join(pack, "a.yaml"), "name: a\nprompt: y\n")
	writeFile(t, filepath.Join(pack, ".git", "placeholder"), "") // a .git that isn't a repository
	gitRun(src, "add", ".")
	gitRun(src, "commit", "-q", "-m", "x")
	m, err := Export(src, filepath.Join(t.TempDir(), "twin.tar.gz"), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Packs {
		if strings.Contains(p.Repo, "dotfiles") {
			t.Fatalf("the outer repository became the pack's source: %+v", p)
		}
	}
}
