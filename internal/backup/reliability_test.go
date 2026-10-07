package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
)

// A network drive that isn't connected leaves its mount point there but
// empty: the backups this machine wrote are gone from it. A run refuses
// rather than fill the empty mount point on this very disk, and choosing
// where backups go again starts over.
func TestBackupsGoneFromTheTargetAreRefused(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	tw := newTwin(t)
	p := fixedPhrase(t)
	mount := t.TempDir()
	e := engineFor(t, tw, p, mount)
	runAt(t, e, time.Now().Add(-time.Hour))
	dir := e.Target.(*folder).dir
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	e.Now = time.Now
	_, err := e.Run(context.Background())
	if !errors.Is(err, ErrGone) || !strings.Contains(err.Error(), "the backups in "+mount+" are gone; is the drive connected?") {
		t.Fatalf("run over an empty mount point: %v", err)
	}
	if es, _ := os.ReadDir(mount); len(es) != 0 {
		t.Fatalf("a run wrote into the empty mount point: %v", es)
	}
	if st, _ := LoadState(tw.data); !strings.Contains(st.LastError, "is the drive connected?") {
		t.Fatalf("the health check doesn't hear why: %+v", st)
	}
	// `mirrin backup target` starts over.
	cfgPath := filepath.Join(tw.home, "config.yaml")
	if _, err := SetTarget(cfgPath, config.Backup{Target: TargetFolder, Path: mount}); err != nil {
		t.Fatal(err)
	}
	if st, _ := LoadState(tw.data); st.LastName != "" {
		t.Fatalf("choosing the target again kept the old record: %+v", st)
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("after choosing the target again: %v", err)
	}
}

