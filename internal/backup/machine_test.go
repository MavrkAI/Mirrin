package backup

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/config"
	devreg "github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Regression (backup merged with devices): the restore checklist said the
// new master key cut every other device off and they must pair again, but
// each paired device has its own key in devices.json, which came back with
// the twin: they still work. The checklist says so, and points at the
// device review, the real safeguard.
func TestRestoredDevicesKeepTheirOwnKeys(t *testing.T) {
	tw := newTwin(t)
	cfg, _ := os.ReadFile(filepath.Join(tw.home, "config.yaml"))
	if err := os.WriteFile(filepath.Join(tw.home, "config.yaml"), append(cfg, []byte("api:\n    remote: true\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := devreg.Open(devreg.Path(tw.data))
	if err != nil {
		t.Fatal(err)
	}
	phone, tok, err := reg.Add("Tablet", devreg.KindPWA, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	p := fixedPhrase(t)
	dir := t.TempDir()
	e := engineFor(t, tw, p, dir)
	runAt(t, e, time.Now().Add(-time.Hour))
	newHome := filepath.Join(t.TempDir(), ".mirrin")
	r, err := Restore(context.Background(), RestoreOptions{Target: nsFolder(dir, p.Namespace(), ""), Phrase: p, Home: newHome, HostLabel: "New Mac"})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := devreg.Open(devreg.Path(filepath.Join(newHome, "data")))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := restored.Authenticate(tok); !ok || d.ID != phone.ID {
		t.Fatal("the restored twin doesn't know the device's own key: the test's premise changed")
	}
	if slices.ContainsFunc(r.Checklist, func(s string) bool { return strings.Contains(s, "mirrin pair") }) {
		t.Fatalf("the checklist says devices must pair again, which isn't so: %q", r.Checklist)
	}
	if !slices.ContainsFunc(r.Checklist, func(s string) bool {
		return strings.Contains(s, "keep their own keys") && strings.Contains(s, "mirrin devices revoke")
	}) {
		t.Fatalf("checklist: %q", r.Checklist)
	}
	if !slices.ContainsFunc(r.Devices, func(s string) bool { return strings.Contains(s, "Tablet") }) {
		t.Fatalf("device review: %q", r.Devices)
	}
}

// Regression (time merged with backup): the snapshot's memory brings back
// when the old machine's scheduler last looked (so the first start made up
// runs the old machine had already done) and whether it was paused (so a
// restore could silently start a twin paused, or lift this machine's own
// pause). Those belong to the machine: the restored twin keeps this
// machine's pause, makes up nothing from before the restore, and says when
// the snapshot's twin was paused.
func TestRestoreKeepsThisMachinesPauseAndClock(t *testing.T) {
	setKV := func(t *testing.T, data string, kv map[string]string) {
		t.Helper()
		st, err := memory.Open(data)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		for k, v := range kv {
			if v == "" {
				_ = st.Unset(context.Background(), k)
			} else if err := st.Set(context.Background(), k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	old := time.Now().Add(-9 * time.Hour).UTC().Format(time.RFC3339Nano)
	tw := newTwin(t)
	setKV(t, tw.data, map[string]string{memory.KeyAlive: old, memory.KeyPaused: old})
	p := fixedPhrase(t)
	dir := t.TempDir()
	e := engineFor(t, tw, p, dir)
	runAt(t, e, time.Now().Add(-time.Hour))

	// On a new machine: running, told so, nothing made up.
	newHome := filepath.Join(t.TempDir(), ".mirrin")
	before := time.Now()
	r, err := Restore(context.Background(), RestoreOptions{Target: nsFolder(dir, p.Namespace(), ""), Phrase: p, Home: newHome, HostLabel: "New Mac"})
	if err != nil {
		t.Fatal(err)
	}
	got := memory.MachineState(filepath.Join(newHome, "data", "memory.db"))
	if got[memory.KeyPaused] != "" {
		t.Fatal("the new machine starts paused without saying so")
	}
	if at, err := time.Parse(time.RFC3339Nano, got[memory.KeyAlive]); err != nil || at.Before(before.Add(-time.Second)) {
		t.Fatalf("the scheduler's last look is still the old machine's: %q", got[memory.KeyAlive])
	}
	if !slices.ContainsFunc(r.Checklist, func(s string) bool { return strings.Contains(s, "paused when this snapshot was taken") }) {
		t.Fatalf("checklist: %q", r.Checklist)
	}

	// Back on the same machine, now running: the owner's current state stays.
	setKV(t, tw.data, map[string]string{memory.KeyPaused: ""})
	if _, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: tw.home}); err != nil {
		t.Fatal(err)
	}
	if got := memory.MachineState(filepath.Join(tw.home, "data", "memory.db")); got[memory.KeyPaused] != "" {
		t.Fatal("rolling back re-paused a twin the owner had resumed")
	}
	// And paused now: it stays paused.
	now := time.Now().UTC().Format(time.RFC3339)
	setKV(t, tw.data, map[string]string{memory.KeyPaused: now})
	if _, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: tw.home}); err != nil {
		t.Fatal(err)
	}
	if got := memory.MachineState(filepath.Join(tw.home, "data", "memory.db")); got[memory.KeyPaused] != now {
		t.Fatalf("rolling back lifted the owner's pause: %q", got[memory.KeyPaused])
	}
	// The memory itself is the snapshot's.
	st, err := memory.Open(filepath.Join(tw.home, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if fs, _ := st.AllFacts(context.Background(), 10); len(fs) == 0 {
		t.Fatal("the restored memory lost its facts")
	}
}

// Regression (observability merged with backup): the encrypted snapshot
// copied memory.db without the check the daily copies and a restore make,
// so a memory damaged while the twin ran could become the newest snapshot,
// and retention could then prune the good ones. It is refused, and the
// snapshots already there stay as they were.
func TestADamagedMemoryIsNeverSnapshotted(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	dir := t.TempDir()
	e := engineFor(t, tw, p, dir)
	runAt(t, e, time.Now().Add(-2*time.Hour))
	good, _ := List(context.Background(), e.Target, p)
	// Fill the memory so the damage lands in real pages, then damage it.
	st, err := memory.Open(tw.data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 400 {
		_, _ = st.Remember(context.Background(), "user", strings.Repeat("filler ", 40)+string(rune('a'+i%26)), "t")
	}
	st.Close()
	path := filepath.Join(tw.data, "memory.db")
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := f.Stat()
	junk := []byte(strings.Repeat("\xde\xad\xbe\xef", 1024))
	for off := info.Size() - 3*4096; off < info.Size(); off += 4096 {
		_, _ = f.WriteAt(junk, off)
	}
	f.Close()
	e.Now = func() time.Time { return time.Now().Add(-time.Hour) }
	// VACUUM INTO refuses damage in the tables it reads; what it would copy
	// regardless is caught by the check on the copy. Either way: no snapshot.
	if _, err := e.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("a damaged memory was backed up (err %v)", err)
	}
	if after, _ := List(context.Background(), e.Target, p); len(after) != len(good) {
		t.Fatalf("snapshots before %d, after %d", len(good), len(after))
	}

	// Damage VACUUM INTO copies as it is is caught by the check on the copy.
	tw2 := newTwin(t)
	e2 := engineFor(t, tw2, p, t.TempDir())
	prev := vacuumInto
	t.Cleanup(func() { vacuumInto = prev })
	vacuumInto = func(ctx context.Context, src, dst string) error {
		if err := vacuumIntoFile(ctx, src, dst); err != nil || !strings.HasSuffix(filepath.ToSlash(dst), nameMemory) {
			return err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		info, _ := f.Stat()
		_, err = f.WriteAt([]byte(strings.Repeat("\xde\xad\xbe\xef", 1024)), info.Size()-4096)
		return err
	}
	e2.Now = func() time.Time { return time.Now().Add(-time.Hour) }
	if _, err := e2.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "doesn't pass SQLite's check") {
		t.Fatalf("a copy that fails the check was saved (err %v)", err)
	}
	if after, _ := List(context.Background(), e2.Target, p); len(after) != 0 {
		t.Fatalf("snapshots: %d", len(after))
	}
}

// A config saved before settings were layered has the zone the old machine
// had at install written in. There it followed the system while they
// matched (config.upgrade); restored on a machine in another zone it was
// taken for a pin. It keeps following the system; a zone that didn't match
// (the owner's own pin) stays.
func TestARestoredFullDumpKeepsFollowingTheSystemsZone(t *testing.T) {
	root := func(cfg string) *yaml.Node {
		t.Helper()
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
			t.Fatal(err)
		}
		return &doc
	}
	for _, c := range []struct {
		name, cfg, want string
	}{
		{"install-time zone", "name: Jeeves\nuser:\n  timezone: Australia/Sydney\n", "Local"},
		{"a pinned zone", "name: Jeeves\nuser:\n  timezone: Asia/Tokyo\n", "Asia/Tokyo"},
		{"already layered", "config_version: 2\nuser:\n  timezone: Australia/Sydney\n", "Australia/Sydney"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(c.cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			m := Manifest{Zone: "Australia/Sydney", Home: dir}
			if _, err := relocateConfig(path, m, dir, dir); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(path)
			tz := find(root(string(b)).Content[0], []string{"user", "timezone"})
			if tz == nil || tz.Value != c.want {
				t.Fatalf("timezone after restore:\n%s", b)
			}
		})
	}
	// The engine records the zone it ran in.
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	if m, _ := runAt(t, e, time.Now().Add(-time.Hour)); m.Zone != config.LocalTimezone() {
		t.Fatalf("manifest zone %q", m.Zone)
	}
}

// A restore keeps the twin that was here aside, never deleting it; its
// memory is registered as a copy, so forgetting a fact in the restored twin
// scrubs the one set aside too (site's "forgetting is thorough" merged with
// backup).
func TestForgettingAfterARestoreReachesTheTwinSetAside(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	runAt(t, e, time.Now().Add(-time.Hour))
	r, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: tw.home})
	if err != nil || r.Aside == "" {
		t.Fatalf("restore: %+v %v", r, err)
	}
	st, err := memory.Open(filepath.Join(tw.home, "data"))
	if err != nil {
		t.Fatal(err)
	}
	facts, _ := st.AllFacts(context.Background(), 10)
	var id int64
	for _, f := range facts {
		if f.Content == "likes flat whites" {
			id = f.ID
		}
	}
	if id == 0 {
		t.Fatalf("setup: facts %+v", facts)
	}
	if err := st.Forget(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	st.Close()
	b, err := os.ReadFile(filepath.Join(r.Aside, "data", "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "likes flat whites") {
		t.Fatal("the twin set aside by the restore still holds the forgotten fact")
	}
}
