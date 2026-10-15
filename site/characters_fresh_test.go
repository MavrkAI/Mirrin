package site

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The site's portraits are drawn from the screen's own code by draw.js, and
// a change to a drawing that wasn't followed by a run of it left the site
// showing the old Pickoo. With node here, this draws them again into a copy
// of the tree and insists the committed files are what it draws.
func TestSiteCharactersAreFreshlyDrawn(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable; run node site/assets/characters/draw.js by hand after a drawing changes")
	}
	tmp := t.TempDir()
	files := []string{"site/assets/characters/draw.js", "internal/api/characters.js", "site/index.html"}
	for _, rel := range files {
		if err := os.MkdirAll(filepath.Join(tmp, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, rel), []byte(repoFile(t, rel)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), node, "site/assets/characters/draw.js")
	cmd.Dir = tmp
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("draw.js: %v\n%s", err, out)
	}
	for _, rel := range []string{"site/assets/characters/mirrin.svg", "site/assets/characters/nyra.svg", "site/assets/characters/penguin.svg", "site/index.html"} {
		drawn, err := os.ReadFile(filepath.Join(tmp, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(drawn) != repoFile(t, rel) {
			t.Errorf("%s is not what draw.js draws from internal/api/characters.js now; run node site/assets/characters/draw.js", rel)
		}
	}
}
