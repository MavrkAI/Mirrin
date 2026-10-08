package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestTestsIsolateHome keeps `go test ./...` away from the contributor's own
// twin. Save() writes ~/.mirrin/config.yaml, so a test that reaches package
// config must point MIRRIN_HOME at a temporary directory first: in the same
// file (t.Setenv), or in the package's TestMain. Another file of the
// package setting it for its own tests leaves the rest unguarded. Only
// setting it counts: a file that merely mentions the home variable (a unit
// fixture, say) isolates nothing.
func TestTestsIsolateHome(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("module root not found from %s", root)
	}
	self, _ := filepath.Abs("home_isolation_test.go")
	here, _ := filepath.Abs(".")
	// The secrets file lives in the home too (secrets.env), and every key
	// accessor falls back to it.
	qualified := regexp.MustCompile(`\bconfig\.(Default|Home|Path|Load|LoadRemote|RemotePath|SaveRemote|Secret|ReadSecrets|SaveSecrets|SecretsPath|LoadSecrets)\(`)
	local := regexp.MustCompile(`(?:^|[^.\w])(Default|Home|Path|Load|LoadRemote|RemotePath|SaveRemote|Secret|ReadSecrets|SaveSecrets|SecretsPath|LoadSecrets)\(`)

	byDir := map[string][]string{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, "_test.go") && p != self {
			byDir[filepath.Dir(p)] = append(byDir[filepath.Dir(p)], p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var bad []string
	for dir, files := range byDir {
		isolated := false // a TestMain points MIRRIN_HOME somewhere
		var reaches []string
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			src := string(b)
			if mainSetsHome(src) {
				isolated = true
			}
			if (qualified.MatchString(src) || (dir == here && local.MatchString(src))) && !setsHome(src) {
				rel, _ := filepath.Rel(root, f)
				reaches = append(reaches, filepath.ToSlash(rel))
			}
		}
		if len(reaches) > 0 && !isolated {
			bad = append(bad, reaches...)
		}
	}
	sort.Strings(bad)
	for _, f := range bad {
		t.Errorf("%s reaches the real ~/.mirrin through package config; point MIRRIN_HOME at a temporary directory first (t.Setenv, or a TestMain for the whole package)", f)
	}
}

// reSetsHome is a test pointing MIRRIN_HOME somewhere: t.Setenv or
// os.Setenv, or a child process's environment.
var reSetsHome = regexp.MustCompile(`Setenv\(\s*"MIRRIN_HOME"|"MIRRIN_HOME="`)

// setsHome reports whether a test file points MIRRIN_HOME somewhere.
func setsHome(src string) bool { return reSetsHome.MatchString(src) }

// reTestMain is the start of a TestMain.
var reTestMain = regexp.MustCompile(`(?m)^func TestMain\(`)

// mainSetsHome reports whether a test file's TestMain points MIRRIN_HOME
// somewhere, for every test of the package.
func mainSetsHome(src string) bool {
	loc := reTestMain.FindStringIndex(src)
	if loc == nil {
		return false
	}
	body := src[loc[0]:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	return setsHome(body)
}

// A unit fixture that names a home variable isolates nothing.
func TestOnlySettingTheHomeIsolates(t *testing.T) {
	for src, want := range map[string]bool{
		`t.Setenv("MIRRIN_HOME", t.TempDir())`:                         true,
		`os.Setenv("MIRRIN_HOME", dir)`:                                true,
		`cmd.Env = append(os.Environ(), "MIRRIN_HOME="+dir)`:           true,
		`env := map[string]string{"MIRRIN_HOME": "/Users/me/.mirrin"}`: false,
		`// set MIRRIN_HOME first`:                                     false,
		`if env["MIRRIN_HOME"] != home {`:                              false,
	} {
		if got := setsHome(src); got != want {
			t.Errorf("setsHome(%s) = %v", src, got)
		}
	}
}

// Only a TestMain guards the whole package: one test setting MIRRIN_HOME in
// another function doesn't.
func TestOnlyTestMainGuardsThePackage(t *testing.T) {
	for src, want := range map[string]bool{
		"func TestMain(m *testing.M) {\n\tos.Setenv(\"MIRRIN_HOME\", dir)\n\tos.Exit(m.Run())\n}\n":                                         true,
		"func TestMain(m *testing.M) {\n\tos.Exit(m.Run())\n}\n\nfunc TestX(t *testing.T) {\n\tt.Setenv(\"MIRRIN_HOME\", t.TempDir())\n}\n": false,
		"func TestX(t *testing.T) {\n\tt.Setenv(\"MIRRIN_HOME\", t.TempDir())\n}\n":                                                         false,
	} {
		if got := mainSetsHome(src); got != want {
			t.Errorf("mainSetsHome(%q) = %v", src, got)
		}
	}
}
