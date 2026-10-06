package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Migration is one ordered change to memory.db's schema. Every change the
// database has ever had is a registered migration, so a twin upgrading from
// any earlier version is brought up to date in the same order every time,
// and schema_version records what was done.
//
// Other parts of the package (or other packages) add their own with Register
// from an init function; they don't need to touch this file:
//
//	func init() {
//		memory.Register(memory.Migration{Version: 10, Name: "reminders-snooze", Up: func(ctx context.Context, db memory.Execer) error {
//			return memory.AddColumn(ctx, db, "reminders", "snoozed", "INTEGER NOT NULL DEFAULT 0")
//		}})
//	}
type Migration struct {
	// Version orders migrations. Pick the next free number; two with the same
	// number run in Name order.
	Version int
	// Name identifies the migration in schema_version. Never rename one that
	// has shipped: it would run again.
	Name string
	// Up makes the change inside the migration transaction. It must be safe
	// on a database that already has it (databases made before schema_version
	// existed have every early change but no record of it), so use CREATE …
	// IF NOT EXISTS and AddColumn.
	Up func(ctx context.Context, db Execer) error
}

// Execer is what a migration runs its statements on.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
	migMu      sync.Mutex
	migrations []Migration
)

// Register adds a migration. It panics on a missing name, version or Up, and
// on a name registered twice, so a mistake shows in the first test run.
func Register(m Migration) {
	if m.Name == "" || m.Version < 1 || m.Up == nil {
		panic(fmt.Sprintf("memory: migration %q needs a name, a version above 0 and an Up", m.Name))
	}
	migMu.Lock()
	defer migMu.Unlock()
	for _, x := range migrations {
		if x.Name == m.Name {
			panic("memory: migration " + m.Name + " registered twice")
		}
	}
	migrations = append(migrations, m)
}

// registered returns the migrations in the order they run.
func registered() []Migration {
	migMu.Lock()
	out := append([]Migration(nil), migrations...)
	migMu.Unlock()
	sortMigrations(out)
	return out
}

// sortMigrations puts migrations in the order they run: by version, then name.
func sortMigrations(ms []Migration) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Version != ms[j].Version {
			return ms[i].Version < ms[j].Version
		}
		return ms[i].Name < ms[j].Name
	})
}

// AddColumn adds a column to a table unless it is already there.
func AddColumn(ctx context.Context, db Execer, table, column, decl string) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

// execAll runs statements in order.
func execAll(ctx context.Context, db Execer, stmts ...string) error {
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// The schema every database had before schema_version existed. These are
// the CREATE statements Open used to run each time, folded in unchanged.
func init() {
	Register(Migration{Version: 1, Name: "base-tables", Up: func(ctx context.Context, db Execer) error {
		return execAll(ctx, db,
			`CREATE TABLE IF NOT EXISTS facts (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				subject TEXT NOT NULL,
				content TEXT NOT NULL,
				source TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS facts_subject ON facts(subject)`,
			`CREATE TABLE IF NOT EXISTS messages (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				chat_key TEXT NOT NULL,
				role TEXT NOT NULL,
				blocks TEXT NOT NULL,
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS messages_chat ON messages(chat_key, id)`,
			`CREATE TABLE IF NOT EXISTS reminders (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				chat_key TEXT NOT NULL,
				due_at TEXT NOT NULL,
				text TEXT NOT NULL,
				fired INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS approvals (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				chat_key TEXT NOT NULL,
				tool TEXT NOT NULL,
				input TEXT NOT NULL,
				summary TEXT NOT NULL,
				status TEXT NOT NULL DEFAULT 'pending',
				created_at TEXT NOT NULL,
				resolved_at TEXT
			)`,
			`CREATE TABLE IF NOT EXISTS audit (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				ts TEXT NOT NULL,
				kind TEXT NOT NULL,
				chat_key TEXT NOT NULL DEFAULT '',
				detail TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS kv (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL
			)`,
		)
	}})
	// Reminders retry with backoff when a channel is down.
	Register(Migration{Version: 2, Name: "reminders-retry", Up: func(ctx context.Context, db Execer) error {
		if err := AddColumn(ctx, db, "reminders", "attempts", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		return AddColumn(ctx, db, "reminders", "retry_at", "TEXT NOT NULL DEFAULT ''")
	}})
	// Reminders can be ticked off or put back for later, and say when they
	// went out (so a "done" soon after is about them). kind and brief are
	// for reminders that check on something rather than only remind.
	Register(Migration{Version: 5, Name: "reminders-done-snooze", Up: func(ctx context.Context, db Execer) error {
		for _, c := range [][2]string{
			{"done_at", "TEXT"},
			{"snoozes", "INTEGER NOT NULL DEFAULT 0"},
			{"kind", "TEXT NOT NULL DEFAULT 'remind'"},
			{"brief", "TEXT NOT NULL DEFAULT ''"},
			{"fired_at", "TEXT NOT NULL DEFAULT ''"},
		} {
			if err := AddColumn(ctx, db, "reminders", c[0], c[1]); err != nil {
				return err
			}
		}
		return nil
	}})
}

// prepare checks memory.db and brings its schema up to date. Two processes
// opening a brand-new memory at the same moment can find it locked while the
// first switches it to WAL, a moment SQLite doesn't wait out by itself; that
// is retried rather than reported.
func (s *Store) prepare(ctx context.Context) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
		}
		if err = s.checkIntegrity(ctx); err == nil {
			if err = s.runMigrations(ctx); err != nil {
				if isCorrupt(err) {
					return s.damaged(trimDetail(err.Error()))
				}
				err = fmt.Errorf("migrate: %w", err)
			}
		}
		if err == nil || !isBusy(err) {
			return err
		}
	}
	return err
}

