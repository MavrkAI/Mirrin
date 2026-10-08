package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/config"
)

func TestSetupStoresOnlyPublicKeys(t *testing.T) {
	tw := newTwin(t)
	p, err := NewPhrase()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tw.home, "config.yaml")
	dir := t.TempDir()
	s, err := Setup(cfgPath, tw.data, p, config.Backup{Target: TargetFolder, Path: dir}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(cfgPath)
	if err != nil || got.Recipient != p.Recipient() || got.RecoveryPub != p.RecoveryPub() || got.Target != TargetFolder || got != s {
		t.Fatalf("settings: %+v %v", got, err)
	}
	// Take a snapshot too, then look everywhere on disk for the words.
	e := engineFor(t, tw, p, dir)
	runAt(t, e, time.Now())
	pq, _ := p.AgeIdentity()
	x, _ := p.X25519Identity()
	secrets := [][]byte{
		[]byte(strings.Join(p.Words(), " ")),
		[]byte(hex.EncodeToString(p.k[:])),
		p.k[:],
		[]byte(pq.String()),
		[]byte(x.String()),
		p.derive(infoPQ),
		p.derive(infoRecovery),
		[]byte(hex.EncodeToString(p.derive(infoPQ))),
	}
	for _, root := range []string{tw.home, dir} {
		filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, _ := os.ReadFile(path)
			for _, s := range secrets {
				if bytes.Contains(b, s) {
					t.Errorf("%s holds the words or a key made from them", path)
				}
			}
			// Twelve words in a row, in any spacing.
			if strings.Contains(strings.Join(strings.Fields(string(b)), " "), strings.Join(p.Words()[:4], " ")) {
				t.Errorf("%s holds part of the phrase", path)
			}
			return nil
		})
	}
	cfg, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(cfg), "# my twin") || !strings.Contains(string(cfg), plantedAPIKey) {
		t.Fatalf("setup rewrote the rest of the config:\n%s", cfg)
	}
}

func TestSettingsRoundTripAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("# keep me\nname: Ava # inline\ntray: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := config.Backup{Recipient: "age1pq1abc", RecoveryPub: "xyz", Target: TargetFolder, Path: "/Volumes/Backup"}
	if err := SaveSettings(path, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadSettings(path); got != s {
		t.Fatalf("got %+v", got)
	}
	s.Path = "/elsewhere"
	if err := SaveSettings(path, s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "backup:") != 1 || !strings.Contains(string(b), "/elsewhere") || !strings.Contains(string(b), "# keep me") || !strings.Contains(string(b), "# inline") {
		t.Fatalf("config:\n%s", b)
	}
	// The whole config still loads the normal way.
	var cfg config.Config
	if err := yaml.Unmarshal(b, &cfg); err != nil || cfg.Backup.Path != "/elsewhere" || cfg.Name != "Ava" {
		t.Fatalf("%v %+v", err, cfg.Backup)
	}
	if err := SaveSettings(path, config.Backup{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "backup") {
		t.Fatalf("empty settings should drop the section:\n%s", b)
	}
}

func TestRecoveryKitAndWordCheck(t *testing.T) {
	p := fixedPhrase(t)
	kit := RecoveryKit(p, KitInfo{Twin: "Jeeves", Where: "iCloud Drive › Mirrin Backups", Made: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)})
	for i, w := range p.Words() {
		if !strings.Contains(kit, fmt.Sprintf("%d. %s", i+1, w)) {
			t.Errorf("word %d (%s) missing from the kit", i+1, w)
		}
	}
	for _, want := range []string{"Jeeves", "27 September 2026", "mirrin restore", KitID(p)} {
		if !strings.Contains(kit, want) {
			t.Errorf("kit lacks %q", want)
		}
	}
	for _, line := range strings.Split(kit, "\n") {
		if len([]rune(line)) > 72 {
			t.Errorf("line too wide to print: %q", line)
		}
	}
	// Pasting the kit's columns back gives the same words.
	var cols []string
	for _, line := range strings.Split(kit, "\n") {
		if strings.Contains(line, "7. ") || strings.Contains(line, "8. ") || strings.Contains(line, "9. ") || strings.Contains(line, "10. ") || strings.Contains(line, "11. ") || strings.Contains(line, "12. ") {
			cols = append(cols, line)
		}
	}
	if back, err := ParsePhrase(strings.Join(cols, "\n")); err != nil || back != p {
		t.Fatalf("the kit doesn't paste back: %v", err)
	}
	if !CheckTyped(p, CheckWord, " Worth ") || !CheckTyped(p, CheckWord, "wort") || CheckTyped(p, CheckWord, "useful") || CheckTyped(p, CheckWord, "") {
		t.Fatal("word 7 check")
	}
}

func TestNudgeYearly(t *testing.T) {
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	if NudgeDue(State{}, now) || NudgeDue(State{KitShown: now.AddDate(0, -6, 0)}, now) {
		t.Fatal("nudged too early")
	}
	if !NudgeDue(State{KitShown: now.AddDate(-1, 0, -1)}, now) {
		t.Fatal("no nudge after a year")
	}
	if NudgeDue(State{KitShown: now.AddDate(-2, 0, 0), Nudged: now.AddDate(0, -1, 0)}, now) {
		t.Fatal("nudged twice in a year")
	}
	// A machine whose config came with the keys but never showed a kit
	// counts from when its backups began.
	if !NudgeDue(State{Since: now.AddDate(-1, 0, -1)}, now) || NudgeDue(State{Since: now.AddDate(0, -1, 0)}, now) {
		t.Fatal("no nudge counted from set-up")
	}
}

// A backup folder that isn't there (a disk or share not connected) fails
// the run. Making a fresh folder in its place would put the backups on the
// disk they are meant to protect, and the health check would stay green.
func TestAMissingBackupFolderFailsTheRun(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	base := filepath.Join(t.TempDir(), "NAS", "Backups")
	e := engineFor(t, tw, p, base)
	_, err := e.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "isn't there") || !strings.Contains(err.Error(), "connected") {
		t.Fatalf("got %v", err)
	}
	if exists(base) || exists(filepath.Dir(base)) {
		t.Fatal("the run made the missing backup folder")
	}
	st, _ := LoadState(tw.data)
	if !st.LastGood.IsZero() || !strings.Contains(st.LastError, "isn't there") {
		t.Fatalf("state: %+v", st)
	}
	// Connected again: the namespace folder inside it is made as needed.
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(base, p.Namespace())) {
		t.Fatal("no namespace folder")
	}
	// Listing a folder that isn't there says so, rather than "no backups".
	if _, err := nsFolder(filepath.Join(t.TempDir(), "gone"), p.Namespace(), "").List(context.Background()); err == nil || !strings.Contains(err.Error(), "isn't there") {
		t.Fatalf("list: %v", err)
	}
}

// A folder inside the twin's own folder isn't a backup: it's on the same
// disk, and a restore moves it aside with the twin.
func TestABackupFolderInsideTheTwinIsRefused(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	cfgPath := filepath.Join(tw.home, "config.yaml")
	for _, inside := range []string{filepath.Join(tw.home, "backups"), tw.home, filepath.Join(tw.data, "b")} {
		_, err := Setup(cfgPath, tw.data, p, config.Backup{Target: TargetFolder, Path: inside}, time.Now())
		if err == nil || !strings.Contains(err.Error(), "outside Mirrin's own folder") {
			t.Fatalf("Setup %s: %v", inside, err)
		}
		if _, err := SetTarget(cfgPath, config.Backup{Target: TargetFolder, Path: inside}); err == nil || !strings.Contains(err.Error(), "outside Mirrin's own folder") {
			t.Fatalf("SetTarget %s: %v", inside, err)
		}
	}
	if s, _ := LoadSettings(cfgPath); s.Recipient != "" || s.Path != "" {
		t.Fatalf("a refused folder was saved: %+v", s)
	}
	outside := filepath.Join(t.TempDir(), "Backups")
	if _, err := Setup(cfgPath, tw.data, p, config.Backup{Target: TargetFolder, Path: outside}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(outside, p.Namespace())) {
		t.Fatal("set-up didn't make the backup folder")
	}
	// Changing where backups go makes the new folder once, too.
	other := filepath.Join(t.TempDir(), "Other")
	if _, err := SetTarget(cfgPath, config.Backup{Target: TargetFolder, Path: other}); err != nil || !exists(other) {
		t.Fatalf("SetTarget: %v", err)
	}
}

