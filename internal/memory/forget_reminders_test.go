package memory

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A reminder set from a fact carries the fact's words. Forgetting the fact
// takes the reminder and its link out of the live database before the WAL
// is checkpointed, and out of every backup and recorded copy, so restoring
// one doesn't bring the reminder back.
func TestForgetDropsTheFactsRemindersFromEveryCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gone := "Mum's birthday is on the 12th of November"
	id, err := s.Remember(ctx, "family", gone, "c")
	if err != nil {
		t.Fatal(err)
	}
	keep, err := s.Remember(ctx, "family", "Tony's anniversary is on the 3rd of May", "c")
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(48 * time.Hour)
	rGone, err := s.AddReminder(ctx, "c", due, gone+" (tomorrow)")
	if err != nil {
		t.Fatal(err)
	}
	rKeep, err := s.AddReminder(ctx, "c", due, "Tony's anniversary is on the 3rd of May (tomorrow)")
	if err != nil {
		t.Fatal(err)
	}
	links := fmt.Sprintf(`[{"fact":%d,"reminders":[%d],"day":"2026-11-12","yearly":true},{"fact":%d,"reminders":[%d],"day":"2027-05-03","yearly":true}]`, id, rGone, keep, rKeep)
	if err := s.Set(ctx, FactRemindersKey, links); err != nil {
		t.Fatal(err)
	}
	backup, err := s.Backup(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(t.TempDir(), "backups", "20261003-080000")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatal(err)
	}
	copyDB := filepath.Join(cdir, "memory.db")
	b, _ := os.ReadFile(backup)
	if err := os.WriteFile(copyDB, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteCopy(copyDB); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ForgetFact(ctx, id); err != nil {
		t.Fatal(err)
	}

	// The live file, while the store is still open: nothing left in the
	// main file or the WAL.
	files, _ := filepath.Glob(filepath.Join(dir, "memory.db*"))
	for _, p := range files {
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("12th of November")) {
			t.Errorf("%s still holds the forgotten reminder's words", filepath.Base(p))
		}
	}
	if v, _ := s.Get(ctx, FactRemindersKey); strings.Contains(v, fmt.Sprintf(`"fact":%d,`, id)) || !strings.Contains(v, `"yearly":true`) {
		t.Errorf("live links after the forget: %s", v)
	}
	if rs, _ := s.PendingReminders(ctx, "c"); len(rs) != 1 || rs[0].ID != rKeep {
		t.Errorf("only the other fact's reminder should remain: %+v", rs)
	}

	for _, p := range []string{backup, copyDB} {
		db, err := sql.Open("sqlite", fileURI(p))
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		rows, err := db.QueryContext(ctx, `SELECT id FROM reminders`)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for rows.Next() {
			var r int64
			_ = rows.Scan(&r)
			ids = append(ids, r)
		}
		rows.Close()
		var v string
		_ = db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, FactRemindersKey).Scan(&v)
		db.Close()
		if len(ids) != 1 || ids[0] != rKeep {
			t.Errorf("%s reminders after the forget: %v", filepath.Base(p), ids)
		}
		if strings.Contains(v, fmt.Sprintf(`"fact":%d,`, id)) || !strings.Contains(v, fmt.Sprintf(`"fact":%d,`, keep)) {
			t.Errorf("%s links after the forget: %s", filepath.Base(p), v)
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("12th of November")) {
			t.Errorf("%s still holds the forgotten reminder's words on disk", filepath.Base(p))
		}
	}
}

// With nothing else linked, the record itself goes.
func TestForgetUnsetsAnEmptiedReminderRecord(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id, _ := s.Remember(ctx, "family", "Mum's birthday is on the 12th", "c")
	rid, _ := s.AddReminder(ctx, "c", time.Now().Add(time.Hour), "Mum's birthday is on the 12th (tomorrow)")
	_ = s.Set(ctx, FactRemindersKey, fmt.Sprintf(`[{"fact":%d,"reminders":[%d],"day":"2026-11-12"}]`, id, rid))
	if _, err := s.ForgetFact(ctx, id); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get(ctx, FactRemindersKey); v != "" {
		t.Errorf("record should be gone, got %q", v)
	}
	if rs, _ := s.PendingReminders(ctx, "c"); len(rs) != 0 {
		t.Errorf("reminder should be gone: %+v", rs)
	}
}

// What a forget hook deletes (the daemon's own clean-up) is pushed out of
// the write-ahead log as well, not left in the main file.
func TestForgetHooksDeletionsLeaveNoTraceOnDisk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _ := s.Remember(ctx, "family", "Mum likes orchids", "c")
	rid, err := s.AddReminder(ctx, "c", time.Now().Add(time.Hour), "Buy the purple orchids for Mum")
	if err != nil {
		t.Fatal(err)
	}
	s.OnForgot(func() { _ = s.DropReminder(ctx, rid) })
	if _, err := s.ForgetFact(ctx, id); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "memory.db*"))
	for _, p := range files {
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("purple orchids")) {
			t.Errorf("%s still holds what the hook deleted", filepath.Base(p))
		}
	}
}
