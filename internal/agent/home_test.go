package agent

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives every test in the package a throwaway MIRRIN_HOME.
// config.Default() resolves the home directory, which moves a pre-rename
// ~/.openhuman into place, so a test run could otherwise move or rewrite the
// contributor's own twin.
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
