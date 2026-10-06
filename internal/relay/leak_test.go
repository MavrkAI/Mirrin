package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

// goroutines returns every goroutine but the caller's, by id. It does what
// go.uber.org/goleak does for this package without adding a dependency.
func goroutines() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	m := map[string]string{}
	for i, g := range strings.Split(string(buf), "\n\n") {
		if i == 0 {
			continue // the caller comes first
		}
		if f := strings.Fields(g); len(f) > 1 && f[0] == "goroutine" {
			m[f[1]] = g
		}
	}
	return m
}

// checkNoNewGoroutines waits up to 10 s for every goroutine not in base to
// exit, then fails listing the ones that remain.
func checkNoNewGoroutines(t *testing.T, base map[string]string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var left []string
		for id, g := range goroutines() {
			if _, ok := base[id]; !ok {
				left = append(left, g)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines outlived the listener:\n\n%s", len(left), strings.Join(left, "\n\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Acceptance: no goroutine leaks after ctx cancel. Work is left in every
// state first: a connection the server holds and never closes, filling
// r2's one place, with another queued behind it; one the server closed
// that the relay has not; one waiting for Accept; one half way through its
// PROXY header; and both control streams. Cancelling the context alone
// must end all of it, on the daemon's side and on the relays' side of each
// tunnel.
func TestNoGoroutineLeakAfterCancel(t *testing.T) {
	f1, f2 := newFakeRelay(t, "r1", welcome()), newFakeRelay(t, "r2", welcomeMax(1))
	base := goroutines()

	ctx, cancel := context.WithCancel(context.Background())
	l, err := Listen(ctx, config(f1, f2))
	if err != nil {
		t.Fatal(err)
	}
	s1, s2 := f1.session(t), f2.session(t)
	waitFor(t, 5*time.Second, "both tunnels", func() bool {
		st := l.Status()
		return st[0].Online && st[1].Online
	})

	held := s2.open(t, header(1, "r2"))
	defer held.Close()
	c, err := accept(l, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	readerDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, c) // and never Close
		readerDone <- err
	}()
	queued := s2.open(t, header(2, "r2")) // behind held, in yamux's backlog
	defer queued.Close()
	lingering := s1.open(t, header(3, "r1"))
	defer lingering.Close()
	c2, err := accept(l, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The relay never closes its half, so the daemon's side lingers.
	c2.Close()
	waiting := s1.open(t, header(4, "r1")) // never accepted
	defer waiting.Close()
	partial, err := s1.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()
	partial.Write([]byte("\r\n\r\n\x00")) // the first 5 bytes of a signature
	time.Sleep(100 * time.Millisecond)    // let each reach its blocking point

	cancel()
	select {
	case <-l.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("tunnels still running 10 s after cancel")
	}
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("a held connection outlived its tunnel")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after cancel: %v", err)
	}
	for _, st := range l.Status() {
		if st.Online {
			t.Fatalf("%s still online after cancel", st.Relay)
		}
	}
	checkNoNewGoroutines(t, base)
}
