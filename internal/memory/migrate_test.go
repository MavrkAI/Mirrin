package memory

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFreshDatabaseRecordsEveryMigration(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	all := registered()
	if want := all[len(all)-1].Version; v != want {
		t.Fatalf("schema version %d, want %d", v, want)
	}
	applied, err := appliedMigrations(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if !applied[m.Name] {
			t.Errorf("%s not recorded", m.Name)
		}
	}
}

// A database made before schema_version existed (the layout every install
// had until now) keeps its rows and gets what it lacks.
func TestUpgradesADatabaseFromBeforeSchemaVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old, err := sql.Open("sqlite", fileURI(filepath.Join(dir, "memory.db")))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE facts (id INTEGER PRIMARY KEY AUTOINCREMENT, subject TEXT NOT NULL, content TEXT NOT NULL, source TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE reminders (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_key TEXT NOT NULL, due_at TEXT NOT NULL, text TEXT NOT NULL, fired INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL)`,
		`INSERT INTO facts(subject, content, created_at, updated_at) VALUES('user', 'Tony takes his coffee black.', '2025-01-01T00:00:00Z', '2025-01-01T00:00:00Z')`,
		`INSERT INTO reminders(chat_key, due_at, text, created_at) VALUES('telegram:1', '2030-01-01T00:00:00Z', 'call Priya', '2025-01-01T00:00:00Z')`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("an old database must open: %v", err)
	}
	defer s.Close()
	facts, _ := s.AllFacts(ctx, 10)
	if len(facts) != 1 || !strings.Contains(facts[0].Content, "black") {
		t.Fatalf("facts lost: %+v", facts)
	}
	rs, err := s.AllPendingReminders(ctx, 10) // reads the columns the upgrade added
	if err != nil || len(rs) != 1 || rs[0].Attempts != 0 || rs[0].Kind != "remind" || rs[0].Snoozes != 0 || !rs[0].FiredAt.IsZero() {
		t.Fatalf("reminders: %v %+v", err, rs)
	}
	if v, _ := s.SchemaVersion(ctx); v < 3 {
		t.Fatalf("schema version %d after upgrade", v)
	}
	if err := s.RecordUsage(ctx, "2026-09-27", "anthropic/claude-opus-5", "chat", tokens(10, 1)); err != nil {
		t.Fatalf("new table missing: %v", err)
	}
}

func TestRegisteredMigrationRunsOnceInOrder(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	var order []string
	var runs atomic.Int32
	list := append(registered(),
		Migration{Version: 1001, Name: "zz-second", Up: func(ctx context.Context, db Execer) error {
			order = append(order, "zz-second")
			return AddColumn(ctx, db, "approvals", "risk_test", "TEXT NOT NULL DEFAULT ''")
		}},
		Migration{Version: 1000, Name: "zz-first", Up: func(ctx context.Context, db Execer) error {
			runs.Add(1)
			order = append(order, "zz-first")
			_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS extra_test (id INTEGER PRIMARY KEY)`)
			return err
		}},
	)
	sortMigrations(list)
	for i := 0; i < 2; i++ {
		if err := s.applyMigrations(ctx, list); err != nil {
			t.Fatal(err)
		}
	}
	if runs.Load() != 1 || strings.Join(order, ",") != "zz-first,zz-second" {
		t.Fatalf("runs %d, order %v", runs.Load(), order)
	}
	if v, _ := s.SchemaVersion(ctx); v != 1001 {
		t.Fatalf("version %d", v)
	}
	if _, err := s.db.Exec(`INSERT INTO extra_test(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
}

func TestFailedMigrationChangesNothing(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	list := append(registered(),
		Migration{Version: 2000, Name: "adds-a-table", Up: func(ctx context.Context, db Execer) error {
			_, err := db.ExecContext(ctx, `CREATE TABLE half_done (id INTEGER)`)
			return err
		}},
		Migration{Version: 2001, Name: "fails", Up: func(ctx context.Context, db Execer) error {
			return errors.New("boom")
		}},
	)
	if err := s.applyMigrations(ctx, list); err == nil || !strings.Contains(err.Error(), "fails") {
		t.Fatalf("want the failing migration named, got %v", err)
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='half_done'`).Scan(&n)
	applied, _ := appliedMigrations(ctx, s.db)
	if n != 0 || applied["adds-a-table"] {
		t.Fatalf("a failed batch left changes behind (table %d, recorded %v)", n, applied["adds-a-table"])
	}
	// The store still works.
	if _, err := s.Remember(ctx, "user", "still here", "t"); err != nil {
		t.Fatal(err)
	}
}

