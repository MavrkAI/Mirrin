package pagetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The screen tells the twin whether it is in sight: with the name its feed
// gave it, as it connects and whenever it goes into the background or comes
// back. Without that, a background tab still counted as "in front of the
// owner", and a page handed over by voice never came up (screen_seen.go).
func TestTheScreenSaysWhetherItIsInSight(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	var mu sync.Mutex
	var seen []string
	var queries []string
	d.handle("GET /events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"kind\":\"state\",\"text\":\"idle\",\"screen\":\"s1\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	d.handle("POST /events/seen", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Screen  string
			Visible bool
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s %v", in.Screen, in.Visible))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return ""
		}
		return seen[len(seen)-1]
	}
	waitSeen := func(want string) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			if last() == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("the screen never said %q (said %q)", want, seen)
	}

	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitSeen("s1 true")
	mu.Lock()
	q := queries[0]
	mu.Unlock()
	if q != "" {
		t.Fatalf("a screen in front connected with %q", q)
	}
	// into the background, and back
	run(t, ctx, chromedp.Evaluate(`Object.defineProperty(document, 'hidden', {configurable: true, get: () => true}); document.dispatchEvent(new Event('visibilitychange'))`, nil))
	waitSeen("s1 false")
	run(t, ctx, chromedp.Evaluate(`Object.defineProperty(document, 'hidden', {configurable: true, get: () => false}); document.dispatchEvent(new Event('visibilitychange'))`, nil))
	waitSeen("s1 true")
}

// The floating character isn't a screen: it never reports itself in sight.
func TestTheOrbDoesNotSayItIsInSight(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"kind\":\"state\",\"text\":\"idle\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(380, 300), chromedp.Navigate(d.url("/ui?mode=orb")))
	d.waitCalled("GET /events", 1)
	run(t, ctx, chromedp.Evaluate(`document.dispatchEvent(new Event('visibilitychange'))`, nil))
	time.Sleep(300 * time.Millisecond)
	if n := d.count("POST /events/seen"); n != 0 {
		t.Fatalf("the orb reported itself %d times", n)
	}
}
