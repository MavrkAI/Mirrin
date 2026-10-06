package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the driver memory uses

	"github.com/MavrkAI/Mirrin/internal/memory"
)

// The memory database runs in WAL mode and the daemon may have it open, so it
// is never copied or replaced as a file. Export reads a consistent snapshot
// through SQLite (VACUUM INTO); import restores through SQLite in one
// transaction, which is safe next to other connections.

// openDB opens an existing database without creating it. A path with '#',
// '?' or '%' in it is escaped as memory does. Rows it replaces are zeroed on
// disk (secure_delete), as a forgotten fact is.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", memory.FileURI(path)+"?mode=rw&_pragma=busy_timeout(10000)&_pragma=secure_delete(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// snapshotDB writes a consistent copy of the database at src to dst, including
// anything still in the write-ahead log.
func snapshotDB(src, dst string) error {
	db, err := openDB(src)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return err
	}
	return os.Chmod(dst, 0o600)
}

// checkDB confirms path is an intact SQLite database.
func checkDB(path string) error {
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New(res)
	}
	return nil
}

// notCopied are tables an import never takes from the archive:
// schema_version is this memory's own record of the migrations run on it
// (an archive from a newer build would otherwise mark migrations this build
// lacks as done, and they would never run after an upgrade).
var notCopied = map[string]bool{"schema_version": true}

// restoreDB replaces the contents of the memory in dataDir with the database
// at staged (named memory.db), table by table, in one transaction. What the
// machine keeps rather than the twin (memory/machine.go: its pause, when its
// scheduler last looked) stays this machine's.
func restoreDB(ctx context.Context, dataDir, staged string) error {
	// Bring both to this version's schema first.
	for _, dir := range []string{filepath.Dir(staged), dataDir} {
		st, err := memory.Open(dir)
		if err != nil {
			return err
		}
		st.Close()
	}
	db, err := openDB(filepath.Join(dataDir, "memory.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	carried := memory.MachineState(filepath.Join(dataDir, "memory.db"))
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, staged); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `DETACH DATABASE src`) }()

	tables, err := tableNames(ctx, conn, "main")
	if err != nil {
		return err
	}
	theirs, err := tableNames(ctx, conn, "src")
	if err != nil {
		return err
	}
	type copyStmt struct{ table, cols string }
	var copies []copyStmt
	for _, t := range tables {
		if !contains(theirs, t) || notCopied[t] {
			continue
		}
		mine, err := columns(ctx, conn, "main", t)
		if err != nil {
			return err
		}
		src, err := columns(ctx, conn, "src", t)
		if err != nil {
			return err
		}
		var common []string
		for _, c := range mine {
			if contains(src, c) {
				common = append(common, quote(c))
			}
		}
		if len(common) > 0 {
			copies = append(copies, copyStmt{t, strings.Join(common, ", ")})
		}
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range copies {
		if _, err := tx.ExecContext(ctx, `DELETE FROM main.`+quote(c.table)); err != nil {
			return err
		}
		q := fmt.Sprintf(`INSERT INTO main.%s (%s) SELECT %s FROM src.%s`, quote(c.table), c.cols, c.cols, quote(c.table))
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", c.table, err)
		}
	}
	if err := memory.SettleKV(ctx, tx, carried, time.Now()); err != nil {
		return fmt.Errorf("kv: %w", err)
	}
	return tx.Commit()
}

func tableNames(ctx context.Context, conn *sql.Conn, schema string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM `+schema+`.sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func columns(ctx context.Context, conn *sql.Conn, schema, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info(?, ?)`, table, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func quote(ident string) string { return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"` }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
