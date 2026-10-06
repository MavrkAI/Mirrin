package custom

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives every test in the package a throwaway MIRRIN_HOME. A custom
// tool runs with the variables it names (procenv.With), looked up in the
// home's secrets.env when this process lacks them, and a test run must never
// read, or write, the contributor's own.
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
