package daemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLockHomeKeepsATwinOut(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	release, err := LockHome(ctx, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !HomeInUse(dir) {
		t.Fatal("a locked home must count as in use")
	}
	if _, err := LockHome(ctx, dir, 300*time.Millisecond); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("a second claim must wait, then give up: %v", err)
	}
	// A twin waiting to start gets in once the claim is given back.
	done := make(chan error, 1)
	go func() {
		r, err := LockHome(ctx, dir, 5*time.Second)
		if err == nil {
			r()
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	release()
	if err := <-done; err != nil {
		t.Fatalf("after release: %v", err)
	}
	if HomeInUse(dir) {
		t.Fatal("home still in use after release")
	}
}
