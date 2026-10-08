package pagetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// withBrowser is /screen when the twin has a browser of its own.
func withBrowser(m map[string]any) {
	m["connected"] = map[string]any{"calendar": false, "mail": false, "voice": false, "browser": true}
}

// onPracticeForm is the twin's browser on the practice form, at work or not.
func onPracticeForm(active bool) map[string]any {
	return map[string]any{"open": true, "url": "https://httpbin.org/forms/post", "title": "Practice form", "held": false, "active": active}
}

// Try this: with a browser, the first chip is a web chore on a public
// practice form that stops before sending. Tapped on this computer, it asks
// the whole chore, the page comes up to watch as the twin starts on it, and
// the chip isn't offered again on this device (after a reload too).
func TestTryThisWatchesAWebChoreOnce(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(withBrowser)))
	b := newBrowserFake(d, map[string]any{"open": false})
	var mu sync.Mutex
	var sent string
	release := make(chan struct{})
	d.handle("POST /message/stream", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m struct{ Text string }
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		sent = m.Text
		mu.Unlock()
		// the twin starts on the form while the reply is on its way
		b.set(onPracticeForm(true))
		d.push(map[string]any{"kind": "browser", "text": "active"})
		select {
		case <-release:
		case <-time.After(20 * time.Second):
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: done\ndata: {\"reply\":\"It's all filled in. Shall I send it?\"}\n\n")
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && !document.querySelector('#suggest').hidden`, "the suggestions")
	if got := eval[string](t, ctx, chips); got != "Fill in a practice form, and stop before sending | Remind me in 10 minutes to stretch | What can you do for me? | Remind me to…" {
		t.Fatalf("with a browser: %q", got)
	}
	run(t, ctx, chromedp.Click(`#suggest button`, chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("watch"), "the page, to watch")
	close(release)
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes('Shall I send it?')`, "the reply")
	mu.Lock()
	got := sent
	mu.Unlock()
	if !strings.Contains(got, "https://httpbin.org/forms/post") || !strings.Contains(got, "stop before submitting") {
		t.Fatalf("sent %q", got)
	}

	b.set(map[string]any{"open": false})
	run(t, ctx, chromedp.Reload())
	waitFor(t, ctx, loaded+` && !document.querySelector('#suggest').hidden`, "the suggestions again")
	if got := eval[string](t, ctx, chips); strings.Contains(got, "practice form") {
		t.Fatalf("tried once, and still offered: %q", got)
	}
}
