package pagetest

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// A task's question can lead to the page it's waiting on, and can be
// dropped (two presses: a dropped task doesn't come back).
func TestAQuestionLeadsToTheBrowserAndCanBeDropped(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["tasks"] = []any{map[string]any{"id": "10011", "title": "Book Bali flights", "status": "waiting_user", "owner": "voice:local",
			"question": "Clear the not-a-robot check on the screen, then say done.", "asked_at": time.Now().Format(time.RFC3339), "steps": []any{}}}
	})))
	d.handle("GET /browser/state", jsonH(map[string]any{"open": true, "url": "https://booking.jetstar.com/", "title": "Challenge Validation", "held": false}))
	d.handle("GET /browser/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	var mu sync.Mutex
	var dropped []string
	d.handle("POST /tasks/10011/cancel", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		dropped = append(dropped, "10011")
		mu.Unlock()
		jsonH(map[string]bool{"cancelled": true})(w, r)
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!!document.querySelector('#needs .qbrowser') && !!document.querySelector('#needs .qdrop')`, "the card's actions")
	run(t, ctx, chromedp.Click("#needs .qbrowser", chromedp.ByQuery))
	waitFor(t, ctx, `!document.querySelector('#bLayer').hidden`, "the browser opened")
	run(t, ctx, chromedp.Click("#bsClose", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#bLayer').hidden`, "the browser closed")
	run(t, ctx, chromedp.Click("#needs .qdrop", chromedp.ByQuery))
	if got := textOf(t, ctx, "#needs .qdrop"); got != "Drop it?" {
		t.Fatalf("first press: %q", got)
	}
	mu.Lock()
	n := len(dropped)
	mu.Unlock()
	if n != 0 {
		t.Fatal("dropped on the first press")
	}
	run(t, ctx, chromedp.Click("#needs .qdrop", chromedp.ByQuery))
	for i := 0; i < 100; i++ {
		mu.Lock()
		n = len(dropped)
		mu.Unlock()
		if n > 0 {
			break
		}
		run(t, ctx, chromedp.Sleep(20*time.Millisecond))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dropped) != 1 || dropped[0] != "10011" {
		t.Fatalf("dropped %v", dropped)
	}
}

// A dropped task leaves the board; a finished one stays an hour, then
// moves into Done today (ui_states_test.go), and an older one goes.
func TestWorkingOnShowsWhatIsStillGoing(t *testing.T) {
	d := newDaemon(t)
	now := time.Now()
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["tasks"] = []any{
			map[string]any{"id": "a", "title": "Chase the refund", "status": "running", "updated": now.Format(time.RFC3339), "steps": []any{}},
			map[string]any{"id": "b", "title": "Bali flights early October", "status": "cancelled", "updated": now.Format(time.RFC3339), "steps": []any{}},
			map[string]any{"id": "c", "title": "Book Bali flights", "status": "done", "updated": now.Add(-20 * time.Minute).Format(time.RFC3339), "steps": []any{}},
			map[string]any{"id": "e", "title": "Last week's errand", "status": "done", "updated": now.Add(-72 * time.Hour).Format(time.RFC3339), "steps": []any{}},
		}
	})))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `document.querySelector('#tasks').textContent.includes('Chase the refund')`, "the board")
	got := textOf(t, ctx, "#tasks")
	for _, gone := range []string{"Bali flights early October", "Last week's errand"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q still on the board", gone)
		}
	}
	if !strings.Contains(got, "Book Bali flights") {
		t.Error("a task finished 20 minutes ago left the board")
	}
}
