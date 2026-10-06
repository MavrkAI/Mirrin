package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testName(kind string, i int) string {
	return fmt.Sprintf("%s-202609%02dT033000Z-%08x.age", kind, i, i)
}

func TestMigrateCopiesByteForByte(t *testing.T) {
	ctx := context.Background()
	from, to := t.TempDir(), t.TempDir()
	src, dst := Folder(from), Folder(to)
	want := map[string][]byte{}
	for i := 1; i <= 3; i++ {
		n := testName("snap", i)
		want[n] = bytes.Repeat([]byte{byte(i)}, 1000*i)
		if err := src.Put(ctx, n, bytes.NewReader(want[n]), int64(len(want[n]))); err != nil {
			t.Fatal(err)
		}
	}
	marker := testName("handover", 4)
	want[marker] = []byte("marker")
	src.Put(ctx, marker, bytes.NewReader(want[marker]), 6)
	// Something that isn't a backup is left behind.
	os.WriteFile(filepath.Join(from, "notes.txt"), []byte("x"), 0o600)

	var heard []string
	m, err := Migrate(ctx, src, dst, func(name string, _ int64) { heard = append(heard, name) })
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Copied) != 4 || len(heard) != 4 || m.Bytes != 6006 {
		t.Fatalf("migrated %+v, heard %v", m, heard)
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(to, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(to, "notes.txt")); err == nil {
		t.Fatal("a file that isn't a backup was copied")
	}
	// Again: nothing to copy, everything the same.
	if m, err := Migrate(ctx, src, dst, nil); err != nil || len(m.Copied) != 0 || len(m.Same) != 4 {
		t.Fatalf("again: %+v %v", m, err)
	}
	// A different object under the same name is never replaced.
	n := testName("snap", 5)
	src.Put(ctx, n, strings.NewReader("mine"), 4)
	dst.Put(ctx, n, strings.NewReader("else"), 4)
	if _, err := Migrate(ctx, src, dst, nil); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("a clash: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(to, n)); string(b) != "else" {
		t.Fatalf("the destination's object became %q", b)
	}
	if _, err := Migrate(ctx, src, Folder(from), nil); err == nil {
		t.Fatal("migrating a target onto itself")
	}
}

// closedTarget keeps what it has and takes nothing new, as the paid
// service does once payment lapses.
type closedTarget struct{ Target }

func (closedTarget) Put(context.Context, string, io.Reader, int64) error {
	return fmt.Errorf("%w: payment lapsed", ErrWritesClosed)
}

func (closedTarget) String() string { return "a closed place" }

// A target that takes no new backups gets the free places named.
func TestWritesClosedNamesTheFreePlaces(t *testing.T) {
	tw := newTwin(t)
	p := fixedPhrase(t)
	e := engineFor(t, tw, p, t.TempDir())
	e.Target = closedTarget{e.Target}
	_, err := e.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "payment lapsed") || !strings.Contains(err.Error(), "mirrin backup target folder") ||
		!strings.Contains(err.Error(), "mirrin backup migrate") {
		t.Fatalf("%v", err)
	}
}
