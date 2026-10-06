package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A suspension runs abuse.notify_command with the handle and the network
// count as arguments, without a shell, and without blocking the relay; a
// failing command is logged.
func TestSuspendNotifier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test command is a shell script")
	}
	dir := t.TempDir()
	got := filepath.Join(dir, "got")
	cmd := filepath.Join(dir, "notify")
	script := "#!/bin/sh\nprintf '%s|%s|%s' \"$1\" \"$2\" \"$#\" > " + got + "\n"
	if err := os.WriteFile(cmd, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(lockedBuf{&mu, &logs}, nil))

	start := time.Now()
	suspendNotifier(cmd, log)("ember; rm -rf /", 201)
	if time.Since(start) > time.Second {
		t.Fatal("the hook blocked")
	}
	var b []byte
	for deadline := time.Now().Add(10 * time.Second); ; {
		b, _ = os.ReadFile(got)
		if len(b) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if string(b) != "ember; rm -rf /|201|2" {
		t.Fatalf("command got %q", b)
	}

	suspendNotifier(filepath.Join(dir, "missing"), log)("ember", 201)
	for deadline := time.Now().Add(10 * time.Second); ; {
		mu.Lock()
		out := logs.String()
		mu.Unlock()
		if strings.Contains(out, `msg="relay: abuse notify"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a failing command was not logged:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type lockedBuf struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (l lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
