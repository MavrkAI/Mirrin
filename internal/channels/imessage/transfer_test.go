package imessage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// An attachment is handed on when Messages says its download finished and
// the file holds all of it, not when its size merely stops changing.
func TestAttachmentWaitsForMessagesToFinishTheTransfer(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "chat.db")+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root := t.TempDir()
	name := filepath.Join(root, "photo.png")
	if _, err := db.Exec(schema + `
		INSERT INTO message VALUES (1,'',NULL,1,0,0,0,NULL,1,0,NULL);
		INSERT INTO attachment VALUES (1,'image/png','photo.png','` + name + `',3,4);
		INSERT INTO message_attachment_join VALUES (1,1);`); err != nil {
		t.Fatal(err)
	}
	rows, err := poll(context.Background(), db, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %+v", err, rows)
	}
	r := rows[0]
	if r.AttachmentID != 1 || r.Transfer != (transfer{State: 3, Total: 4}) {
		t.Fatalf("transfer not read: %+v", r)
	}

	// Half the file, still downloading.
	if err := os.WriteFile(name, []byte("da"), 0o600); err != nil {
		t.Fatal(err)
	}
	var complete atomic.Bool
	done := make(chan []byte, 1)
	go func() {
		b, err := r.attachmentAt(root).Fetch(context.Background(), 10)
		if err != nil {
			t.Error(err)
		}
		done <- b
	}()
	step := func(what string) {
		time.Sleep(4 * attachmentPoll)
		select {
		case b := <-done:
			t.Fatalf("handed on %q %s", b, what)
		default:
		}
	}
	step("while the transfer was in progress")
	// Marked finished while the file on disk is still short.
	if _, err := db.Exec(`UPDATE attachment SET transfer_state = 5 WHERE ROWID = 1`); err != nil {
		t.Fatal(err)
	}
	step("before the file held total_bytes")
	// The whole file, but back in progress: still not ready.
	if _, err := db.Exec(`UPDATE attachment SET transfer_state = 3 WHERE ROWID = 1`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	step("before Messages said the transfer finished")
	complete.Store(true)
	if _, err := db.Exec(`UPDATE attachment SET transfer_state = 5 WHERE ROWID = 1`); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-done:
		if string(b) != "data" || !complete.Load() {
			t.Fatalf("got %q", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never handed on")
	}
}

// A failed read of the transfer state (chat.db busy, say) once Messages has
// recorded one is "not ready yet", not a reason to fall back to the size
// check and hand on a half-downloaded file.
func TestAttachmentFailedStateReadIsNotReady(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "photo.png")
	if err := os.WriteFile(name, []byte("da"), 0o600); err != nil {
		t.Fatal(err)
	}
	var finished atomic.Bool
	state := func(context.Context) (transfer, error) {
		if finished.Load() {
			return transfer{State: transferFinished, Total: 2}, nil
		}
		return transfer{}, errors.New("database is locked (5) (SQLITE_BUSY)")
	}
	done := make(chan []byte, 1)
	go func() {
		b, _ := waitAttachment(context.Background(), root, name, 10, transfer{State: 3, Total: 4}, state)
		done <- b
	}()
	time.Sleep(6 * attachmentPoll)
	select {
	case b := <-done:
		t.Fatalf("handed on %q while the state could not be read", b)
	default:
	}
	finished.Store(true)
	select {
	case b := <-done:
		if string(b) != "da" {
			t.Fatalf("got %q", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never handed on")
	}
}
