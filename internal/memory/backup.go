package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupDir is where Backup keeps copies of memory.db.
func (s *Store) BackupDir() string { return filepath.Join(s.dir, "backups") }

// Backup writes a consistent copy of the live database into BackupDir while
// the twin keeps running (VACUUM INTO), then keeps only the newest keep
// copies. It checks the database first and refuses to back up a damaged one,
// so a bad file never pushes the good copies out.
func (s *Store) Backup(ctx context.Context, keep int) (string, error) {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	var check string
	if err := s.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return "", fmt.Errorf("check memory.db: %w", err)
	}
	if check != "ok" {
		return "", fmt.Errorf("memory.db failed its health check (%s), so the existing backups were kept", check)
	}
	dir := s.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "memory-"+time.Now().UTC().Format(backupStamp)+".db")
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("back up memory.db: %w", err)
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if keep < 1 {
		keep = 1
	}
	all, _ := s.Backups()
	for _, old := range all[min(keep, len(all)):] {
		_ = os.Remove(old)
	}
	return path, nil
}

// BackupIfDue makes a backup when the newest one is older than every (or
// there is none). It returns the new backup's path, or "" if none was due.
func (s *Store) BackupIfDue(ctx context.Context, every time.Duration, keep int) (string, error) {
	if all, _ := s.Backups(); len(all) > 0 {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(all[0]), "memory-"), ".db")
		if at, err := time.Parse(backupStamp, name); err == nil && time.Since(at) < every {
			return "", nil
		}
	}
	return s.Backup(ctx, keep)
}

// backupStamp names backups so they sort by age.
const backupStamp = "20060102-150405.000"

// Backups lists the backup files, newest first.
func (s *Store) Backups() ([]string, error) {
	entries, err := os.ReadDir(s.BackupDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, "memory-") && strings.HasSuffix(n, ".db") {
			out = append(out, filepath.Join(s.BackupDir(), n))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}
