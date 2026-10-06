package main

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives the package a throwaway MIRRIN_HOME, so a test that forgets
// its own (home(t)) never reads or writes the contributor's ~/.mirrin, or
// moves their home from before the rename. A test that runs this binary
// again as mirrin (MIRRIN_TEST_MAIN) passes the home it wants.
func TestMain(m *testing.M) {
	if os.Getenv("MIRRIN_TEST_MAIN") != "" {
		os.Exit(m.Run())
	}
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
