//go:build (darwin && cgo) || linux || windows

package tray

import (
	"slices"
	"testing"
)

func TestWelcomeChromeUsesIsolatedProfile(t *testing.T) {
	args := welcomeChromeArgs("http://127.0.0.1:1234/welcome", "/temporary/welcome-profile")
	for _, want := range []string{"--use-mock-keychain", "--user-data-dir=/temporary/welcome-profile", "--app=http://127.0.0.1:1234/welcome"} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing %s in %v", want, args)
		}
	}
}
