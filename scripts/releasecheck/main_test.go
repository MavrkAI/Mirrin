package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hostBinary is a real executable for this machine: the test binary itself.
func hostBinary(t *testing.T) []byte {
	t.Helper()
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64":
	default:
		t.Skipf("no release build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckMatchesContentsToName(t *testing.T) {
	bin := hostBinary(t)
	otherOS := map[string]string{"linux": "darwin", "darwin": "linux", "windows": "linux"}[runtime.GOOS]
	otherArch := map[string]string{"amd64": "arm64", "arm64": "amd64"}[runtime.GOARCH]
	ext := map[bool]string{true: ".exe"}[runtime.GOOS == "windows"]
	cases := []struct {
		name  string
		asset string
		data  []byte
		want  string // substring of the problem; empty means none
	}{
		{"named for what it is", "mirrin-" + runtime.GOOS + "-" + runtime.GOARCH + ext, bin, ""},
		{"MIT build named for what it is", "mirrin-" + runtime.GOOS + "-" + runtime.GOARCH + "-nowhatsapp" + ext, bin, ""},
		{"MIT wrong architecture", "mirrin-" + runtime.GOOS + "-" + otherArch + "-nowhatsapp" + ext, bin, "binary, not " + runtime.GOOS + "/" + otherArch},
		{"MIT Windows without extension", "mirrin-windows-amd64-nowhatsapp", bin, "only Windows builds end in .exe"},
		{"MIT Windows wrong suffix order", "mirrin-windows-amd64.exe-nowhatsapp", bin, "not a release name"},
		// The v0.2.0 bug: a Linux ELF uploaded under a darwin name.
		{"another OS's name", "mirrin-" + otherOS + "-" + runtime.GOARCH, bin, "is a " + runtime.GOOS + "/" + runtime.GOARCH + " binary"},
		{"another architecture's name", "mirrin-" + runtime.GOOS + "-" + otherArch + ext, bin, "binary, not " + runtime.GOOS + "/" + otherArch},
		{"uname -m instead of GOARCH", "mirrin-darwin-x86_64", bin, "not a release name"},
		{"Windows without .exe", "mirrin-windows-amd64", bin, "only Windows builds end in .exe"},
		{"not an executable", "mirrin-linux-amd64", []byte("<html>404</html>"), "not an executable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.asset), c.data, 0o755); err != nil {
				t.Fatal(err)
			}
			// Files that aren't CLI binaries are left alone.
			_ = os.WriteFile(filepath.Join(dir, "Mirrin-v1.0.0-macos.dmg"), []byte("dmg"), 0o644)
			problems, err := checkFixtures(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(problems, "\n")
			if c.want == "" && got != "" {
				t.Fatalf("unexpected problems:\n%s", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("want a problem containing %q, got:\n%s", c.want, got)
			}
		})
	}
}

func TestCheckFlagsFilesThatArentPartOfARelease(t *testing.T) {
	cases := []struct {
		name string
		bad  bool
	}{
		{"Mirrin-v1.2.0-macos.dmg", false},
		{"Mirrin-v1.2.0-rc.1-windows-setup.exe", false},
		{"Mirrin-v1.2.0-source.tar.gz", false},
		{"Mirrin-v1.2.0-sbom.spdx.json", false},
		{NoticesFile, false},
		{"THIRD_PARTY_NOTICES", true}, // the repository's copy, without versions
		{SumsFile, false},
		{SumsFile + ".sigstore.json", false},
		{".DS_Store", false},
		// Leftovers in a local dist/ would otherwise be summed and uploaded.
		{"other-linux-amd64", true},
		{"other-relay-linux-arm64", true},
		{"Other-v0.2.1-macos.dmg", true},
		{"Other-v0.2.1-windows-setup.exe", true},
		{"notes.md", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			problems, err := checkFixtures(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(problems) > 0; got != c.bad {
				t.Fatalf("flagged = %v, want %v: %v", got, c.bad, problems)
			}
		})
	}
	// The app bundle is a folder next to the DMG; it isn't published.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Mirrin.app", "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if problems, _ := checkFixtures(dir, nil); len(problems) > 0 {
		t.Fatalf("Mirrin.app flagged: %v", problems)
	}
}

func TestCheckReportsMissingPlatforms(t *testing.T) {
	problems, err := checkFixtures(t.TempDir(), []string{"darwin/amd64", "windows/arm64", "windows/amd64/nowhatsapp", "relay/linux/arm64"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(problems, "\n")
	for _, want := range []string{"missing mirrin-darwin-amd64", "missing mirrin-windows-arm64.exe", "missing mirrin-windows-amd64-nowhatsapp.exe", "missing mirrin-relay-linux-arm64"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}

func TestWriteSums(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"mirrin-linux-amd64":              "linux",
		"Mirrin-v1.0.0-macos.dmg":         "dmg",
		SumsFile:                          "stale",
		SumsFile + ".sigstore.json":       "{}",
		".DS_Store":                       "junk",
		"Mirrin-v1.0.0-windows-setup.exe": "setup",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	n, err := writeSums(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, SumsFile))
	if err != nil {
		t.Fatal(err)
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	want := sum("dmg") + "  Mirrin-v1.0.0-macos.dmg\n" +
		sum("setup") + "  Mirrin-v1.0.0-windows-setup.exe\n" +
		sum("linux") + "  mirrin-linux-amd64\n"
	if n != 3 || string(b) != want {
		t.Fatalf("got %d files:\n%s\nwant:\n%s", n, b, want)
	}
}
