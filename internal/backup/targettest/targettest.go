// Package targettest is the conformance suite every backup.Target passes:
// the folder and iCloud Drive targets, S3-compatible buckets, and Mirrin
// Cloud. A target only ever holds ciphertext under names that carry a time,
// so the suite checks storage behaviour and nothing about contents.
package targettest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
)

// Options tune the suite.
type Options struct {
	// Large is the size of the large round trips, made once from a stream
	// and once from a file (default 12 MiB). Choose it above the target's
	// multipart threshold to exercise uploads in parts.
	Large int64
	// Huge also round-trips 200 MiB from a stream; it is skipped with
	// -short.
	Huge bool
}

// HugeSize is the size of the Huge round trip.
const HugeSize = 200 << 20

// Run runs the suite with the default options. newTarget returns an empty
// target for each subtest.
func Run(t *testing.T, newTarget func(t *testing.T) backup.Target) {
	RunWith(t, newTarget, Options{})
}

// RunWith runs the suite with o.
func RunWith(t *testing.T, newTarget func(t *testing.T) backup.Target, o Options) {
	if o.Large <= 0 {
		o.Large = 12 << 20
	}
	ctx := context.Background()

	t.Run("empty", func(t *testing.T) {
		tg := newTarget(t)
		objs, err := tg.List(ctx)
		if err != nil {
			t.Fatalf("List of an empty target: %v", err)
		}
		if len(objs) != 0 {
			t.Fatalf("an empty target lists %v", objs)
		}
		if _, err := tg.Get(ctx, Name("snap", 1)); !errors.Is(err, backup.ErrNotFound) {
			t.Fatalf("Get of a missing object = %v, want ErrNotFound", err)
		}
		if err := tg.Delete(ctx, Name("snap", 1)); err != nil {
			t.Fatalf("Delete of a missing object: %v", err)
		}
		if tg.String() == "" {
			t.Fatal("String is empty")
		}
	})

	t.Run("round trip, listed oldest first", func(t *testing.T) {
		tg := newTarget(t)
		want := map[string][]byte{}
		// Stored out of order; List orders them by the time in the name.
		for _, i := range []int{3, 1, 2} {
			for _, kind := range []string{"snap", "handover"} {
				n := Name(kind, i)
				body := []byte(strings.Repeat(n, i))
				Put(t, tg, n, body)
				want[n] = body
			}
		}
		objs, err := tg.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) != len(want) {
			t.Fatalf("listed %d objects, want %d: %v", len(objs), len(want), objs)
		}
		for i, o := range objs {
			if int64(len(want[o.Name])) != o.Size {
				t.Errorf("%s: listed size %d, stored %d", o.Name, o.Size, len(want[o.Name]))
			}
			if o.Modified.IsZero() {
				t.Errorf("%s: no modified time", o.Name)
			}
			if i > 0 && nameTime(objs[i-1].Name).After(nameTime(o.Name)) {
				t.Errorf("not oldest first: %s before %s", objs[i-1].Name, o.Name)
			}
		}
		for n, body := range want {
			if got := Get(t, tg, n); !bytes.Equal(got, body) {
				t.Errorf("%s: got %d bytes back, want %d", n, len(got), len(body))
			}
		}
	})

	t.Run("a name is never replaced", func(t *testing.T) {
		tg := newTarget(t)
		n := Name("snap", 1)
		Put(t, tg, n, []byte("first"))
		if err := tg.Put(ctx, n, strings.NewReader("second"), 6); err == nil {
			t.Fatal("a second Put of the same name succeeded")
		}
		if got := Get(t, tg, n); string(got) != "first" {
			t.Fatalf("the first object became %q", got)
		}
	})

	t.Run("only backup names", func(t *testing.T) {
		tg := newTarget(t)
		for _, n := range []string{"../escape.age", "notes.txt", "snap-x.age", "", "a/" + Name("snap", 1)} {
			if err := tg.Put(ctx, n, strings.NewReader("x"), 1); err == nil {
				t.Errorf("Put(%q) succeeded", n)
			}
			if _, err := tg.Get(ctx, n); err == nil {
				t.Errorf("Get(%q) succeeded", n)
			}
		}
		if objs, err := tg.List(ctx); err != nil || len(objs) != 0 {
			t.Fatalf("after refused names List = %v, %v", objs, err)
		}
	})

	t.Run("a short or long body is refused and nothing is kept", func(t *testing.T) {
		tg := newTarget(t)
		short, long := Name("snap", 1), Name("snap", 2)
		if err := tg.Put(ctx, short, strings.NewReader(strings.Repeat("s", 50)), 100); err == nil {
			t.Error("a body shorter than its size was stored")
		}
		if err := tg.Put(ctx, long, strings.NewReader(strings.Repeat("l", 100)), 50); err == nil {
			t.Error("a body longer than its size was stored")
		}
		for _, n := range []string{short, long} {
			if _, err := tg.Get(ctx, n); !errors.Is(err, backup.ErrNotFound) {
				t.Errorf("%s: Get = %v, want ErrNotFound", n, err)
			}
		}
		if objs, err := tg.List(ctx); err != nil || len(objs) != 0 {
			t.Fatalf("List = %v, %v; want nothing", objs, err)
		}
	})

	t.Run("a cancelled put keeps nothing", func(t *testing.T) {
		tg := newTarget(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		n := Name("snap", 1)
		if err := tg.Put(cctx, n, strings.NewReader("data"), 4); err == nil {
			t.Fatal("a cancelled Put succeeded")
		}
		if _, err := tg.Get(ctx, n); !errors.Is(err, backup.ErrNotFound) {
			t.Fatalf("Get = %v, want ErrNotFound", err)
		}
	})

	t.Run("empty object", func(t *testing.T) {
		tg := newTarget(t)
		n := Name("handover", 1)
		Put(t, tg, n, nil)
		if got := Get(t, tg, n); len(got) != 0 {
			t.Fatalf("got %d bytes back", len(got))
		}
	})

	t.Run("delete", func(t *testing.T) {
		tg := newTarget(t)
		keep, gone := Name("snap", 1), Name("snap", 2)
		Put(t, tg, keep, []byte("keep"))
		Put(t, tg, gone, []byte("gone"))
		if err := tg.Delete(ctx, gone); err != nil {
			t.Fatal(err)
		}
		if _, err := tg.Get(ctx, gone); !errors.Is(err, backup.ErrNotFound) {
			t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
		}
		objs, err := tg.List(ctx)
		if err != nil || len(objs) != 1 || objs[0].Name != keep {
			t.Fatalf("List after Delete = %v, %v", objs, err)
		}
		if err := tg.Delete(ctx, gone); err != nil {
			t.Fatalf("a second Delete: %v", err)
		}
	})

	t.Run("large, from a stream", func(t *testing.T) {
		RoundTrip(t, newTarget(t), Name("snap", 1), o.Large, false)
	})

	t.Run("large, from a file", func(t *testing.T) {
		RoundTrip(t, newTarget(t), Name("snap", 1), o.Large, true)
	})

	if o.Huge {
		t.Run("200 MiB", func(t *testing.T) {
			if testing.Short() {
				t.Skip("-short")
			}
			RoundTrip(t, newTarget(t), Name("snap", 1), HugeSize, false)
		})
	}
}

