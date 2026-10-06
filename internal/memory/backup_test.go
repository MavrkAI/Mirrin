package memory

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestBackupRotatesAndRestores(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if _, err := s.Remember(ctx, "user", "Tony likes flat whites.", "t"); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := 0; i < 4; i++ {
		p, err := s.Backup(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
		time.Sleep(2 * time.Millisecond)
	}
	all, _ := s.Backups()
	if len(all) != 2 || all[0] != paths[3] || all[1] != paths[2] {
		t.Fatalf("want the newest two kept, newest first; got %v", all)
	}
	// A backup is a working database.
	db, err := sql.Open("sqlite", "file:"+all[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var content string
	if err := db.QueryRow(`SELECT content FROM facts`).Scan(&content); err != nil || content != "Tony likes flat whites." {
		t.Fatalf("backup unreadable: %v %q", err, content)
	}
}

func TestBackupIfDue(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	first, err := s.BackupIfDue(ctx, 24*time.Hour, 7)
	if err != nil || first == "" {
		t.Fatalf("first backup should be made: %q %v", first, err)
	}
	if again, err := s.BackupIfDue(ctx, 24*time.Hour, 7); err != nil || again != "" {
		t.Fatalf("a fresh backup exists, none is due: %q %v", again, err)
	}
	if later, err := s.BackupIfDue(ctx, time.Nanosecond, 7); err != nil || later == "" {
		t.Fatalf("an old backup should trigger a new one: %q %v", later, err)
	}
}
