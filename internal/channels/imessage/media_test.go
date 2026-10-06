package imessage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestMediaPathFromDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(schema + `
 INSERT INTO message VALUES (1,'caption',NULL,1,0,0,0,NULL,1,0,NULL);
 INSERT INTO attachment (ROWID, mime_type, transfer_name, filename) VALUES (1,'image/png','photo.png','~/Library/Messages/Attachments/photo.png');
 INSERT INTO message_attachment_join VALUES (1,1);`); err != nil {
		t.Fatal(err)
	}
	rows, err := poll(context.Background(), db, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %+v", err, rows)
	}
	r := rows[0]
	if r.Filename != "~/Library/Messages/Attachments/photo.png" || r.attachment() == nil || r.attachment().Mime != "image/png" {
		t.Fatalf("%+v", r)
	}
	r.Audio = true
	r.Mime = "audio/x-caf"
	text, kind, ok := r.message()
	if !ok || text != "caption" || kind != channels.Voice || r.attachment() == nil {
		t.Fatalf("%+v", r)
	}
}

func TestMediaReadBoundaries(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "clip")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{file, "~/Library/Messages/Attachments/clip"} {
		b, err := readAttachment(context.Background(), root, name, 4)
		if err != nil || string(b) != "data" {
			t.Fatalf("%q %v", b, err)
		}
		if _, err := readAttachment(context.Background(), root, name, 3); !errors.Is(err, channels.ErrTooBig) {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("secret"), 0600)
	link := filepath.Join(root, "link")
	t.Run("symlink escape", func(t *testing.T) {
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := readAttachment(context.Background(), root, link, 10); err == nil {
			t.Fatal("accepted escaping symlink")
		}
	})
	for _, name := range []string{outside, filepath.Join(root, "missing"), root} {
		if _, err := readAttachment(context.Background(), root, name, 10); err == nil {
			t.Fatal(name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readAttachment(ctx, root, file, 4); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMediaConvertsNativeFormats(t *testing.T) {
	old := convertAttachment
	t.Cleanup(func() { convertAttachment = old })
	root := t.TempDir()
	for _, tt := range []struct{ mime, ext, want string }{
		{"audio/x-caf", ".caf", "audio/wav"},
		{"image/heic", ".heic", "image/jpeg"},
		{"image/heif", ".heif", "image/jpeg"},
		{"image/tiff", ".tiff", "image/jpeg"},
		{"", ".CAF", "audio/wav"},
	} {
		name := filepath.Join(root, "input"+tt.ext)
		if err := os.WriteFile(name, []byte("input"), 0600); err != nil {
			t.Fatal(err)
		}
		calls := 0
		convertAttachment = func(ctx context.Context, b []byte, target string, max int64) ([]byte, error) {
			calls++
			if string(b) != "input" || target != tt.want || max != 10 {
				t.Fatal(string(b), target, max)
			}
			return []byte("converted"), nil
		}
		a := (row{Attachments: true, Mime: tt.mime, Filename: name}).attachmentAt(root)
		if a.Mime != tt.want {
			t.Fatal(a.Mime)
		}
		b, err := a.Fetch(context.Background(), 10)
		if err != nil || string(b) != "converted" || calls != 1 {
			t.Fatal(string(b), err, calls)
		}
		if _, err := a.Fetch(context.Background(), 4); !errors.Is(err, channels.ErrTooBig) || calls != 1 {
			t.Fatal(err, calls)
		}
		convertAttachment = func(context.Context, []byte, string, int64) ([]byte, error) {
			return nil, errors.New("conversion failed")
		}
		if _, err := a.Fetch(context.Background(), 10); err == nil {
			t.Fatal("conversion failure lost")
		}
	}
}

func TestMediaWaitsForTransfer(t *testing.T) {
	root := t.TempDir()
	fetch := func(name string) chan error {
		a := (row{Attachments: true, Mime: "image/png", Filename: name}).attachmentAt(root)
		done := make(chan error, 1)
		go func() {
			b, err := a.Fetch(context.Background(), 4)
			if err == nil && string(b) != "data" {
				err = fmt.Errorf("got %q, want the whole download", b)
			}
			done <- err
		}()
		return done
	}

	// The file lands whole, after the row.
	name := filepath.Join(root, "late.png")
	done := fetch(name)
	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(name+".part", []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name+".part", name); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// The file is created first and filled in place: several polls see it
	// empty before its bytes arrive.
	name = filepath.Join(root, "inplace.png")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	done = fetch(name)
	time.Sleep(3 * attachmentPoll)
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	missing := (row{Attachments: true, Filename: filepath.Join(root, "missing")}).attachmentAt(root)
	if _, err := missing.Fetch(ctx, 4); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
