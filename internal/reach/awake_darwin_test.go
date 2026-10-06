package reach

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakePower installs a pmset that prints whatever the test writes to a
// file, and a caffeinate that records its pid and sleeps.
func fakePower(t *testing.T) (setAC func(bool), pid func() int) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "power")
	pidFile := filepath.Join(dir, "caffeinate.pid")
	os.WriteFile(filepath.Join(dir, "pmset"), []byte("#!/bin/sh\ncat '"+state+"'\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "caffeinate"), []byte("#!/bin/sh\necho $$ > '"+pidFile+"'\nexec sleep 60\n"), 0o755)
	oldP, oldC, oldE := pmsetPath, caffeinatePath, awakeEvery
	pmsetPath, caffeinatePath, awakeEvery = filepath.Join(dir, "pmset"), filepath.Join(dir, "caffeinate"), 10*time.Millisecond
	t.Cleanup(func() { pmsetPath, caffeinatePath, awakeEvery = oldP, oldC, oldE })
	setAC = func(on bool) {
		src := "'Battery Power'"
		if on {
			src = "'AC Power'"
		}
		os.WriteFile(state, []byte("Now drawing from "+src+"\n -InternalBattery-0 (id=1)\t80%; charging\n"), 0o644)
	}
	pid = func() int {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	return setAC, pid
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func TestKeepAwakeCaffeinateOnlyOnAC(t *testing.T) {
	setAC, pid := fakePower(t)
	setAC(false)
	stop := KeepAwake(context.Background(), true)
	time.Sleep(100 * time.Millisecond)
	if pid() != 0 {
		t.Fatal("caffeinate started on battery")
	}
	setAC(true)
	waitFor(t, 5*time.Second, "caffeinate on AC", func() bool { return alive(pid()) })
	first := pid()
	// Unplugged: caffeinate stops.
	setAC(false)
	waitFor(t, 5*time.Second, "caffeinate to stop on battery", func() bool { return !alive(first) })
	// Plugged in again: a new one; stop() ends it.
	setAC(true)
	waitFor(t, 5*time.Second, "caffeinate again", func() bool { return pid() != first && alive(pid()) })
	second := pid()
	stop()
	if alive(second) {
		time.Sleep(200 * time.Millisecond)
		if alive(second) {
			t.Fatal("caffeinate outlived stop")
		}
	}
}

func TestKeepAwakeAlwaysWhenNotACOnly(t *testing.T) {
	setAC, pid := fakePower(t)
	setAC(false)
	ctx, cancel := context.WithCancel(context.Background())
	stop := KeepAwake(ctx, false)
	defer stop()
	waitFor(t, 5*time.Second, "caffeinate on battery when asked", func() bool { return alive(pid()) })
	p := pid()
	cancel() // the daemon exiting
	waitFor(t, 5*time.Second, "caffeinate to stop on exit", func() bool { return !alive(p) })
}
