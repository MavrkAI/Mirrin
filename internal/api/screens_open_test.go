package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A presence screen open on this computer is counted while it listens, so
// a page handed over by voice opens one only when none is there; the orb,
// which listens too, isn't a screen.
func TestScreensOpenHereAreCounted(t *testing.T) {
	e := newEnv(t)
	listen := func(path string, l listener, remote string) (stop func()) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), listenerKey, l))
		r := httptest.NewRequest("GET", "http://127.0.0.1:7742"+path, nil).WithContext(ctx)
		r.RemoteAddr = remote
		r.Header.Set("Authorization", "Bearer "+master)
		w := &streamWriter{h: map[string][]string{}}
		done := make(chan struct{})
		go func() { defer close(done); e.s.Handler().ServeHTTP(w, r) }()
		deadline := time.Now().Add(3 * time.Second)
		for !strings.Contains(w.String(), "data: ") {
			if time.Now().After(deadline) {
				t.Fatalf("%s never listened: %s", path, w.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
		return func() { cancel(); <-done }
	}
	here := listener{kind: kindLoopback}
	orb := listen("/events?view=orb", here, "127.0.0.1:50001")
	if n := e.f.bus.ScreensOpen(); n != 0 {
		t.Fatalf("the orb counted as %d screens", n)
	}
	screen := listen("/events", here, "127.0.0.1:50002")
	if n := e.f.bus.ScreensOpen(); n != 1 {
		t.Fatalf("a screen here: %d open", n)
	}
	screen()
	orb()
	if n := e.f.bus.ScreensOpen(); n != 0 {
		t.Fatalf("after closing: %d open", n)
	}
}
