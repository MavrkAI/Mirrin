package api

import (
	"os"
	"testing"
	"time"
)

// chromeProfile is a throwaway Chrome profile for one test. Chrome's helper
// processes can go on writing to it (the disk cache) for a moment after the
// browser has quit, which made t.TempDir's cleanup fail now and then on a
// busy machine; it is removed once they have stopped.
func chromeProfile(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mirrin-api-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("Chrome's profile is still in use: %v", err)
				return
			}
		}
	})
	return dir
}
