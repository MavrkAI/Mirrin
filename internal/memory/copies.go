package memory

import (
	"bufio"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Other copies of the memory: the ones an import saves before replacing it
// (identity keeps them next to the other files it replaced, and tells the
// user where). They are listed in a file beside the daily backups, not in the
// database, so restoring a database never brings another machine's list
// along, and forgetting a fact scrubs them as it scrubs the backups.

// copiesFile lists the other copies, one path a line.
func (s *Store) copiesFile() string { return filepath.Join(s.BackupDir(), "copies.list") }

// NoteCopy records a copy of this memory kept elsewhere: a database snapshot
// named memory.db, or an exported memory.yaml (facts and portrait), inside a
// backups folder, or the memory.db of a twin a restore set aside
// (<home>.before-restore-<time>). Anything else is refused.
func (s *Store) NoteCopy(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if !copyPath(abs) {
		return os.ErrInvalid
	}
	s.bmu.Lock()
	defer s.bmu.Unlock()
	have := s.copies()
	if slices.Contains(have, abs) {
		return nil
	}
	if err := os.MkdirAll(s.BackupDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.copiesFile(), []byte(strings.Join(append(have, abs), "\n")+"\n"), 0o600)
}

// copyPath reports whether path can be a copy NoteCopy records. Scrubbing
// deletes a copy it can't clean, so only these are ever touched.
func copyPath(path string) bool {
	base := filepath.Base(path)
	if !filepath.IsAbs(path) || base != "memory.db" && base != "memory.yaml" {
		return false
	}
	dirs := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	return slices.Contains(dirs, "backups") ||
		base == "memory.db" && slices.ContainsFunc(dirs, func(d string) bool { return strings.Contains(d, ".before-restore-") })
}

// copies lists the recorded copies that are still there. The caller holds bmu.
func (s *Store) copies() []string {
	f, err := os.Open(s.copiesFile())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.TrimSpace(sc.Text())
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() && copyPath(p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// scrubCopies forgets f in every recorded copy, deleting one that can't be
// cleaned. A copy may come from another database (the memory an import
// replaced), where the fact has another number: it is found by its words.
// The caller holds bmu.
func (s *Store) scrubCopies(ctx context.Context, f Fact) {
	for _, p := range s.copies() {
		var err error
		if strings.HasSuffix(p, ".yaml") {
			err = scrubFactsFile(p, f)
		} else {
			err = scrubCopyDB(ctx, p, f)
		}
		if err != nil {
			_ = os.Remove(p)
		}
	}
}

func scrubCopyDB(ctx context.Context, path string, f Fact) error {
	db, err := sql.Open("sqlite", fileURI(path)+"?_pragma=busy_timeout(5000)&_pragma=secure_delete(1)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ids := []int64{f.ID}
	rows, err := db.QueryContext(ctx, `SELECT id FROM facts WHERE content=?`, f.Content)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		g := f
		g.ID = id
		if err := scrub(ctx, db, g); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, deletePortrait); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// factsCopy is the exported memory.yaml: the portrait and every fact.
type factsCopy struct {
	Portrait string `yaml:"portrait,omitempty"`
	Facts    []struct {
		Subject string `yaml:"subject"`
		Content string `yaml:"content"`
	} `yaml:"facts"`
}

// scrubFactsFile drops f from an exported memory.yaml, and the portrait with
// it, as a forget does in a database.
func scrubFactsFile(path string, f Fact) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc factsCopy
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	kept := doc.Facts[:0]
	for _, x := range doc.Facts {
		if strings.TrimSpace(x.Content) != strings.TrimSpace(f.Content) {
			kept = append(kept, x)
		}
	}
	doc.Facts = kept
	doc.Portrait = ""
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
