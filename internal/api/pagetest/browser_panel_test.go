package pagetest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// a 1×1 JPEG, base64
const dotJPEG = "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBAQEBAgGBgUGCQgKCgkICQkKDA8MCgsOCwkJDRENDg8QEBEQCgwSExIQEw8QEBD/yQALCAABAAEBAREA/8wABgAQEAX/2gAIAQEAAD8A0s8g/9k="

// The panel shows the twin's page live; taking over sends clicks to it.
func TestBrowserPanelWatchesAndTakesOver(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	var mu sync.Mutex
	held := false
	var inputs []map[string]any
	d.handle("GET /browser/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		jsonH(map[string]any{"open": true, "url": "https://www.booking.com/hotel/bali", "title": "Villa Bali · Booking.com", "held": held})(w, r)
	})
	d.handle("GET /browser/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: frame\ndata: {\"d\":%q,\"w\":1280,\"h\":800}\n\n", dotJPEG)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	d.handle("POST /browser/control", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Hold bool }
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		held = in.Hold
		mu.Unlock()
		jsonH(map[string]any{"open": true, "held": in.Hold})(w, r)
	})
	d.handle("POST /browser/input", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		inputs = append(inputs, m)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!document.querySelector('#secBrowser').hidden && document.querySelector('#bThumbImg').src.startsWith('data:image/jpeg')`, "the live page")
	if got := textOf(t, ctx, "#bURL"); got != "www.booking.com" {
		t.Fatalf("site shown %q", got)
	}
	// Open brings the page up over the screen; Take over there makes it yours.
	run(t, ctx, chromedp.Click("#bOpen", chromedp.ByQuery))
	waitFor(t, ctx, `!document.querySelector('#bLayer').hidden && document.querySelector('#bSheet').dataset.mode === 'watch'`, "the sheet")
	run(t, ctx, chromedp.Click("#bTake", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#bSheet').dataset.mode === 'driving' && document.querySelector('#bTake').textContent === 'Hand back' && document.querySelector('#secBrowser').classList.contains('held')`, "taking over")
	// Driving it, the page is big enough to use.
	waitFor(t, ctx, `document.querySelector('#bSheet').contains(document.querySelector('#bImg')) && document.querySelector('#bImg').getBoundingClientRect().width > 1000`, "the page big")
	// A press held in the middle of the picture lands in the middle of the
	// page, pressed and released apart ("press and hold" checks).
	var box []float64
	run(t, ctx, chromedp.Evaluate(`(() => { const r = document.querySelector('#bImg').getBoundingClientRect(); return [r.left + r.width/2, r.top + r.height/2]; })()`, &box))
	run(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := input.DispatchMouseEvent(input.MousePressed, box[0], box[1]).WithButton(input.Left).WithClickCount(1).Do(ctx); err != nil {
			return err
		}
		time.Sleep(400 * time.Millisecond)
		return input.DispatchMouseEvent(input.MouseReleased, box[0], box[1]).WithButton(input.Left).WithClickCount(1).Do(ctx)
	}))
	for i := 0; i < 50; i++ {
		mu.Lock()
		n := len(inputs)
		mu.Unlock()
		if n >= 2 {
			break
		}
		run(t, ctx, chromedp.Sleep(20e6))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != 2 || inputs[0]["type"] != "down" || inputs[1]["type"] != "up" {
		t.Fatalf("inputs %v", inputs)
	}
	if x, y := inputs[0]["x"].(float64), inputs[0]["y"].(float64); x < 600 || x > 680 || y < 360 || y > 440 {
		t.Fatalf("press at %.0f,%.0f, want about 640,400", x, y)
	}
}

// A page the twin handed over leads the screen: what to do, above the page.
func TestBrowserPanelShowsAHandOver(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("GET /browser/state", jsonH(map[string]any{"open": true, "url": "https://www.jetstar.com/", "title": "Jetstar", "held": true, "handover": true, "ask": "Solve the check, then tap Search."}))
	d.handle("GET /browser/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: frame\ndata: {\"d\":%q,\"w\":1280,\"h\":800}\n\n", dotJPEG)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui#browser")))
	waitFor(t, ctx, `document.querySelector('#secBrowser').classList.contains('handover') && !document.querySelector('#bLayer').hidden && !document.querySelector('#bAsk').hidden`, "the hand-over")
	if got := textOf(t, ctx, "#bAsk"); got == "" || got[:34] != "Solve the check, then tap Search. " {
		t.Fatalf("ask shown %q", got)
	}
	// The screen stays as it was under the sheet, and the link is spent.
	if !eval[bool](t, ctx, `(() => { const s = document.querySelector('#secBrowser'); return !s.hidden && s.parentElement === document.querySelector('#panels') && getComputedStyle(s).position !== 'fixed'; })()`) {
		t.Fatal("the panel left the Now column")
	}
	waitFor(t, ctx, `location.hash === ''`, "the link's #browser cleared")
}
