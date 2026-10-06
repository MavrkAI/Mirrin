package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// defaultHomeAt makes home count as the default home for this test, so the
// homes from before the rename beside it are looked at, all inside a
// temporary folder.
func defaultHomeAt(t *testing.T, home string) {
	t.Helper()
	prev := isDefaultHome
	isDefaultHome = func(h string) bool { return h == home }
	t.Cleanup(func() { isDefaultHome = prev })
}

// A snapshot this machine's AntBot made names a standby key that lives in
// the twin AntBot left (~/.antbot) or set aside (~/.antbot.before-restore-…):
// it still came from this machine.
func TestStandbyKeysCoverTheHomeFromBeforeTheRename(t *testing.T) {
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
	setAside := key(filepath.Join(dir, ".antbot.before-restore-20261004-120000", "data"))
	leftBehind := key(filepath.Join(dir, ".antbot", "data"))
	unrelated := key(filepath.Join(dir, ".antbot-old", "data"))

	got := standbyKeys(home, filepath.Join(home, "data"))
	if !slices.Contains(got, own) || slices.Contains(got, setAside) {
		t.Fatalf("a home elsewhere than the default: %v", got)
	}
	defaultHomeAt(t, home)
	got = standbyKeys(home, filepath.Join(home, "data"))
	for name, k := range map[string]string{"own": own, "set aside by AntBot": setAside, "left behind by AntBot": leftBehind} {
		if !slices.Contains(got, k) {
			t.Errorf("standbyKeys misses the %s key", name)
		}
	}
	if slices.Contains(got, unrelated) {
		t.Error("a folder that only looks alike counted")
	}
}

// A restore that died midway under AntBot left a decrypted copy, keys and
// all, beside its home; the next restore clears it like its own.
func TestRestoreClearsAStagingCopyFromBeforeTheRename(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".mirrin")
	defaultHomeAt(t, home)
	old := time.Now().Add(-2 * time.Hour)
	stale := map[string]bool{
		"..antbot.restore-20261001-120000": true,
		"..mirrin.restore-20261001-120000": true,
		"..antbot.restore-20261004-115900": false, // a restore running now
		".antbot.restore-notes":            false, // not a staging copy
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

// A config from before the rename names the home as ~/.antbot (or
// ~/.openhuman): restored into ~/.mirrin, those paths point there. Paths
// that only start with the same letters stay.
func TestRelocateConfigMapsTildeHomesFromBeforeTheRename(t *testing.T) {
	stage := t.TempDir()
	home := filepath.Join(t.TempDir(), ".mirrin")
	path := filepath.Join(stage, "config.yaml")
	cfg := "name: Jeeves\nchannels:\n  voice:\n    whisper_model: ~/.antbot/models/ggml-base.en.bin\n    kokoro_dir: ~/.openhuman/tts\n" +
		"skills:\n  system:\n    allowed_dirs:\n      - ~/.antbot-old/x\n      - /Volumes/NAS/antbot-backups\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Home: "/Users/someone/.antbot", UserHome: "/Users/someone"}
	if _, err := relocateConfig(path, m, home, stage); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	for _, want := range []string{
		"whisper_model: " + filepath.Join(home, "models") + "/ggml-base.en.bin",
		"kokoro_dir: " + filepath.Join(home, "tts"),
		"~/.antbot-old/x",
		"/Volumes/NAS/antbot-backups",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config lacks %q:\n%s", want, out)
		}
	}
}
