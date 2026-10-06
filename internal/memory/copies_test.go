package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// Only a memory copy inside a backups folder can be recorded: forgetting
// deletes a copy it can't clean, so nothing else may ever be on the list.
func TestNoteCopyTakesOnlyMemoryBackups(t *testing.T) {
	s := openTest(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "backups", "20260927-150405", "data", "memory.db")
	for _, p := range []string{filepath.Join(dir, "notes.db"), filepath.Join(dir, "memory.db"), filepath.Join(dir, "backups", "config.yaml")} {
		if err := s.NoteCopy(p); err == nil {
			t.Errorf("recorded %s", p)
		}
	}
	if err := os.MkdirAll(filepath.Dir(good), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteCopy(good); err != nil {
		t.Fatal(err)
	}
	_ = s.NoteCopy(good)
	// A line added by hand that isn't a memory backup is ignored.
	f, _ := os.OpenFile(s.copiesFile(), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(filepath.Join(dir, "precious.db") + "\n")
	f.Close()
	s.bmu.Lock()
	got := s.copies()
	s.bmu.Unlock()
	if len(got) != 1 || got[0] != good {
		t.Fatalf("copies %v", got)
	}
}