// isBusy reports whether an SQLite error means another connection had the
// database locked (SQLITE_BUSY, SQLITE_LOCKED).
func isBusy(err error) bool {
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case 5, 6:
			return true
		}
	}
	return strings.Contains(err.Error(), "database is locked")
}

const schemaTable = `CREATE TABLE IF NOT EXISTS schema_version (
	name TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	applied_at TEXT NOT NULL
)`

// appliedMigrations lists the migrations memory.db has had. A database made
// before schema_version existed has none recorded.
func appliedMigrations(ctx context.Context, db Execer) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_version`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// runMigrations brings memory.db up to date. Checking costs one read; when
// something is pending, the write lock is taken first (BEGIN IMMEDIATE), so a
// menu bar app and a terminal command starting together wait their turn
// instead of failing, and the second finds nothing left to do. All pending
// migrations commit together or not at all.
func (s *Store) runMigrations(ctx context.Context) error { return s.applyMigrations(ctx, registered()) }

func (s *Store) applyMigrations(ctx context.Context, all []Migration) error {
	applied, err := appliedMigrations(ctx, s.db)
	if err != nil {
		return err
	}
	if !pendingIn(all, applied) {
		return nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, schemaTable); err != nil {
		return err
	}
	if applied, err = appliedMigrations(ctx, conn); err != nil {
		return err
	}
	for _, m := range all {
		if applied[m.Name] {
			continue
		}
		if err := m.Up(ctx, conn); err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_version(name, version, applied_at) VALUES(?,?,?)`, m.Name, m.Version, now()); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	done = true
	return nil
}

func pendingIn(all []Migration, applied map[string]bool) bool {
	for _, m := range all {
		if !applied[m.Name] {
			return true
		}
	}
	return false
}

// SchemaVersion is the highest migration version memory.db has had.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// ---- Integrity --------------------------------------------------------------

// DamagedError is returned by Open when memory.db fails SQLite's integrity
// check. Nothing has been changed; its message says how to recover.
type DamagedError struct {
	Path   string
	Detail string // what SQLite reported
	// Backup is the newest backup copy ("" if there is none), taken at BackupAt.
	Backup   string
	BackupAt time.Time
	Backups  int
}

