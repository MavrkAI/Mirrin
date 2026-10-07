package protocols

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A pack that came with an identity import is a copy with no .git, and
// `protocols update` said it was "copied from a folder" and never updated
// it, though its provenance names the repository. It is now cloned again
// from there and swapped in, at the remote commit.
func TestCopiedPackWithProvenanceReconnects(t *testing.T) {
	src, git := gitRepo(t)
	write(t, filepath.Join(src, "protocols", "news.yaml"), "name: news\nprompt: ONE\n")
	git("add", "-A")
	git("commit", "-q", "-m", "one")
	first := git("rev-parse", "HEAD")

	dir := t.TempDir()
	ctx := context.Background()
	name, err := AddPack(ctx, dir, fileURL(src))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(PacksDir(dir), name)
	// what an identity import leaves: the files and the provenance, no .git
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "protocols", "news.yaml"), "name: news\nprompt: TWO\n")
	git("commit", "-q", "-am", "two")
	second := git("rev-parse", "HEAD")

	ups, err := CheckPackUpdates(ctx, dir)
	if err != nil || len(ups) != 1 {
		t.Fatalf("check: %v %+v", err, ups)
	}
	u := ups[0]
	if !u.Pending() || !u.Reconnect || u.From != first || u.To != second || u.Source != fileURL(src) {
		t.Fatalf("a copy with a source should offer to reconnect: %+v", u)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		t.Fatal("checking changed the pack")
	}
	if err := ApplyPackUpdate(ctx, dir, u); err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != second {
		t.Fatalf("not a checkout at the remote commit: %s %v", head, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "protocols", "news.yaml")); !strings.Contains(string(b), "TWO") {
		t.Fatalf("files not updated:\n%s", b)
	}
	if pv, _ := ReadProvenance(root); pv.Commit != second || pv.Source != fileURL(src) || pv.Updated.IsZero() {
		t.Fatalf("provenance %+v", pv)
	}
	if entries, _ := os.ReadDir(PacksDir(dir)); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	// now it updates in place, like any installed pack
	if ups, _ := CheckPackUpdates(ctx, dir); ups[0].Pending() || ups[0].Reconnect || ups[0].Err != nil {
		t.Fatalf("should be up to date: %+v", ups[0])
	}
}

// A clone that fails leaves the copy as it was.
func TestReconnectPackKeepsTheCopyOnFailure(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(PacksDir(dir), "news", "protocols", "a.yaml"), "name: a\nprompt: x\n")
	missing := fileURL(filepath.Join(t.TempDir(), "gone"))
	if err := ReconnectPack(context.Background(), dir, "news", Provenance{Source: missing}, ""); err == nil {
		t.Fatal("a missing repository should fail")
	}
	if _, err := os.Stat(filepath.Join(PacksDir(dir), "news", "protocols", "a.yaml")); err != nil {
		t.Fatal("the copy was lost")
	}
	for _, bad := range []string{"..", ".hidden", "a/b", ""} {
		if err := ReconnectPack(context.Background(), dir, bad, Provenance{Source: missing}, ""); err == nil {
			t.Errorf("ReconnectPack(%q) should refuse", bad)
		}
	}
	if err := ReconnectPack(context.Background(), dir, "news", Provenance{Source: "ext::sh -c x"}, ""); err == nil {
		t.Fatal("an unsafe source should be refused")
	}
}
