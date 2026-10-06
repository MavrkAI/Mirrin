package voice

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAsyncCaptureKeepsItsOwnAudio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "helper.wav")
	if err := os.WriteFile(path, []byte("first clip"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &Channel{dataDir: dir}
	snapshot, cleanup, err := c.snapshotCapture(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(path, []byte("next clip"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(snapshot); err != nil || string(got) != "first clip" {
		t.Fatalf("snapshot = %q, %v", got, err)
	}
	cleanup()
	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Fatalf("snapshot left behind: %v", err)
	}
}
