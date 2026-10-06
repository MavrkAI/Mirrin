package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Two programs editing config.yaml (the running twin's settings, `mirrin
// backup init`) each read the file, change their part and write it back;
// run together, the later write dropped the earlier change. Edits now take
// turns: the second starts from the file the first wrote.
func TestConfigEditsTakeTurns(t *testing.T) {
	home(t)
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	inFirst := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Edit(func() error {
			note("first begins")
			close(inFirst)
			time.Sleep(150 * time.Millisecond)
			note("first ends")
			return nil
		})
	}()
	<-inFirst
	_ = Edit(func() error { note("second"); return nil })
	<-done
	if len(order) != 3 || order[2] != "second" {
		t.Fatalf("edits overlapped: %q", order)
	}
	// A lock that can't be had (held elsewhere past the wait) never blocks
	// an edit for good.
	editWait = 100 * time.Millisecond
	t.Cleanup(func() { editWait = 10 * time.Second })
	release := make(chan struct{})
	held := make(chan struct{})
	go func() { _ = Edit(func() error { close(held); <-release; return nil }) }()
	<-held
	ran := false
	if err := Edit(func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("an edit waited forever or failed: %v %v", ran, err)
	}
	close(release)
}

// A config edited outside the home (a restore's staging copy, a test's own
// file) is locked beside that file: nothing appears in the home, where a
// stray lock file would stop a home from before the rename moving in.
func TestEditFileLocksBesideTheFile(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("MIRRIN_HOME", home)
	other := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := EditFile(filepath.Join(other, "config.yaml"), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("EditFile: %v %v", ran, err)
	}
	if _, err := os.Stat(filepath.Join(other, "config.yaml.lock")); err != nil {
		t.Fatalf("no lock beside the file: %v", err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("the home was made for an edit elsewhere: %v", err)
	}
}