func (e *DamagedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "your twin's memory file is damaged, so it stopped before changing anything.\n  File: %s", e.Path)
	if e.Detail != "" {
		fmt.Fprintf(&b, " (%s)", e.Detail)
	}
	if e.Backup != "" {
		fmt.Fprintf(&b, "\nTo go back to the backup from %s, run:\n  mirrin memory restore", e.BackupAt.Local().Format("Mon 2 Jan 15:04"))
	} else {
		b.WriteString("\nThere's no backup to go back to yet. To start again with an empty memory (your settings, persona and protocols aren't in this file and stay as they are), run:\n  mirrin memory restore --fresh")
	}
	b.WriteString("\nThe damaged file is kept beside the new one, in case you need something from it.")
	return b.String()
}

// IsDamaged reports whether err is (or wraps) a DamagedError.
func IsDamaged(err error) bool {
	var d *DamagedError
	return errors.As(err, &d)
}

// isCorrupt reports whether an SQLite error means the file itself is bad
// (SQLITE_CORRUPT or SQLITE_NOTADB), as opposed to busy, locked or I/O.
func isCorrupt(err error) bool {
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case 11, 26: // SQLITE_CORRUPT, SQLITE_NOTADB
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "malformed") || strings.Contains(msg, "file is not a database")
}

