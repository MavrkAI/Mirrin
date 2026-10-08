package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServiceLogsRotateWithOpenWriter(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	dir := t.TempDir()
	for _, name := range []string{"mirrin.err", "mirrin.out", "mirrin.err.log", "mirrin.out.log"} {
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			text := strings.Repeat(fmt.Sprint(i), 32)
			if _, err := f.WriteString(text); err != nil {
				t.Fatal(err)
			}
			if err := rotateServiceLogs(dir, 16); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path + ".1")
			if err != nil || string(data) != text[16:] {
				t.Fatalf("%q %v", data, err)
			}
			st, _ := os.Stat(path)
			if st.Size() != 0 {
				t.Fatal("open file was not truncated")
			}
		}
		f.Close()
		if _, err := os.Stat(path + ".3"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
			t.Fatal("too many archives")
		}
		st, _ := os.Stat(path + ".1")
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
			t.Fatal(st.Mode())
		}
	}
}

func TestServiceLogsLeaveSmallFilesAndSymlinksAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirrin.err")
	if err := os.WriteFile(path, []byte("recent"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dir, "mirrin.out")); err != nil {
		t.Skip(err)
	}
	if err := rotateServiceLogs(dir, 20); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "recent" {
		t.Fatal(string(b))
	}
	if err := rotateServiceLog(filepath.Join(dir, "mirrin.out"), 2); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != "recent" {
		t.Fatal("followed symlink")
	}
}

// Old units wrote with file:, which keeps the writer's offset after a
// truncate, so rotating them would corrupt the log.
func TestRotationWaitsForAnAppendUnit(t *testing.T) {
	dir := t.TempDir()
	unit := func(name, out, errOut string) string {
		p := filepath.Join(dir, name)
		body := "[Service]\nStandardOutput=" + out + "\nStandardError=" + errOut + "\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range []struct {
		name, goos, unit string
		ok               bool
	}{
		{"old file: unit", "linux", unit("old.service", "file:/x.out", "file:/x.err"), false},
		{"half updated", "linux", unit("half.service", "append:/x.out", "file:/x.err"), false},
		{"append: unit", "linux", unit("new.service", "append:/x.out", "append:/x.err"), true},
		{"no unit", "linux", filepath.Join(dir, "missing.service"), false},
		{"macOS", "darwin", "", true},
	} {
		if err := rotationAllowed(c.goos, c.unit); (err == nil) != c.ok {
			t.Errorf("%s: rotationAllowed = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
