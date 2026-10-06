package config

import (
	"os"
	"strings"
	"testing"
)

// A twin without backups keeps a config with no backup section, so configs
// written before backups existed stay as they were; one with backups keeps
// only public keys and where they go.
func TestBackupSettingsSaveOnlyWhenSet(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := Default()
	cfg.LLM.APIKey = "sk-test"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path())
	if strings.Contains(string(b), "backup") {
		t.Fatalf("an unset backup section was written:\n%s", b)
	}
	cfg.Backup = Backup{Recipient: "age1pq1abc", RecoveryPub: "pub", Target: "folder", Path: "/Volumes/B"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if got == nil {
		t.Fatal(err)
	}
	if got.Backup != cfg.Backup {
		t.Fatalf("got %+v", got.Backup)
	}
}