// The menu bar app and a terminal command can open a new memory at the same
// moment; both must come up.
func TestConcurrentOpensBothMigrate(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := Open(dir)
			if err == nil {
				err = s.Close()
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// An upgrade started by two processes at once runs each migration once:
// the second waits for the first, then finds nothing left to do.
func TestConcurrentUpgradeRunsOnce(t *testing.T) {
	dir := t.TempDir()
	old, err := sql.Open("sqlite", fileURI(filepath.Join(dir, "memory.db"))+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE reminders (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_key TEXT NOT NULL, due_at TEXT NOT NULL, text TEXT NOT NULL, fired INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := Open(dir)
			if err == nil {
				err = s.Close()
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	s := openDir(t, dir)
	var n, distinct int
	_ = s.db.QueryRow(`SELECT count(*), count(DISTINCT name) FROM schema_version`).Scan(&n, &distinct)
	if n != len(registered()) || distinct != n {
		t.Fatalf("schema_version has %d rows (%d distinct), want %d", n, distinct, len(registered()))
	}
}

func openDir(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStatsCountWithoutReading(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_, _ = s.Remember(ctx, "user", "a", "t")
	_, _ = s.Remember(ctx, "user", "b", "t")
	s.Audit(ctx, "tool.ok", "c", "x")
	if _, err := s.Backup(ctx, 3); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Facts != 2 || st.Audit != 1 || st.Backups != 1 || st.NewestBackup.IsZero() || st.SizeBytes == 0 || st.SchemaVersion < 3 {
		t.Fatalf("stats %+v", st)
	}
}

func TestRegisterRefusesDuplicates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a second base-tables migration was accepted")
		}
	}()
	Register(Migration{Version: 1, Name: "base-tables", Up: func(context.Context, Execer) error { return nil }})
}

// corrupt overwrites the last pages of a database file (where the facts
// are), as a failing disk or a half-copied file would. Only a full read of
// the file finds that: opening it and checking the schema don't.
func corrupt(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	junk := []byte(strings.Repeat("\xde\xad\xbe\xef", 1024))
	for off := st.Size() - 3*4096; off < st.Size(); off += 4096 {
		if _, err := f.WriteAt(junk, off); err != nil {
			t.Fatal(err)
		}
	}
}

func filledStore(t *testing.T, dir string, facts int) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < facts; i++ {
		if _, err := s.Remember(ctx, "user", strings.Repeat("Tony likes his coffee black. ", 20), "t"); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestDamagedMemoryIsCaughtAtOpenWithAWayBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := filledStore(t, dir, 40)
	if _, err := s.Backup(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "user", "Added after the backup.", "t"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	corrupt(t, filepath.Join(dir, "memory.db"))

	_, err := Open(dir)
	var dmg *DamagedError
	if !errors.As(err, &dmg) || !IsDamaged(err) {
		t.Fatalf("want a DamagedError, got %v", err)
	}
	if dmg.Backup == "" || dmg.Backups != 1 || !strings.Contains(err.Error(), "mirrin memory restore") || strings.Contains(err.Error(), "--fresh") {
		t.Fatalf("the message must offer the backup: %v", err)
	}

	res, err := Restore(dir, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.From != dmg.Backup || res.Kept == "" {
		t.Fatalf("restore result %+v", res)
	}
	if _, err := os.Stat(res.Kept); err != nil {
		t.Fatalf("the damaged file must be kept: %v", err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatalf("restored memory doesn't open: %v", err)
	}
	defer s.Close()
	facts, _ := s.AllFacts(ctx, 100)
	if len(facts) != 40 {
		t.Fatalf("want the 40 facts from the backup, got %d", len(facts))
	}
}

func TestNotADatabaseWithoutBackupsOffersAFreshStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.db"), []byte(strings.Repeat("this is not sqlite ", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !IsDamaged(err) || !strings.Contains(err.Error(), "mirrin memory restore --fresh") {
		t.Fatalf("got %v", err)
	}
	if _, err := Restore(dir, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "--fresh") {
		t.Fatalf("no backup: want the fresh option named, got %v", err)
	}
	res, err := Restore(dir, RestoreOptions{Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(res.Kept); !strings.Contains(string(b), "not sqlite") {
		t.Fatal("the old file must be kept as it was")
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestRestoreLeavesASoundMemoryAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := filledStore(t, dir, 3)
	if _, err := s.Backup(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "user", "newer", "t"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Restore(dir, RestoreOptions{}); !errors.Is(err, ErrNotDamaged) {
		t.Fatalf("want ErrNotDamaged, got %v", err)
	}
	s, _ = Open(dir)
	facts, _ := s.AllFacts(ctx, 10)
	s.Close()
	if len(facts) != 4 {
		t.Fatalf("a refused restore changed memory: %d facts", len(facts))
	}
	res, err := Restore(dir, RestoreOptions{Anyway: true})
	if err != nil || res.Kept == "" {
		t.Fatalf("anyway: %v %+v", err, res)
	}
	s, _ = Open(dir)
	defer s.Close()
	if facts, _ = s.AllFacts(ctx, 10); len(facts) != 3 {
		t.Fatalf("want the backup's 3 facts, got %d", len(facts))
	}
}

// The service manager restarts a twin whose memory is damaged every few
// seconds. One that starts while a restore is under way must neither leave
// an empty memory in the backup's place nor a journal that SQLite would
// apply to the restored file. That holds for a damaged file and for a sound
// one set aside with --anyway (which a twin can open and write to), and on a
// drive without hard links.
func TestRestoreSurvivesATwinStartingHalfWay(t *testing.T) {
	for _, c := range []struct {
		name          string
		damaged, link bool
	}{
		{"damaged", true, true},
		{"anyway", false, true},
		{"damaged-no-links", true, false},
		{"anyway-no-links", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s := filledStore(t, dir, 25)
			if _, err := s.Backup(ctx, 7); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Remember(ctx, "user", "added after the backup", "t"); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if c.damaged {
				corrupt(t, filepath.Join(dir, "memory.db"))
			}
			if !c.link {
				prev := linkFile
				linkFile = func(string, string) error { return errors.New("no hard links on this drive") }
				t.Cleanup(func() { linkFile = prev })
			}

			var twins []*Store
			t.Cleanup(func() {
				for _, s := range twins {
					s.Close()
				}
			})
			steps := map[string]bool{}
			prev := restoreStep
			restoreStep = func(step string) {
				steps[step] = true
				// A twin restarting now opens memory.db and, if it can,
				// writes to it and keeps it open.
				if s, err := Open(dir); err == nil {
					twins = append(twins, s)
					_, _ = s.Remember(ctx, "user", "written by a twin that started mid-restore", "t")
				}
			}
			t.Cleanup(func() { restoreStep = prev })

			res, err := Restore(dir, RestoreOptions{Anyway: !c.damaged})
			if err != nil {
				t.Fatal(err)
			}
			if !steps["ready"] || !steps["aside"] {
				t.Fatalf("restore skipped a step: %v", steps)
			}
			for _, s := range twins {
				s.Close()
			}
			twins = nil
			s, err = Open(dir)
			if err != nil {
				t.Fatalf("restored memory doesn't open: %v", err)
			}
			defer s.Close()
			facts, err := s.AllFacts(ctx, 100)
			if err != nil || len(facts) != 25 {
				t.Fatalf("want the backup's 25 facts, got %d (%v)", len(facts), err)
			}
			if b, err := os.ReadFile(res.Kept); err != nil || len(b) == 0 {
				t.Fatalf("the old file must be kept: %v", err)
			}
			left, _ := filepath.Glob(filepath.Join(dir, "*restoring*"))
			fresh, _ := filepath.Glob(filepath.Join(dir, ".fresh-*"))
			if len(left)+len(fresh) > 0 {
				t.Fatalf("restore left files behind: %v %v", left, fresh)
			}
		})
	}
}

// A twin that opens the old file part-way through and then dies leaves a
// journal with changes for that file. It must not stay beside the restored
// one, where SQLite would replay it.
func TestRestoreSetsAsideAJournalLeftByACrashedTwin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.db")
	s := filledStore(t, dir, 25)
	if _, err := s.Backup(ctx, 7); err != nil {
		t.Fatal(err)
	}
	s.Close()
	prev := restoreStep
	restoreStep = func(step string) {
		if step != "aside" {
			return
		}
		db, err := sql.Open("sqlite", fileURI(path)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, q := range []string{`PRAGMA wal_autocheckpoint=0`, `DELETE FROM facts`, `INSERT INTO facts(subject, content, created_at, updated_at) VALUES('user','from the crashed twin','x','x')`} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		wal, err := os.ReadFile(path + "-wal")
		if err != nil || len(wal) == 0 {
			t.Fatalf("no journal written: %v", err)
		}
		db.Close()
		// As if it had died before it could close: its journal stays.
		if err := os.WriteFile(path+"-wal", wal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { restoreStep = prev })
	if _, err := Restore(dir, RestoreOptions{Anyway: true}); err != nil {
		t.Fatal(err)
	}
	s = openDir(t, dir)
	facts, err := s.AllFacts(ctx, 100)
	if err != nil || len(facts) != 25 {
		t.Fatalf("want the backup's 25 facts, got %d (%v)", len(facts), err)
	}
}

// A backup copied by hand from a live memory.db is in WAL mode; checking it
// read-only leaves journal files that SQLite doesn't clean up itself.
func TestRestoreFromAWALModeCopyLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := filledStore(t, dir, 5)
	s.Close()
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := copyFile(filepath.Join(dir, "memory.db"), copyPath); err != nil {
		t.Fatal(err)
	}
	corrupt(t, filepath.Join(dir, "memory.db"))
	if _, err := Restore(dir, RestoreOptions{From: copyPath}); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*restoring*")); len(left) > 0 {
		t.Fatalf("restore left files behind: %v", left)
	}
	if left, _ := filepath.Glob(copyPath + "-*"); len(left) > 0 {
		t.Fatalf("checking the copy left files beside it: %v", left)
	}
	s = openDir(t, dir)
	if facts, _ := s.AllFacts(ctx, 10); len(facts) != 5 {
		t.Fatalf("want 5 facts, got %d", len(facts))
	}
}

// Two restores in the same second each keep the file they replaced.
func TestRestoreNeverOverwritesAKeptFile(t *testing.T) {
	dir := t.TempDir()
	s := filledStore(t, dir, 2)
	if _, err := s.Backup(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	s.Close()
	first, err := Restore(dir, RestoreOptions{Anyway: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Restore(dir, RestoreOptions{Anyway: true, Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Kept == second.Kept {
		t.Fatalf("both kept as %s", first.Kept)
	}
	for _, k := range []string{first.Kept, second.Kept} {
		if detail, err := checkFile(context.Background(), k); err != nil || detail != "" {
			t.Fatalf("kept file %s: %v %q", k, err, detail)
		}
	}
}

// A fresh start is made and checked before the damaged file is touched.
func TestRestoreFreshLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.db"), []byte(strings.Repeat("this is not sqlite ", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(dir, RestoreOptions{Fresh: true}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		if n != "memory.db" && !strings.HasPrefix(n, "memory.db-") && !strings.HasPrefix(n, "memory.db.damaged-") {
			t.Errorf("left behind: %s", n)
		}
	}
	s := openDir(t, dir)
	if v, _ := s.SchemaVersion(context.Background()); v != registered()[len(registered())-1].Version {
		t.Fatalf("a fresh memory is fully set up, got schema version %d", v)
	}
}

// A memory restored from a daily copy keeps what the machine keeps for
// itself (memory/machine.go) as it is now: the owner's current pause, and a
// scheduler that owes nothing from before the restore (the twin ran those
// runs before the file went bad), rather than the copy's.
func TestRestoreKeepsTheMachinesOwnState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := filledStore(t, dir, 3)
	old := time.Now().Add(-20 * time.Hour).UTC().Format(time.RFC3339Nano)
	_ = s.Set(ctx, KeyAlive, old)
	if _, err := s.Backup(ctx, 7); err != nil {
		t.Fatal(err)
	}
	paused := time.Now().UTC().Format(time.RFC3339)
	_ = s.Set(ctx, KeyPaused, paused) // paused since the copy was taken
	s.Close()
	before := time.Now()
	if _, err := Restore(dir, RestoreOptions{Anyway: true}); err != nil {
		t.Fatal(err)
	}
	got := MachineState(filepath.Join(dir, "memory.db"))
	if got[KeyPaused] != paused {
		t.Fatalf("the owner's pause: %q", got[KeyPaused])
	}
	if at, err := time.Parse(time.RFC3339Nano, got[KeyAlive]); err != nil || at.Before(before.Add(-time.Second)) {
		t.Fatalf("the scheduler's last look is the copy's: %q", got[KeyAlive])
	}
	for _, j := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, "memory.db.restoring"+j)); err == nil {
			t.Fatalf("a journal was left beside the restored copy: %s", j)
		}
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if facts, _ := s.AllFacts(ctx, 10); len(facts) != 3 {
		t.Fatalf("want the copy's 3 facts, got %d", len(facts))
	}
}
