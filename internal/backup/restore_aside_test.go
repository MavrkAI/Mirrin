package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A snapshot naming a standby key that lives in the twin or in one an
// earlier restore set aside (~/.mirrin.before-restore-…) came from this
// machine.
func TestStandbyKeysCoverTheTwinsSetAside(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".mirrin")
	key := func(data string) string {
		id, err := standbyIdentity(data)
		if err != nil {
			t.Fatal(err)
		}
		return id.Recipient().String()
	}
	own := key(filepath.Join(home, "data"))
	setAside := key(filepath.Join(dir, ".mirrin.before-restore-20261004-120000", "data"))
	unrelated := key(filepath.Join(dir, ".mirrin-old", "data"))

	got := standbyKeys(home, filepath.Join(home, "data"))
	for name, k := range map[string]string{"own": own, "set aside": setAside} {
		if !slices.Contains(got, k) {
			t.Errorf("standbyKeys misses the %s key", name)
		}
	}
	if slices.Contains(got, unrelated) {
		t.Error("a folder that only looks alike counted")
	}
}

// A restore that died midway left a decrypted copy, keys and all, beside
// the home; the next restore clears it once it is an hour old.
func TestRestoreClearsAStaleStagingCopy(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".mirrin")
	old := time.Now().Add(-2 * time.Hour)
	stale := map[string]bool{
		"..mirrin.restore-20261001-120000": true,
		"..mirrin.restore-20261004-115900": false, // a restore running now
		".mirrin.restore-notes":            false, // not a staging copy
	}
	for name, isOld := range stale {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "secrets.env"), []byte("K=v\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if isOld || strings.HasSuffix(name, "notes") {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	clearStaleStaging(home)
	for name, isOld := range stale {
		_, err := os.Stat(filepath.Join(dir, name))
		if gone := os.IsNotExist(err); gone != isOld {
			t.Errorf("%s: gone %v, want %v", name, gone, isOld)
		}
	}
}
