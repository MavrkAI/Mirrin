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

// keptWhole are the tracked files that may name AntBot anywhere: history,
// this package (the old names' one home), data that is signed or pinned,
// and the tests that prove a Mirrin and an AntBot-era install both work. A
// folder ends in /; the rest are path.Match patterns.
var keptWhole = []string{
	"CHANGELOG.md",          // released entries are history
	"docs/launch/name-*.md", // the search behind the name: history, never published
	"docs/dev/",             // history, never published
	"internal/brand/",
	"registry/index.json",                   // minisign-signed: the maintainer re-signs it
	"internal/backup/testdata/vectors.json", // golden vectors, derived under antbot-backup-v1
	"internal/cloud/testdata/mentions/",     // proves the Cloud-pitch guard still catches AntBot Cloud

	// Tests with AntBot-era fixtures beside the current ones.
	"internal/service/*_test.go",
	"cmd/mirrin/update_test.go",
	"cmd/mirrin/uninstall_test.go",
	"cmd/mirrin-relay/main_test.go",
	"internal/api/auth_test.go",
	"internal/api/pwa/sw_test.js",
	"internal/api/sharedkey_test.go",
	"internal/identity/*_test.go",
	"internal/backup/*_test.go",
	"internal/entitle/*_test.go",
	"internal/config/home_test.go",
	"internal/config/home_isolation_test.go",
	"internal/homelock/homelock_test.go",
	"internal/daemon/servicename_test.go",
	"internal/protocols/packs_test.go",
	"internal/skills/system/own_files_test.go",
	"packaging/docker/entrypoint_test.go",
	"scripts/scripts_test.go",
	"scripts/releasecheck/main_test.go",
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

// keptSpellings are the old name's spellings that never change, because
// they are bytes on the wire, in signatures and encrypted backups, or names
// of files and browser storage on users' machines (frozen_test.go pins
// each one where it is defined). The branch the work happens on isn't one:
// what is built but not released is "in the next release".
var keptSpellings = regexp.MustCompile(`antbot\.lock|\.antbot-pack\.json|antbot\.tunnel\.v\d+|antbot-relay-tunnel-v1|EXPORTER-antbot-tunnel|` +
	`antbot-(backup|handover|backup-namespace|recover|cloud|approval|enrol)-v1|antbot dev key|"aud":"antbot"|aud is not antbot|` +
	`(?i:antbot-stepup|antbot-notice)|ANTBOT01|antbot\.(name|theme|noted\.seen|left\.seen|character)\b|antbot-(install-ticket|last-seen)\b`)

var reOldName = regexp.MustCompile(`(?i)antbot`)

// namesOldProduct reports whether a line of a tracked file names AntBot
// other than where that is kept on purpose: a file kept whole, a rename:keep
// line or a kept spelling.
func namesOldProduct(file, text string) bool {
	if isKeptWhole(file) || strings.Contains(text, "rename:keep") {
		return false
	}
	return reOldName.MatchString(keptSpellings.ReplaceAllString(text, ""))
}

// oldNameLines is how many lines of each other tracked file may name AntBot,
// not counting rename:keep lines and kept spellings: code and docs that
// explain what happens to an AntBot-era install. A file not listed may name
// it nowhere.
var oldNameLines = map[string]int{
	"AGENTS.md":                            1,
	"ARCHITECTURE.md":                      2,
	"Makefile":                             1,
	"README.md":                            1,
	"cmd/mirrin-relay/main.go":             1,
	"cmd/mirrin/uninstall.go":              9,
	"cmd/mirrin/update.go":                 2,
	"docs/backup-format.md":                1,
	"docs/cloud-design.md":                 4,
	"docs/maintainers-release.md":          4, // the private MavrkAI/AntBot repository stays
	"docs/reach.md":                        1,
	"docs/relay-selfhost.md":               8,
	"docs/threat-model.md":                 1,
	"install.ps1":                          3,
	"install.sh":                           3,
	"internal/api/auth.go":                 2,
	"internal/backup/restore.go":           4,
	"internal/backup/target_folder.go":     1,
	"internal/config/home.go":              7,
	"internal/config/home_rebase.go":       2,
	"internal/config/secrets.go":           1,
	"internal/config/service.go":           3,
	"internal/daemon/daemon.go":            2,
	"internal/homelock/homelock.go":        1,
	"internal/identity/settings.go":        3,
	"internal/reach/byod.go":               1,
	"internal/service/desktop.go":          1,
	"internal/service/foreign.go":          1,
	"internal/service/legacy.go":           3,
	"internal/service/logs.go":             1,
	"internal/service/service.go":          6,
	"internal/service/rehome.go":           4,
	"internal/service/startup.go":          7,
	"internal/service/startup_windows.go":  2,
	"internal/skills/system/sensitive.go":  2,
	"packaging/docker/Dockerfile":          2,
	"packaging/docker/entrypoint.sh":       2,
	"packaging/relay/mirrin-relay.service": 6,
	"packaging/windows/mirrin.iss":         11,
	"scripts/build-app.sh":                 1,
	"site/README.md":                       3,
	"site/site_test.go":                    3, // checks the old name is gone from the pages
}

// TestOldNameStaysOut keeps AntBot's name out of the tracked files, except
// where it is kept on purpose. A branch started before the rename and merged
// after it would otherwise bring back os.Getenv("ANTBOT_…") reads, ~/.antbot
// paths and AntBot in text people read, which compile and pass their own
// tests while being quietly wrong.
func TestOldNameStaysOut(t *testing.T) {
	root := repoRoot(t)
	lines := map[string][]string{}
	for _, m := range gitGrep(t, root, "-i", "-e", "antbot") {
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
			t.Errorf("%s names AntBot on %d lines, %d allowed:\n\t%s\n"+
				"Read an old environment variable, home or service name through package brand. "+
				"A line that must say AntBot (a migration, a message about the old install) gets a rename:keep comment, "+
				"or raise the file's count in internal/brand/gate_test.go.",
				f, n, allowed, strings.Join(lines[f], "\n\t"))
		}
	}

	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, f := range bytes.Split(out, []byte{0}) {
		if name := string(f); reOldName.MatchString(name) && !isKeptWhole(name) {
			t.Errorf("%s is named after AntBot; name it after Mirrin", name)
		}
	}
}