// quickCheck runs PRAGMA quick_check and returns "" when the file is sound,
// else what SQLite found (at most a few lines). A busy or unreadable file is
// an error, not damage.
func quickCheck(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check(5)`)
	if err != nil {
		if isCorrupt(err) {
			return trimDetail(err.Error()), nil
		}
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			if isCorrupt(err) {
				return trimDetail(err.Error()), nil
			}
			return "", err
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		if isCorrupt(err) {
			return trimDetail(err.Error()), nil
		}
		return "", err
	}
	if len(lines) == 1 && lines[0] == "ok" {
		return "", nil
	}
	if len(lines) == 0 {
		return "the integrity check returned nothing", nil
	}
	return trimDetail(strings.Join(lines, "; ")), nil
}

func trimDetail(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// damaged describes memory.db as damaged, with the newest backup to go back to.
func (s *Store) damaged(detail string) *DamagedError {
	e := &DamagedError{Path: filepath.Join(s.dir, "memory.db"), Detail: detail}
	if all, _ := s.Backups(); len(all) > 0 {
		e.Backups, e.Backup, e.BackupAt = len(all), all[0], backupTime(all[0])
	}
	return e
}

// checkIntegrity is the boot-time check: quick_check reads every page, so a
// damaged file is found here, with a way back, rather than as a confusing
// failure in the middle of a conversation.
func (s *Store) checkIntegrity(ctx context.Context) error {
	detail, err := quickCheck(ctx, s.db)
	if err != nil {
		return err
	}
	if detail != "" {
		return s.damaged(detail)
	}
	return nil
}

// backupTime reads when a backup was taken from its name.
func backupTime(path string) time.Time {
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "memory-"), ".db")
	t, _ := time.Parse(backupStamp, name)
	return t
}

// ---- Restore ----------------------------------------------------------------

// RestoreOptions says what Restore puts in place of memory.db.
type RestoreOptions struct {
	// From is the backup to use; "" means the newest one that passes its check.
	From string
	// Fresh starts an empty memory instead of using a backup.
	Fresh bool
	// Anyway restores even when the current file is sound (everything since
	// the backup is then set aside).
	Anyway bool
}

// RestoreResult says what Restore did.
type RestoreResult struct {
	From   string    // the backup put in place ("" for a fresh start)
	FromAt time.Time // when that backup was taken
	Kept   string    // where the previous memory.db was moved
}

// ErrNotDamaged is returned by Restore when memory.db is sound and
// RestoreOptions.Anyway isn't set.
var ErrNotDamaged = errors.New("your memory file is fine, so nothing was changed. Going back to a backup would set aside everything since it was taken; to do that anyway, run: mirrin memory restore --anyway")

// Restore replaces memory.db in dir with a backup (or an empty memory). The
// previous file and its journal are kept as memory.db.damaged-<time>;
// nothing is deleted. The twin must not be running, but one that starts
// part-way through (the service manager restarts a twin whose memory is
// damaged every few seconds) finds either the old file or the restored one,
// never an empty space to make a new memory in: the replacement is made and
// checked beside memory.db first, then swapped in with a single rename.
func Restore(dir string, opts RestoreOptions) (RestoreResult, error) {
	ctx := context.Background()
	var res RestoreResult
	path := filepath.Join(dir, "memory.db")
	_, statErr := os.Stat(path)
	exists := statErr == nil
	if exists && !opts.Anyway {
		detail, err := checkFile(ctx, path)
		if err != nil {
			return res, fmt.Errorf("couldn't check %s: %w", path, err)
		}
		if detail == "" {
			return res, ErrNotDamaged
		}
	}

	// What this machine keeps (its pause), read before anything moves.
	carried := MachineState(path)

	// The replacement, made and checked before anything else is touched.
	next := path + ".restoring"
	removeDB(next)
	if opts.Fresh {
		if err := freshFile(dir, next); err != nil {
			removeDB(next)
			return res, fmt.Errorf("couldn't make an empty memory, so nothing was changed: %w", err)
		}
	} else {
		candidates := []string{opts.From}
		if opts.From == "" {
			candidates, _ = (&Store{dir: dir}).Backups()
		}
		for _, c := range candidates {
			if detail, err := checkCopy(ctx, c); err != nil || detail != "" {
				continue
			}
			if err := copyFile(c, next); err != nil {
				removeDB(next)
				return res, fmt.Errorf("couldn't copy the backup, so nothing was changed: %w", err)
			}
			// The copy itself is checked: a disk that damaged memory.db can
			// damage a copy too.
			if detail, err := checkCopy(ctx, next); err != nil || detail != "" {
				removeDB(next)
				continue
			}
			res.From, res.FromAt = c, backupTime(c)
			break
		}
		if res.From == "" {
			if opts.From != "" {
				return res, fmt.Errorf("the backup %s can't be read, so nothing was changed", opts.From)
			}
			return res, errors.New("there's no backup of your memory that can be read, so nothing was changed. To start again with an empty memory, run: mirrin memory restore --fresh")
		}
	}
	// This machine's pause stays as it is, and runs that fell due before
	// the restore were run by this twin already: none is made up again.
	if _, err := SettleRestored(next, carried, time.Now()); err != nil {
		removeDB(next)
		return res, fmt.Errorf("couldn't prepare the restored memory, so nothing was changed: %w", err)
	}
	restoreStep("ready")

	if exists {
		res.Kept = freeName(path + ".damaged-" + time.Now().Format("20060102-150405"))
		if err := swapIn(path, next, res.Kept); err != nil {
			removeDB(next)
			return RestoreResult{}, err
		}
	} else if err := os.Rename(next, path); err != nil {
		removeDB(next)
		return RestoreResult{}, fmt.Errorf("couldn't put the restored memory in place: %w", err)
	}
	syncDir(dir)

	s, err := Open(dir)
	if err != nil {
		return res, fmt.Errorf("the restored memory didn't open: %w", err)
	}
	return res, s.Close()
}

// restoreStep marks the steps of Restore; tests start a twin at each one.
var restoreStep = func(step string) {}

// linkFile gives a file a second name (replaced in tests).
var linkFile = os.Link

// journal is the files SQLite keeps beside a database while it is in use.
var journal = []string{"-wal", "-shm"}

// swapIn replaces path with next, keeping the old file (and its journal) as
// kept. path always names a whole database: the old file stays in place
// until one rename puts next over it.
func swapIn(path, next, kept string) error {
	// A second name for the old file, so it never has to leave path first.
	linked := linkFile(path, kept) == nil
	// Its journal goes with it: beside the restored file, SQLite would apply
	// it to the wrong database.
	var moved []string
	for _, j := range journal {
		if err := moveIfThere(path+j, kept+j); err != nil {
			for _, m := range moved {
				_ = os.Rename(kept+m, path+m)
			}
			if linked {
				_ = os.Remove(kept)
			}
			return fmt.Errorf("couldn't move the old memory file aside, so nothing was changed: %w", err)
		}
		moved = append(moved, j)
	}
	restoreStep("aside")
	if !linked {
		// No hard links here (some shared or removable drives): a moment
		// without memory.db can't be helped.
		if err := os.Rename(path, kept); err != nil {
			for _, m := range moved {
				_ = os.Rename(kept+m, path+m)
			}
			return fmt.Errorf("couldn't move the old memory file aside, so nothing was changed: %w", err)
		}
	}
	if err := os.Rename(next, path); err != nil {
		if linked {
			_ = os.Remove(kept)
		} else {
			_ = os.Rename(kept, path)
		}
		for _, m := range moved {
			_ = os.Rename(kept+m, path+m)
		}
		return fmt.Errorf("couldn't put the restored memory in place, so nothing was changed: %w", err)
	}
	// Anything that opened the old file in the meantime left a journal for
	// it; that goes aside too.
	for _, j := range journal {
		if _, err := os.Stat(path + j); err == nil {
			_ = os.Rename(path+j, freeName(kept+j))
		}
	}
	return nil
}

// moveIfThere renames from to to, if from exists.
func moveIfThere(from, to string) error {
	if _, err := os.Stat(from); err != nil {
		return nil
	}
	return os.Rename(from, to)
}

// freeName is path, or path.1, path.2, … if that is taken.
func freeName(path string) string {
	name := path
	for i := 1; ; i++ {
		if _, err := os.Lstat(name); os.IsNotExist(err) {
			return name
		}
		name = fmt.Sprintf("%s.%d", path, i)
	}
}

// removeDB removes a database file and its journal, if they exist.
func removeDB(path string) {
	_ = os.Remove(path)
	for _, j := range journal {
		_ = os.Remove(path + j)
	}
}

// freshFile makes an empty, fully set-up memory at dst.
func freshFile(dir, dst string) error {
	tmp, err := os.MkdirTemp(dir, ".fresh-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	s, err := Open(tmp)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		s.Close()
		return err
	}
	if err := s.Close(); err != nil {
		return err
	}
	return os.Rename(filepath.Join(tmp, "memory.db"), dst)
}

// Check runs SQLite's integrity check on memory.db in dir, read-only and
// without opening it as a store (no migrations, no writes). It returns ""
// when the file is sound, else what SQLite found; a missing file is an error.
func Check(dir string) (string, error) {
	return checkFile(context.Background(), filepath.Join(dir, "memory.db"))
}

// checkFile runs quick_check on a database file without changing it.
func checkFile(ctx context.Context, path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer db.Close()
	return quickCheck(ctx, db)
}

// checkCopy is checkFile for a copy no twin uses (a backup, the file about
// to be restored). A read-only look at one in WAL mode (a copy made by hand
// of a live memory.db) leaves an empty journal beside it, which is tidied
// away so it can't follow the file.
func checkCopy(ctx context.Context, path string) (string, error) {
	var made []string
	for _, j := range journal {
		if _, err := os.Lstat(path + j); os.IsNotExist(err) {
			made = append(made, path+j)
		}
	}
	detail, err := checkFile(ctx, path)
	if len(made) == len(journal) {
		if st, statErr := os.Stat(path + "-wal"); statErr != nil || st.Size() == 0 {
			for _, f := range made {
				_ = os.Remove(f)
			}
		}
	}
	return detail, err
}

// copyFile copies src to dst (0600) and makes sure it has reached the disk.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// syncDir makes a rename in dir durable, where the system allows it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// ---- Stats ------------------------------------------------------------------

// Stats describes memory.db without revealing anything in it: its size,
// schema version, how many rows each part holds and its backups.
type Stats struct {
	SizeBytes     int64
	SchemaVersion int
	Facts         int64
	Messages      int64
	Reminders     int64
	Approvals     int64
	Audit         int64
	Backups       int
	NewestBackup  time.Time
}

// Stats counts what memory.db holds.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	for _, name := range []string{"memory.db", "memory.db-wal"} {
		if fi, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			st.SizeBytes += fi.Size()
		}
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		return st, err
	}
	st.SchemaVersion = v
	for _, c := range []struct {
		table string
		n     *int64
	}{{"facts", &st.Facts}, {"messages", &st.Messages}, {"reminders", &st.Reminders}, {"approvals", &st.Approvals}, {"audit", &st.Audit}} {
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+c.table).Scan(c.n); err != nil {
			return st, err
		}
	}
	if all, _ := s.Backups(); len(all) > 0 {
		st.Backups, st.NewestBackup = len(all), backupTime(all[0])
	}
	return st, nil
}
