package backup

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives the package a throwaway MIRRIN_HOME, so a test that
// reaches config (a settings edit, a key) never touches the contributor's
// own ~/.mirrin, where even an empty lock file would stop a home from
// before the rename moving in.
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
