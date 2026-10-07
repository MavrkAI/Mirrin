package identity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// Imports clone packs that arrived as copies from their sources. Tests use
// addresses like https://example.com/..., which must never be contacted, so
// by default the clone fails and the copy stays; a test that wants a clone
// points it at a local repository.
func TestMain(m *testing.M) {
	reconnectPack = func(context.Context, string, string, protocols.Provenance, string) error {
		return errors.New("offline in tests")
	}
	fetchIndex = func(context.Context, string) (*protocols.Registry, error) {
		return nil, errors.New("offline in tests")
	}
	os.Exit(m.Run())
}

// Import used to leave a pack that came as a copy, and print a remove-and-add
// command for the user to run. It now clones it again from its recorded
// source, at the recorded commit, so it updates here straight away.
func TestImportReconnectsCopiedPacks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	gitRun := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitRun("init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "protocols", "news.yaml"), "name: news\nprompt: ONE\n")
	gitRun("add", "-A")
	gitRun("commit", "-q", "-m", "one")
	first := gitRun("rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "protocols", "news.yaml"), "name: news\nprompt: TWO\n")
	gitRun("commit", "-q", "-am", "two")

	const source = "https://example.com/packs/news.git"
	var asked []protocols.Provenance
	old := reconnectPack
	reconnectPack = func(ctx context.Context, dir, name string, pv protocols.Provenance, commit string) error {
		asked = append(asked, pv)
		if pv.Source != source {
			return errors.New("unexpected source " + pv.Source)
		}
		pv.Source = fileURL(repo) // the same repository, reachable offline
		return protocols.ReconnectPack(ctx, dir, name, pv, commit)
	}
	oldIndex := fetchIndex
	fetchIndex = func(_ context.Context, u string) (*protocols.Registry, error) {
		if u != "" {
			return nil, errors.New("unexpected index " + u)
		}
		return &protocols.Registry{Packs: []protocols.Pack{{Name: "news", Repo: source}}}, nil
	}
	t.Cleanup(func() { reconnectPack, fetchIndex = old, oldIndex })

	src := twinHome(t, nil)
	pack := filepath.Join(src, "protocols", "packs", "news")
	writeFile(t, filepath.Join(pack, "protocols", "news.yaml"), "name: news\nprompt: ONE\n")
	writeFile(t, filepath.Join(pack, provenanceFile), `{"source": "`+source+`", "registry": "news", "commit": "`+first+`"}`)
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if _, err := Export(src, out, false); err != nil {
		t.Fatal(err)
	}

	dst := twinHome(t, nil)
	r, err := Import(dst, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Packs) != 0 || len(r.Reconnected) != 1 || r.Reconnected[0] != "news" {
		t.Fatalf("packs %+v, reconnected %v", r.Packs, r.Reconnected)
	}
	if len(asked) != 1 || asked[0].Registry != "news" || asked[0].Commit != first {
		t.Fatalf("reconnected from %+v", asked)
	}
	root := filepath.Join(dst, "protocols", "packs", "news")
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != first {
		t.Fatalf("the pack isn't a checkout at the recorded commit: %s %v", head, err)
	}
	pv, ok := protocols.ReadProvenance(root)
	if !ok || pv.Registry != "news" || pv.Commit != first {
		t.Fatalf("provenance %+v", pv)
	}

	// Offline, the copy stays and the CLI is told how to reconnect it.
	reconnectPack = func(context.Context, string, string, protocols.Provenance, string) error {
		return errors.New("offline")
	}
	dst2 := twinHome(t, nil)
	r, err = Import(dst2, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Packs) != 1 || len(r.Reconnected) != 0 {
		t.Fatalf("offline: packs %+v, reconnected %v", r.Packs, r.Reconnected)
	}
	if !strings.Contains(readFile(t, filepath.Join(dst2, "protocols", "packs", "news", "protocols", "news.yaml")), "ONE") {
		t.Fatal("the copy was lost")
	}
}

// A registry pack was cloned again from the index the archive named, so an
// archive could tie it to an index this machine never chose, whose later
// updates nothing checks. It is looked up in this machine's index now, and
// stays a copy when the archive names another index or the entry differs.
func TestImportReconnectsRegistryPacksOnlyThroughThisIndex(t *testing.T) {
	const source = "https://example.com/packs/news.git"
	var asked []protocols.Provenance
	oldPack, oldIndex := reconnectPack, fetchIndex
	t.Cleanup(func() { reconnectPack, fetchIndex = oldPack, oldIndex })
	reconnectPack = func(_ context.Context, _, _ string, pv protocols.Provenance, _ string) error {
		asked = append(asked, pv)
		return nil
	}
	var indexes []string
	listed := source
	fetchIndex = func(_ context.Context, u string) (*protocols.Registry, error) {
		indexes = append(indexes, u)
		return &protocols.Registry{Packs: []protocols.Pack{{Name: "news", Repo: listed}}}, nil
	}
	commit := strings.Repeat("a", 40)
	archive := func(index string) string {
		src := twinHome(t, nil)
		pack := filepath.Join(src, "protocols", "packs", "news")
		writeFile(t, filepath.Join(pack, "protocols", "news.yaml"), "name: news\nprompt: ONE\n")
		writeFile(t, filepath.Join(pack, provenanceFile), `{"source": "`+source+`", "registry": "news", "index": "`+index+`", "commit": "`+commit+`"}`)
		out := filepath.Join(t.TempDir(), "twin.tar.gz")
		if _, err := Export(src, out, false); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// The archive names its own index: the pack stays a copy, and that
	// index is never read.
	r, err := Import(twinHome(t, nil), archive("https://evil.example/index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Packs) != 1 || len(r.Reconnected) != 0 || len(asked) != 0 {
		t.Fatalf("foreign index: packs %+v, reconnected %v, asked %+v", r.Packs, r.Reconnected, asked)
	}
	for _, u := range indexes {
		if u != "" {
			t.Fatalf("read the archive's index %s", u)
		}
	}

	// This machine's index lists it elsewhere: it stays a copy.
	listed = "https://example.com/other/news.git"
	r, err = Import(twinHome(t, nil), archive(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Packs) != 1 || len(asked) != 0 {
		t.Fatalf("moved entry: packs %+v, asked %+v", r.Packs, asked)
	}

	// Listed here at the same repository: reconnected, following this index.
	listed = source + "/"
	r, err = Import(twinHome(t, nil), archive(protocols.DefaultRegistryURL))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Reconnected) != 1 || len(asked) != 1 || asked[0].Index != "" || asked[0].Registry != "news" {
		t.Fatalf("listed: reconnected %v, asked %+v", r.Reconnected, asked)
	}

	// The default index's address from before the rename is the default
	// too, in the archive and in this machine's config: the signed index is
	// read, never that address.
	const oldDefault = "https://raw.githubusercontent.com/MavrkAI/AntBot/main/registry/index.json"
	asked, indexes = nil, nil
	here := twinHome(t, func(c *config.Config) { c.ProtocolRegistry = oldDefault })
	r, err = Import(here, archive(oldDefault))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Reconnected) != 1 || len(asked) != 1 || asked[0].Index != "" || len(indexes) != 1 || indexes[0] != "" {
		t.Fatalf("old default: reconnected %v, asked %+v, indexes read %q", r.Reconnected, asked, indexes)
	}
}

// Every import saved what it replaced in a new backups/<time> folder, and
// none was ever removed. Only the newest few are kept now.
func TestImportKeepsTheNewestBackups(t *testing.T) {
	src := twinHome(t, nil)
	writeFile(t, filepath.Join(src, "personas", "me.yaml"), "name: Me\ncharacter: calm\n")
	out := filepath.Join(t.TempDir(), "twin.tar.gz")
	if _, err := Export(src, out, false); err != nil {
		t.Fatal(err)
	}
	dst := twinHome(t, nil)
	writeFile(t, filepath.Join(dst, "backups", "not-an-import", "keep"), "x")
	var made []string
	for range 7 {
		r, err := Import(dst, out)
		if err != nil {
			t.Fatal(err)
		}
		if r.Backup == "" {
			t.Fatal("the import replaced nothing, so the test proves nothing")
		}
		made = append(made, filepath.Base(r.Backup))
	}
	entries, err := os.ReadDir(filepath.Join(dst, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, e := range entries {
		if e.Name() != "not-an-import" {
			kept = append(kept, e.Name())
		}
	}
	if len(kept) != keepBackups {
		t.Fatalf("%d backups kept, want %d: %v", len(kept), keepBackups, kept)
	}
	for _, name := range made[len(made)-keepBackups:] {
		if _, err := os.Stat(filepath.Join(dst, "backups", name)); err != nil {
			t.Errorf("newest backup %s was removed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "backups", "not-an-import", "keep")); err != nil {
		t.Fatal("a folder the import didn't make was removed")
	}
}
