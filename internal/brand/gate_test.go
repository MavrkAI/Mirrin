package brand

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot is the module root, two folders up, or the test is skipped (a
// copy of the package outside the repository, say).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("module root not found from %s", root)
	}
	return root
}

// gitGrep runs git grep over the tracked files in root and returns its
// matches as file, line and text. The test is skipped where there is no git
// or no checkout.
func gitGrep(t *testing.T, root string, args ...string) []match {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git isn't installed")
	}
	if err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		t.Skipf("%s isn't a git checkout", root)
	}
	cmd := exec.Command("git", append([]string{"-C", root, "grep", "-n", "-I", "-z"}, args...)...)
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
		return nil // no matches
	}
	if err != nil {
		t.Fatalf("git grep: %v", err)
	}
	var ms []match
	for _, l := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		parts := strings.SplitN(l, "\x00", 3)
		if len(parts) != 3 {
			t.Fatalf("git grep printed %q", l)
		}
		ms = append(ms, match{file: parts[0], line: parts[1], text: parts[2]})
	}
	return ms
}

type match struct{ file, line, text string }

// keptWhole are the tracked files that may name the old products anywhere:
// private history that is never published, this package, and the backup
// test vectors, which were derived under the old backup salt. A folder ends
// in /; the rest are path.Match patterns.
var keptWhole = []string{
	"docs/dev/",    // history, never published
	"docs/launch/", // the search behind the name: history, never published
	"internal/brand/",
	"internal/backup/testdata/vectors.json",
}

func isKeptWhole(file string) bool {
	for _, p := range keptWhole {
		if strings.HasSuffix(p, "/") && strings.HasPrefix(file, p) {
			return true
		}
		if ok, _ := path.Match(p, file); ok {
			return true
		}
	}
	return false
}

// keptSpellings are the backup values that still carry the old name, because
// existing backups and their recovery depend on them (frozen_test.go pins
// each one where it is defined).
var keptSpellings = regexp.MustCompile(`antbot-(backup|handover|backup-namespace|recover)-v1|AntBot Backups`)

var reOldName = regexp.MustCompile(`(?i)antbot|openhuman`)

// namesOldProduct reports whether a line of a tracked file names AntBot or
// openHuman other than in a file kept whole or a kept backup value.
func namesOldProduct(file, text string) bool {
	if isKeptWhole(file) {
		return false
	}
	return reOldName.MatchString(keptSpellings.ReplaceAllString(text, ""))
}

// oldNameLines is how many other lines of a file may name the old products:
// tests of the kept backup values, and the test that checks the site never
// mentions them. A file not listed may name them nowhere.
var oldNameLines = map[string]int{
	"internal/backup/phrase_test.go": 1, // the vectors' "antbot" key
	"site/site_test.go":              1,
}

// TestOldNameStaysOut keeps the old products' names out of the tracked
// files, except for the backup values kept on purpose. A branch started
// before the rename and merged after it would otherwise bring back old
// settings, homes and names in text people read, which compile and pass
// their own tests while being quietly wrong.
func TestOldNameStaysOut(t *testing.T) {
	root := repoRoot(t)
	lines := map[string][]string{}
	for _, m := range gitGrep(t, root, "-i", "-E", "-e", "antbot|openhuman") {
		if !namesOldProduct(m.file, m.text) {
			continue
		}
		lines[m.file] = append(lines[m.file], m.file+":"+m.line+": "+strings.TrimSpace(m.text))
	}
	files := make([]string, 0, len(lines))
	for f := range lines {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		if n, allowed := len(lines[f]), oldNameLines[f]; n > allowed {
			t.Errorf("%s names an old product on %d lines, %d allowed:\n\t%s\nName Mirrin instead.",
				f, n, allowed, strings.Join(lines[f], "\n\t"))
		}
	}

	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, f := range bytes.Split(out, []byte{0}) {
		if name := string(f); reOldName.MatchString(name) && !isKeptWhole(name) {
			t.Errorf("%s is named after an old product; name it after Mirrin", name)
		}
	}
}

// TestNoHalfRenamedSpellings catches what a blind replacement makes of the
// kept backup values, the branch name and the domains Mirrin doesn't use.
func TestNoHalfRenamedSpellings(t *testing.T) {
	root := repoRoot(t)
	pattern := `mirrin-next|mirrin\.dev([^a-z0-9]|$)|mirrin-(backup|handover|backup-namespace|recover)-v1|mirrin\.ai([^a-z0-9]|$)`
	for _, m := range gitGrep(t, root, "-i", "-E", "-e", pattern, "--", ":!internal/brand/gate_test.go") {
		t.Errorf("%s:%s: %s\nThis value keeps the old spelling on purpose (see frozen_test.go), or isn't Mirrin's (the domain is mirrin.app).", m.file, m.line, strings.TrimSpace(m.text))
	}
}

// The gate isn't blind: an old name coming back is caught, and only the
// backup values kept on purpose pass.
func TestGateCatchesTheOldName(t *testing.T) {
	for _, c := range []struct {
		file, text string
		want       bool
	}{
		{"internal/daemon/x.go", `if os.Getenv("ANTBOT_DEBUG") != "" {`, true},
		{"internal/daemon/x.go", `home := filepath.Join(user, ".antbot")`, true},
		{"internal/daemon/x.go", `home := filepath.Join(user, ".openhuman")`, true},
		{"internal/api/x.html", `<h1>Welcome to AntBot</h1>`, true},
		{"internal/daemon/x.go", `legacy := ".antbot" // rename:keep`, true},
		{"internal/relay/wire/wire.go", `Subprotocol = "antbot.tunnel.v1"`, true},
		{"internal/api/ui.html", `localStorage.getItem('antbot.theme')`, true},
		{"CHANGELOG.md", "AntBot can now ...", true},
		{"README.md", "built on antbot-next, ships in the next release", true},
		{"internal/backup/keys.go", `const kdfSalt = "antbot-backup-v1"`, false},
		{"internal/backup/target_folder.go", `legacyICloudFolder = "AntBot Backups"`, false},
		{"docs/dev/backlog.md", "AntBot's old notes", false},
		{"internal/brand/brand.go", `// Mirrin was called AntBot`, false},
	} {
		if got := namesOldProduct(c.file, c.text); got != c.want {
			t.Errorf("namesOldProduct(%s, %s) = %v, want %v", c.file, c.text, got, c.want)
		}
	}
}
