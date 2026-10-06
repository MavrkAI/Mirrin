package procenv

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestMain gives the package a throwaway MIRRIN_HOME: a name that isn't in
// the environment is looked up in secrets.env there, never in the
// contributor's own ~/.mirrin.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "mirrin-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("MIRRIN_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// Keys saved with mirrin live in secrets.env and are no longer exported into
// the daemon's environment; a program that names one still gets it.
func TestNamedSecretsSavedWithMirrinArePassedOn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "secrets.env"), []byte("GITHUB_TOKEN=ghp_saved\nANTHROPIC_API_KEY=sk-saved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Expand("$GITHUB_TOKEN"); got != "ghp_saved" {
		t.Fatalf("Expand = %q", got)
	}
	env := With("GITHUB_TOKEN")
	if !slices.Contains(env, "GITHUB_TOKEN=ghp_saved") || slices.Contains(env, "ANTHROPIC_API_KEY=sk-saved") {
		t.Fatalf("With = %v", env)
	}
	// The environment still wins over the file.
	t.Setenv("GITHUB_TOKEN", "ghp_env")
	if got := Expand("${GITHUB_TOKEN}"); got != "ghp_env" {
		t.Fatalf("Expand = %q", got)
	}
	if env := With("GITHUB_TOKEN"); !slices.Contains(env, "GITHUB_TOKEN=ghp_env") || slices.Contains(env, "GITHUB_TOKEN=ghp_saved") {
		t.Fatalf("With = %v", env)
	}
}