// macOS 12 and 13 leave ".<name>.icloud" for a file iCloud Drive moved off
// the Mac: it lists under its name, and Get has it downloaded first.
func TestICloudPlaceholdersListAndDownload(t *testing.T) {
	dir := t.TempDir()
	name := newName("snap", time.Now())
	if err := os.WriteFile(filepath.Join(dir, placeholderOf(name)), []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	tg := Folder(dir)
	objs, err := tg.List(context.Background())
	if err != nil || len(objs) != 1 || objs[0].Name != name || objs[0].Size != 0 {
		t.Fatalf("list (an evicted file's size isn't known): %+v %v", objs, err)
	}
	var asked []string
	was, wasPoll := icloudDownload, icloudPoll
	t.Cleanup(func() { icloudDownload, icloudPoll = was, wasPoll })
	icloudPoll = 10 * time.Millisecond
	icloudDownload = func(_ context.Context, p string) error {
		asked = append(asked, p)
		go func() { // iCloud Drive brings it back a moment later
			time.Sleep(30 * time.Millisecond)
			_ = os.WriteFile(p, []byte("ciphertext"), 0o600)
			_ = os.Remove(filepath.Join(dir, placeholderOf(name)))
		}()
		return nil
	}
	rc, err := tg.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b := make([]byte, 64)
	n, _ := rc.Read(b)
	if string(b[:n]) != "ciphertext" || len(asked) != 1 || asked[0] != filepath.Join(dir, name) {
		t.Fatalf("got %q, downloads asked %v", b[:n], asked)
	}
	// A name with neither file nor placeholder is still not found.
	if _, err := tg.Get(context.Background(), newName("snap", time.Now().Add(time.Hour))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// Deleting an evicted snapshot removes its placeholder.
	other := newName("snap", time.Now().Add(-time.Hour))
	_ = os.WriteFile(filepath.Join(dir, placeholderOf(other)), []byte("plist"), 0o600)
	if err := tg.Delete(context.Background(), other); err != nil || exists(filepath.Join(dir, placeholderOf(other))) {
		t.Fatalf("delete: %v", err)
	}
}

// Once a month the schedule reads the newest snapshot back and checks it
// is what was written; a damaged one turns the health check red.
func TestMonthlyRestoreDrill(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	t0 := time.Now().Add(-100 * 24 * time.Hour)
	runAt(t, e, t0)
	s := schedulerFor(e)
	clock := t0.Add(24 * time.Hour)
	s.Now = func() time.Time { return clock }
	var last time.Time
	s.tick(context.Background(), &last, time.Hour)
	if st, _ := LoadState(tw.data); !st.DrillAt.IsZero() {
		t.Fatalf("the drill ran a day in: %+v", st)
	}
	clock = t0.Add(31 * 24 * time.Hour)
	s.tick(context.Background(), &last, time.Hour)
	st, _ := LoadState(tw.data)
	if !st.DrillAt.Equal(clock) || st.DrillError != "" {
		t.Fatalf("the drill didn't run and pass 31 days on: %+v", st)
	}
	// The disk damages the newest snapshot; a month on, the drill notices.
	snap := snapshotPath(e, st.LastName)
	b, _ := os.ReadFile(snap)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(snap, b, 0o600); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(31 * 24 * time.Hour)
	s.tick(context.Background(), &last, time.Hour)
	st, _ = LoadState(tw.data)
	if st.DrillError == "" {
		t.Fatalf("the drill missed a damaged snapshot: %+v", st)
	}
	state, what, fix := Health(st, Settings{Recipient: p.Recipient()}, clock)
	if state != health.Fail || !strings.Contains(what, "no longer matches") || fix == "" {
		t.Fatalf("health: %v %q %q", state, what, fix)
	}
}

// A name macOS and Linux allow but Windows refuses comes back under a
// changed name there, and the restore report lists it.
func TestRestoringWindowsRefusedNamesRenamesThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows can't make these names; the test makes them elsewhere and restores as Windows would")
	}
	tw := newTwin(t)
	for _, n := range []string{"a?b.txt", "a_b.txt", `q"<*>|.md`} {
		if err := os.WriteFile(filepath.Join(tw.home, "protocols", n), []byte(n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	runAt(t, e, time.Now())
	was := windowsNames
	t.Cleanup(func() { windowsNames = was })
	windowsNames = true
	home := filepath.Join(t.TempDir(), ".mirrin")
	r, err := Restore(context.Background(), RestoreOptions{Target: e.Target, Phrase: p, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{"a_b.txt": "a_b.txt", "a_b (2).txt": "a?b.txt", "q_____.md": `q"<*>|.md`} {
		got, err := os.ReadFile(filepath.Join(home, "protocols", file))
		if err != nil || string(got) != body {
			t.Errorf("%s: %q %v", file, got, err)
		}
	}
	want := []string{"protocols/a?b.txt is now protocols/a_b (2).txt", `protocols/q"<*>|.md is now protocols/q_____.md`}
	slices.Sort(r.Renamed)
	if !slices.Equal(r.Renamed, want) {
		t.Fatalf("report lists %q", r.Renamed)
	}
	for _, c := range []string{":", `\`, "?", "*", "<", ">", "|", `"`} {
		if !strings.Contains(AwkwardChars, c) {
			t.Errorf("the warning doesn't name %q", c)
		}
	}
}

// With backup.signal on, a snapshot carries signal-cli's data folder and a
// restore with --with-sessions puts it back; with it off, it isn't there.
func TestSignalSessionBackupIsOptIn(t *testing.T) {
	tw := newTwin(t)
	sig := filepath.Join(t.TempDir(), "signal-cli")
	for rel, body := range map[string]string{"data/accounts.json": `{"accounts":[]}`, "data/123456.d/account.db": "keys"} {
		pth := filepath.Join(sig, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(pth), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pth, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := fixedPhrase(t)
	tw.layout.SignalDir = sig

	off := engineFor(t, tw, p, t.TempDir())
	_, name := runAt(t, off, time.Now())
	f, _ := os.Open(snapshotPath(off, name))
	names, _ := entries(t, f, p.Identities()...)
	f.Close()
	if slices.Contains(names, nameSignal) {
		t.Fatal("Signal's session was backed up with the flag off")
	}

	on := engineFor(t, tw, p, t.TempDir())
	on.WithSignal = true
	if err := forgetLast(tw.data); err != nil { // another target, as `mirrin backup target` does
		t.Fatal(err)
	}
	_, name = runAt(t, on, time.Now())
	f, _ = os.Open(snapshotPath(on, name))
	names, _ = entries(t, f, p.Identities()...)
	f.Close()
	if !slices.Contains(names, nameSignal) {
		t.Fatalf("no Signal tar with the flag on: %v", names)
	}

	// Without --with-sessions it stays out, and the checklist says how.
	home := filepath.Join(t.TempDir(), ".mirrin")
	dest := filepath.Join(t.TempDir(), "signal-cli")
	r, err := Restore(context.Background(), RestoreOptions{Target: on.Target, Phrase: p, Home: home, SignalDir: dest})
	if err != nil {
		t.Fatal(err)
	}
	if exists(dest) || exists(filepath.Join(home, filepath.FromSlash(nameSignal))) || !r.SessionsLeft || r.Signal != "" {
		t.Fatalf("Signal's session came back without --with-sessions: %+v", r)
	}

	home = filepath.Join(t.TempDir(), ".mirrin")
	if err := os.MkdirAll(dest, 0o700); err != nil { // one already here is kept aside
		t.Fatal(err)
	}
	r, err = Restore(context.Background(), RestoreOptions{Target: on.Target, Phrase: p, Home: home, SignalDir: dest, WithSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "data", "123456.d", "account.db")); err != nil || string(b) != "keys" || r.Signal != dest {
		t.Fatalf("Signal's session wasn't put back: %q %v %+v", b, err, r)
	}
	if exists(filepath.Join(home, filepath.FromSlash(nameSignal))) {
		t.Fatal("the Signal tar was left in the restored home")
	}
	if asides, _ := filepath.Glob(dest + ".before-restore-*"); len(asides) != 1 {
		t.Fatalf("the folder that was there wasn't kept aside: %v", asides)
	}

	// The checklist asks to link Signal only when its session didn't come back.
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	_ = os.WriteFile(cfg, []byte("channels:\n    signal:\n        enabled: true\n        account: \"+15550000\"\n"), 0o600)
	has := func(list []string) bool {
		return slices.ContainsFunc(list, func(s string) bool { return strings.HasPrefix(s, "Signal:") })
	}
	if has(checklist(cfg, home, false, false, true)) || !has(checklist(cfg, home, true, true, false)) {
		t.Fatal("the checklist's Signal line is wrong")
	}
	if !strings.Contains(strings.Join(checklist(cfg, home, true, true, false), "\n"), "--with-sessions") {
		t.Fatal("the checklist doesn't say how to bring Signal's session")
	}
}

// The twin moved to machine B, which writes to the same folder and whose
// retention pruned this machine's last snapshot. The drive is connected
// (B's snapshots are there): a run here isn't refused, and the drill
// doesn't call the backups damaged.
func TestAPrunedLastSnapshotIsNotGone(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	t0 := time.Now().Add(-100 * 24 * time.Hour)
	_, last := runAt(t, e, t0)
	other := newName("snap", t0.Add(time.Hour)) // machine B's
	if err := os.WriteFile(snapshotPath(e, other), []byte("B's snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(snapshotPath(e, last)); err != nil {
		t.Fatal(err)
	}
	// The drill a month on skips it rather than turn the health check red.
	s := schedulerFor(e)
	clock := t0.Add(31 * 24 * time.Hour)
	s.Now = func() time.Time { return clock }
	s.drillIfDue(context.Background())
	if st, _ := LoadState(tw.data); st.DrillError != "" || !st.DrillAt.Equal(clock) {
		t.Fatalf("the drill failed on a snapshot another machine pruned: %+v", st)
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("a run with other snapshots in the folder: %v", err)
	}
	// `mirrin backup resume` forgets the last snapshot too.
	if err := StandBy(tw.data, Handover{Name: "handover-x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Resume(tw.data); err != nil {
		t.Fatal(err)
	}
	if st, _ := LoadState(tw.data); st.LastName != "" {
		t.Fatalf("resume kept the last snapshot: %+v", st)
	}
}

// A failed drill stays until the drill passes again (a nightly run doesn't
// hide damage to the disk), but choosing where backups go clears it.
func TestAFailedDrillClearsOnANewTarget(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	runAt(t, e, time.Now().Add(-time.Hour))
	fail := func() {
		t.Helper()
		if err := UpdateState(tw.data, func(s *State) { s.DrillAt, s.DrillError = time.Now(), "damaged" }); err != nil {
			t.Fatal(err)
		}
	}
	fail()
	runAt(t, e, time.Now())
	if st, _ := LoadState(tw.data); st.DrillError == "" {
		t.Fatalf("a nightly run hid the failed drill: %+v", st)
	}
	if err := forgetLast(tw.data); err != nil {
		t.Fatal(err)
	}
	if st, _ := LoadState(tw.data); st.DrillError != "" || !st.DrillAt.IsZero() {
		t.Fatalf("a new target kept the failed drill: %+v", st)
	}
}

// signal-cli keeps writing its folder while a backup copies it: each
// database is copied consistently, SQLite's side files and attachments are
// left out, and a file that goes away meanwhile doesn't fail the backup.
// A folder that can't be copied at all leaves only Signal out.
func TestSignalSessionIsCopiedWhileSignalCliRuns(t *testing.T) {
	tw := newTwin(t)
	sig := filepath.Join(t.TempDir(), "signal-cli")
	write := func(rel, body string) {
		t.Helper()
		pth := filepath.Join(sig, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(pth), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pth, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("data/accounts.json", `{"accounts":[]}`)
	write("data/1.d/account.db-wal", "wal")
	write("data/1.d/account.db-shm", "shm")
	write("data/zz-gone.txt", "going")
	write("attachments/photo.jpg", "big")
	db := filepath.Join(sig, "data", "1.d", "account.db")
	makeDB(t, db)
	tw.layout.SignalDir = sig
	p := fixedPhrase(t)

	was := vacuumInto
	t.Cleanup(func() { vacuumInto = was })
	vacuumInto = func(ctx context.Context, src, dst string) error {
		if src == db { // signal-cli writes on while the folder is copied
			_ = os.Remove(filepath.Join(sig, "data", "zz-gone.txt"))
			f, _ := os.OpenFile(filepath.Join(sig, "data", "accounts.json"), os.O_APPEND|os.O_WRONLY, 0o600)
			_, _ = f.WriteString(strings.Repeat(" ", 1<<16))
			_ = f.Close()
		}
		return was(ctx, src, dst)
	}
	e := engineFor(t, tw, p, t.TempDir())
	e.WithSignal = true
	_, name := runAt(t, e, time.Now())
	f, _ := os.Open(snapshotPath(e, name))
	_, files := entries(t, f, p.Identities()...)
	f.Close()
	raw, ok := files[nameSignal]
	if !ok {
		t.Fatal("no Signal tar")
	}
	got := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = b
	}
	if b := got["data/1.d/account.db"]; !bytes.HasPrefix(b, []byte(sqliteMagic)) {
		t.Fatalf("the account database wasn't copied: %q", b)
	}
	for _, n := range []string{"data/1.d/account.db-wal", "data/1.d/account.db-shm", "data/zz-gone.txt", "attachments/", "attachments/photo.jpg"} {
		if _, ok := got[n]; ok {
			t.Errorf("%s is in the Signal tar", n)
		}
	}
	if _, ok := got["data/accounts.json"]; !ok {
		t.Error("accounts.json is missing")
	}

	// A database that can't be copied leaves Signal out, not the backup.
	vacuumInto = func(ctx context.Context, src, dst string) error {
		if src == db {
			return errors.New("database is locked")
		}
		return was(ctx, src, dst)
	}
	_, name = runAt(t, e, time.Now().Add(time.Hour))
	f, _ = os.Open(snapshotPath(e, name))
	names, _ := entries(t, f, p.Identities()...)
	f.Close()
	if slices.Contains(names, nameSignal) || !slices.Contains(names, nameMemory) {
		t.Fatalf("a Signal folder that couldn't be copied: %v", names)
	}
}

// A mistaken backup.signal_dir (the home folder, a root, a folder holding
// the twin) is neither backed up whole nor moved aside by a restore.
func TestASignalDirThatCantBeSignalCliIsRefused(t *testing.T) {
	user, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	mirrin := filepath.Join(t.TempDir(), "x", ".mirrin")
	for _, dir := range []string{"", "relative", string(filepath.Separator), user, cwd, filepath.Dir(mirrin), filepath.Dir(filepath.Dir(mirrin))} {
		if signalDirOK(dir, mirrin) {
			t.Errorf("%q passed", dir)
		}
	}
	if ok := filepath.Join(t.TempDir(), "signal-cli"); !signalDirOK(ok, mirrin) {
		t.Errorf("%q was refused", ok)
	}
	// Not backed up without signal-cli's own accounts.json.
	plain := t.TempDir()
	if items := signalItems(Layout{Home: mirrin, SignalDir: plain}); items != nil {
		t.Fatalf("a folder that isn't signal-cli's was backed up: %+v", items)
	}
	// A restore doesn't move the folder holding the twin aside.
	tarPath := filepath.Join(mirrin, filepath.FromSlash(nameSignal))
	if err := os.MkdirAll(filepath.Dir(tarPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tarPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(mirrin)
	if _, err := restoreSignal(mirrin, parent, time.Now()); err == nil || !exists(tarPath) {
		t.Fatalf("restore into the twin's own parent: %v", err)
	}
}

// Migrating into an iCloud Drive folder that holds the same snapshot, moved
// off the Mac, finds it the same rather than "different".
func TestMigrateIntoAnEvictedCopyIsTheSame(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	name := newName("snap", time.Now())
	body := []byte("ciphertext of the snapshot")
	if err := os.WriteFile(filepath.Join(from, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, placeholderOf(name)), []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	was, wasPoll := icloudDownload, icloudPoll
	t.Cleanup(func() { icloudDownload, icloudPoll = was, wasPoll })
	icloudPoll = 5 * time.Millisecond
	icloudDownload = func(_ context.Context, p string) error {
		if err := os.WriteFile(p, body, 0o600); err != nil {
			return err
		}
		return os.Remove(filepath.Join(to, placeholderOf(name)))
	}
	m, err := Migrate(context.Background(), Folder(from), Folder(to), nil)
	if err != nil || !slices.Equal(m.Same, []string{name}) {
		t.Fatalf("migrate: %+v %v", m, err)
	}
}