func TestICloudDrivePath(t *testing.T) {
	user := t.TempDir()
	if _, err := icloudPathIn(user, ""); err == nil || !strings.Contains(err.Error(), "iCloud Drive isn't turned on") {
		t.Fatalf("got %v", err)
	}
	drive := icloudDriveIn(user)
	if err := os.MkdirAll(drive, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"", "ns1"} {
		p, err := icloudPathIn(user, ns)
		if err != nil || p != filepath.Join(drive, "Mirrin Backups") {
			t.Fatalf("namespace %q: %q %v", ns, p, err)
		}
	}
}

// Backups set up before the rename went to iCloud Drive › AntBot Backups,
// and keep going there: Recovery Kits name it, and a machine standing by
// watches it. New set-ups, and a Mac that has Mirrin's folder, use that.
func TestICloudDrivePathKeepsTheOldFolder(t *testing.T) {
	user := t.TempDir()
	drive := icloudDriveIn(user)
	old := filepath.Join(drive, "AntBot Backups")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if p, err := icloudPathIn(user, ""); err != nil || p != old {
		t.Fatalf("with the old folder: %q %v", p, err)
	}
	if err := os.MkdirAll(filepath.Join(drive, "Mirrin Backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	if p, err := icloudPathIn(user, ""); err != nil || p != filepath.Join(drive, "Mirrin Backups") {
		t.Fatalf("with both folders: %q %v", p, err)
	}
	if !exists(old) {
		t.Fatal("the old folder was moved")
	}
}

// The folder is chosen for each set of words, not for the whole drive: a
// "Mirrin Backups" made by another Mac on the same Apple ID (which hadn't
// seen the old folder yet) doesn't send a Mac whose backups are in the old
// folder to an empty one, where its backups would look gone. Backups
// of the same words in either folder are listed and read.
func TestICloudChoosesTheFolderPerNamespace(t *testing.T) {
	ctx := context.Background()
	user := t.TempDir()
	drive := icloudDriveIn(user)
	old, cur := filepath.Join(drive, "AntBot Backups"), filepath.Join(drive, "Mirrin Backups")
	last := newName("snap", time.Now().Add(-48*time.Hour))
	put := func(dir, name string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("ciphertext"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(old, "mine"), last)
	put(filepath.Join(cur, "theirs"), newName("snap", time.Now()))

	if p, err := icloudPathIn(user, "mine"); err != nil || p != old {
		t.Fatalf("this Mac's words: %q %v, want the old folder", p, err)
	}
	if p, err := icloudPathIn(user, "theirs"); err != nil || p != cur {
		t.Fatalf("the other Mac's words: %q %v", p, err)
	}
	if p, err := icloudPathIn(user, "new"); err != nil || p != cur {
		t.Fatalf("new words: %q %v", p, err)
	}

	tg := icloudTarget(old, "mine")
	e := &Engine{Target: tg}
	if err := e.checkGone(ctx, State{LastName: last}); err != nil {
		t.Fatalf("the backups look gone: %v", err)
	}
	if !strings.Contains(tg.String(), "AntBot Backups") {
		t.Fatalf("named %q", tg)
	}

	// The same words in both folders: both are listed and read, and new
	// backups go where the older ones are.
	newer := newName("snap", time.Now().Add(-time.Hour))
	put(filepath.Join(cur, "mine"), newer)
	tg = icloudTarget(old, "mine")
	objs, err := tg.List(ctx)
	if err != nil || len(objs) != 2 || objs[0].Name != last || objs[1].Name != newer {
		t.Fatalf("listed %v %v, want both folders' backups", objs, err)
	}
	rc, err := tg.Get(ctx, newer)
	if err != nil {
		t.Fatalf("reading one from Mirrin's folder: %v", err)
	}
	rc.Close()
	next := newName("snap", time.Now())
	if err := tg.Put(ctx, next, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(old, "mine", next)) {
		t.Fatal("a new backup didn't go beside the older ones")
	}
	if err := tg.Delete(ctx, newer); err != nil || exists(filepath.Join(cur, "mine", newer)) {
		t.Fatalf("pruning missed Mirrin's folder: %v", err)
	}
}

func TestFolderTargetRejectsOtherNames(t *testing.T) {
	f := Folder(t.TempDir())
	for _, bad := range []string{"../x.age", "snap-x.age", "config.yaml"} {
		if _, err := f.Get(t.Context(), bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, err := f.Get(t.Context(), newName("snap", time.Now())); err != ErrNotFound {
		t.Fatalf("missing snapshot: %v", err)
	}
}
