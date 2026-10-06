//go:build !windows

package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A Chrome start that hangs is killed, waited for and tried once more; the
// second start works and the hung process is gone before it begins. With
// someone at the screen that holds for a locked keychain too: the prompt
// shows there, and they may answer it.
func TestHungChromeStartIsKilledAndRetriedOnce(t *testing.T) {
	needChrome(t)
	for _, locked := range []bool{false, true} {
		t.Run(fmt.Sprintf("keychainLocked=%v", locked), func(t *testing.T) { hungStartRetried(t, locked) })
	}
}

func hungStartRetried(t *testing.T, locked bool) {
	real := findChrome()
	dir := t.TempDir()
	mark, pidFile := filepath.Join(dir, "started-once"), filepath.Join(dir, "hung.pid")
	fake := filepath.Join(dir, "chrome")
	script := fmt.Sprintf("#!/bin/sh\nif [ ! -f %q ]; then touch %q; echo $$ > %q; exec sleep 60; fi\nexec %q \"$@\"\n", mark, mark, pidFile, real)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldFind, oldTimeout, oldLocked, oldScreen, oldBefore := findChrome, launchTimeout, keychainLocked, someoneAtScreen, beforeRetry
	t.Cleanup(func() {
		findChrome, launchTimeout, keychainLocked, someoneAtScreen, beforeRetry = oldFind, oldTimeout, oldLocked, oldScreen, oldBefore
		noteLaunch(nil)
	})
	findChrome = func() string { return fake }
	launchTimeout = 3 * time.Second
	keychainLocked = func(context.Context) bool { return locked }
	someoneAtScreen = func() bool { return true }
	noteLaunch(nil)

	hungPID := func() int {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatalf("the first start never ran: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		return pid
	}
	retried := false
	beforeRetry = func() {
		retried = true
		if pid := hungPID(); syscall.Kill(pid, 0) != syscall.ESRCH {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("the hung Chrome (pid %d) was still running when the retry began", pid)
		}
	}

	s := newTestSession(t)
	if _, err := s.tab(false); err != nil {
		t.Fatalf("the second start should work: %v", err)
	}
	if !retried {
		t.Fatal("the hung start was not retried")
	}
	if pid := hungPID(); syscall.Kill(pid, 0) != syscall.ESRCH {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the hung Chrome (pid %d) was left running", pid)
	}
	if le, _ := lastLaunchFailure(); le != nil {
		t.Fatalf("a start that worked the second time is still marked failed: %v", le)
	}
}