// Name is a valid object name of kind ("snap" or "handover") for day i of
// September 2026.
func Name(kind string, i int) string {
	at := time.Date(2026, 9, 1, 3, 30, 0, 0, time.UTC).AddDate(0, 0, i)
	return fmt.Sprintf("%s-%s-%08x.age", kind, at.Format("20060102T150405Z"), i)
}

func nameTime(name string) time.Time {
	parts := strings.Split(name, "-")
	if len(parts) < 3 {
		return time.Time{}
	}
	at, _ := time.Parse("20060102T150405Z", parts[1])
	return at
}

// Put stores body as name, failing the test on error.
func Put(t testing.TB, tg backup.Target, name string, body []byte) {
	t.Helper()
	if err := tg.Put(context.Background(), name, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
}

// Get reads name back, failing the test on error.
func Get(t testing.TB, tg backup.Target, name string) []byte {
	t.Helper()
	rc, err := tg.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("Get %s: %v", name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return b
}

// Stream is size bytes of seeded noise that is neither a file nor
// seekable, so a target has to take it as it comes.
func Stream(seed uint64, size int64) io.Reader {
	var s [32]byte
	s[0] = byte(seed)
	s[1] = byte(seed >> 8)
	return io.LimitReader(onlyReader{rand.NewChaCha8(s)}, size)
}

type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// RoundTrip stores size bytes of noise as name, from a file or a stream,
// reads them back and compares hashes, without holding them in memory.
func RoundTrip(t testing.TB, tg backup.Target, name string, size int64, fromFile bool) {
	t.Helper()
	ctx := context.Background()
	want := sha256.New()
	src := io.TeeReader(Stream(uint64(size), size), want)
	if fromFile {
		f, err := os.Create(filepath.Join(t.TempDir(), "snapshot"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := io.Copy(f, src); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		src = f
	}
	if err := tg.Put(ctx, name, src, size); err != nil {
		t.Fatalf("Put %d bytes: %v", size, err)
	}
	rc, err := tg.Get(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got := sha256.New()
	n, err := io.Copy(got, rc)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if n != size || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatalf("read back %d bytes that differ from the %d stored", n, size)
	}
	objs, err := tg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.Name == name && o.Size != size {
			t.Fatalf("listed size %d, want %d", o.Size, size)
		}
	}
}
