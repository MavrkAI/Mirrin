package config

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives the package a throwaway MIRRIN_HOME, so a test that forgets
// its own never reads or writes the contributor's ~/.mirrin.
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
