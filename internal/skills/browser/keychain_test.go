package browser

import (
	"runtime"
	"testing"
)

// The quiet keychain check answers on a cgo Mac build and says it can't tell
// everywhere else; it never prompts either way.
func TestQuietKeychainCheck(t *testing.T) {
	locked, ok := quietKeychainLocked()
	if !cgoMac && (ok || locked) {
		t.Fatalf("without cgo on a Mac there is no quiet check: locked=%v ok=%v", locked, ok)
	}
	if cgoMac && runtime.GOOS != "darwin" {
		t.Fatal("cgo check built off a Mac")
	}
	t.Logf("locked=%v ok=%v", locked, ok)
}