// TestNoHalfRenamedSpellings catches what a blind AntBot → Mirrin
// replacement makes of the kept spellings (mirrin.lock, mirrin-backup-v1),
// the branch name, the Homebrew class and the domain Mirrin doesn't use.
func TestNoHalfRenamedSpellings(t *testing.T) {
	root := repoRoot(t)
	pattern := `mirrin-next|mirrin\.lock|\.mirrin-pack\.json|mirrin\.dev([^a-z0-9]|$)|` +
		`mirrin-(backup|handover|backup-namespace|recover|approval|enrol|cloud)-v1|mirrin\.tunnel\.v1|mirrin-relay-tunnel-v1|` +
		`EXPORTER-mirrin|mirrin dev key|class Antbot|aud is not mirrin|mirrin\.ai([^a-z0-9]|$)`
	for _, m := range gitGrep(t, root, "-i", "-E", "-e", pattern, "--", ":!internal/brand/gate_test.go") {
		t.Errorf("%s:%s: %s\nThis spelling is the old one's on purpose (see frozen_test.go), or not Mirrin's (the domain is mirrin.app).", m.file, m.line, strings.TrimSpace(m.text))
	}
}

// The gate isn't blind: what a pre-rename branch would bring back is caught,
// and only the spellings kept on purpose pass.
func TestGateCatchesTheOldName(t *testing.T) {
	for _, c := range []struct {
		file, text string
		want       bool
	}{
		{"internal/daemon/x.go", `if os.Getenv("ANTBOT_DEBUG") != "" {`, true},
		{"internal/daemon/x.go", `home := filepath.Join(user, ".antbot")`, true},
		{"internal/api/x.html", `<h1>Welcome to AntBot</h1>`, true},
		{"internal/tlsmgr/x.go", `os.CreateTemp(dir, ".antbot-next-key-*")`, true},
		{"internal/daemon/x.go", `lock := filepath.Join(data, "antbot.lock") // ok, and ANTBOT_HOME too`, true},
		{"internal/daemon/x.go", `lock := filepath.Join(data, "antbot.lock")`, false},
		{"internal/daemon/x.go", `legacy := ".antbot" // rename:keep`, false},
		{"internal/relay/wire/wire.go", `Subprotocol = "antbot.tunnel.v1"`, false},
		{"internal/api/pwa/client_test.js", `assert.equal(again.headers['antbot-stepup'],'su_abc')`, false},
		{"internal/api/ui.html", `localStorage.getItem('antbot.theme')`, false},
		{"README.md", "built on antbot-next, ships in the next release", true},
		{"CHANGELOG.md", "AntBot can now ...", false},
		{"internal/service/service_test.go", `writeFile(t, p, "Label antbot")`, false},
		{"internal/brand/brand.go", `LegacyNames = []string{"antbot", "openhuman"}`, false},
	} {
		if got := namesOldProduct(c.file, c.text); got != c.want {
			t.Errorf("namesOldProduct(%s, %s) = %v, want %v", c.file, c.text, got, c.want)
		}
	}
}
