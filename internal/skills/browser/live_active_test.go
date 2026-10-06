package browser

import (
	"context"
	"testing"
	"time"
)

// The twin counts as using its browser while a browser tool runs and for a
// while after, so the screen can offer to watch (or open to watch) a run,
// whether it came from a chat here, a voice turn, a task or a routine. A
// new run is the first use after the browser has been idle; while the owner
// has the browser, the twin isn't using it.
func TestTheTwinIsActiveWhileItUsesTheBrowser(t *testing.T) {
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	h := &liveHub{now: func() time.Time { return at }}
	active := func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.activeAt(at) }
	if active() {
		t.Fatal("active before anything ran")
	}
	first, fresh := h.use("")
	if !fresh || !active() {
		t.Fatalf("the first use: fresh %v, active %v", fresh, active())
	}
	at = at.Add(10 * time.Second)
	second, fresh := h.use("")
	if fresh {
		t.Fatal("a second use 10s later started a new run")
	}
	first()
	first() // done twice counts once
	second()
	at = at.Add(39 * time.Second)
	if !active() {
		t.Fatal("not active 39s after the last call ended")
	}
	at = at.Add(2 * time.Second)
	if active() {
		t.Fatal("still active 41s after the last call ended")
	}
	again, fresh := h.use("")
	if !fresh {
		t.Fatal("a use after 41s idle isn't a new run")
	}
	again()
	h.take(true)
	if active() {
		t.Fatal("active while the owner has the browser")
	}
	h.take(false)
	if !active() {
		t.Fatal("not active once the owner handed it back")
	}
	// A task or routine (no chat) leaves the chat to carry on in as it was.
	h.mu.Lock()
	h.chat = "voice:local"
	h.mu.Unlock()
	done, _ := h.use("")
	done()
	if h.chat != "voice:local" {
		t.Fatalf("an empty key changed the chat to %q", h.chat)
	}
	done, _ = h.use("screen:local")
	done()
	if h.chat != "screen:local" {
		t.Fatalf("the chat is %q", h.chat)
	}
}

// Chrome still starting for a run: the screen already reads active, with no
// page open yet.
func TestTheScreenReadsActiveBeforeThePageOpens(t *testing.T) {
	s := &Session{}
	if st := s.Live(context.Background()); st.Active || st.Open {
		t.Fatalf("idle: %+v", st)
	}
	done, _ := s.live.use("")
	if st := s.Live(context.Background()); !st.Active || st.Open {
		t.Fatalf("running with no tab: %+v", st)
	}
	done()
}
