package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/tools"
	"github.com/chromedp/chromedp"
)

// The live view: watching streams the page; after taking over, the owner's
// click lands on the page; the twin waits while the owner drives.
func TestLiveViewWatchAndTakeOver(t *testing.T) {
	needChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body style="margin:0"><button id="b" style="position:absolute;left:40px;top:40px;width:200px;height:80px" onclick="this.textContent='clicked'">press</button><div id="h" style="position:absolute;left:40px;top:200px;width:200px;height:80px" onmousedown="window.t0=Date.now()" onmouseup="this.textContent=Date.now()-window.t0>=300?'held':'short'">hold</div></body></html>`))
	}))
	defer srv.Close()
	s := newTestSession(t, "127.0.0.1")
	tab, err := s.tab(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(tab, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatal(err)
	}
	if st := s.Live(context.Background()); !st.Open || st.Held {
		t.Fatalf("state %+v", st)
	}

	// Watching streams frames of the page, with its size.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, err := s.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-frames:
		if f.Data == "" || f.W < 100 || f.H < 100 {
			t.Fatalf("frame %d bytes, %.0f×%.0f", len(f.Data), f.W, f.H)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no frame from the live view")
	}

	// The owner's input goes nowhere until they take over.
	if err := s.Input(context.Background(), InputEvent{Type: "click", X: 140, Y: 80}); !errors.Is(err, errNotHeld) {
		t.Fatalf("input before taking over: %v", err)
	}
	s.TakeOver(true)
	if err := s.Input(context.Background(), InputEvent{Type: "click", X: 140, Y: 80}); err != nil {
		t.Fatal(err)
	}
	var text string
	if err := chromedp.Run(tab, chromedp.Text("#b", &text, chromedp.ByQuery)); err != nil || text != "clicked" {
		t.Fatalf("the click didn't land: %q %v", text, err)
	}
	// A press can be held: down and up arrive apart.
	if err := s.Input(context.Background(), InputEvent{Type: "down", X: 140, Y: 240}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := s.Input(context.Background(), InputEvent{Type: "up", X: 140, Y: 240}); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(tab, chromedp.Text("#h", &text, chromedp.ByQuery)); err != nil || text != "held" {
		t.Fatalf("the hold didn't land: %q %v", text, err)
	}

	// The twin waits while the owner drives, and carries on once handed back.
	old := handBackWait
	handBackWait = 200 * time.Millisecond
	defer func() { handBackWait = old }()
	if _, err := s.tab(false); !errors.Is(err, errOwnerDriving) {
		t.Fatalf("twin's tab while the owner drives: %v", err)
	}
	// Told they're finished, the twin takes it back itself.
	for _, tl := range s.Tools() {
		if tl.Spec().Name == "browser_take_back" {
			if _, err := tl.Run(context.Background(), tools.Call{Input: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if st := s.Live(context.Background()); st.Held {
		t.Fatalf("after browser_take_back: %+v", st)
	}
	if _, err := s.tab(false); err != nil {
		t.Fatalf("twin's tab after handing back: %v", err)
	}

	// A browser that closes (idle, or shut) lets go of the owner's hold, so
	// the twin never waits on a hand-back for a page that's gone.
	s.TakeOver(true)
	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()
	if st := s.Live(context.Background()); st.Held || st.Open {
		t.Fatalf("after the browser closed: %+v", st)
	}
	if _, err := s.tab(false); err != nil {
		t.Fatalf("twin's tab after the browser closed: %v", err)
	}
}
