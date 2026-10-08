package pagetest

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
)

// "Remember this page" on the browser sheet: one press keeps the page and
// says so under it; with nothing to keep it says that plainly; and it
// never sits beside an OK the twin needs.
func TestRememberThisPageFromTheSheet(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	newBrowserFake(d, onBooking(true))
	var gone atomic.Bool
	d.handle("POST /browser/remember", func(w http.ResponseWriter, r *http.Request) {
		if gone.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"no_page","message":"There's no web page open to remember.","fix":"Open the page first, then press Remember this page."}`))
			return
		}
		jsonH(map[string]any{"saved": true, "already": false, "message": "Saved. Ask Mirrin for it any time."})(w, r)
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#bChip"), "the chip")
	run(t, ctx, chromedp.Click("#bChip", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("watch")+` && `+visible("#bRemember")+` && document.querySelector('#bRemember').textContent === 'Remember this page'`, "the button, watching")
	run(t, ctx, chromedp.Click("#bRemember", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#bNote').textContent === 'Saved. Ask Mirrin for it any time.' && !document.querySelector('#bRemember').disabled`, "it says it's saved")
	if n := d.count("POST /browser/remember"); n != 1 {
		t.Fatalf("saved %d times", n)
	}
	if !eval[bool](t, ctx, sheetUp) {
		t.Fatal("saving put the sheet away")
	}

	gone.Store(true)
	run(t, ctx, chromedp.Click("#bRemember", chromedp.ByQuery))
	waitFor(t, ctx, `[...document.querySelectorAll('#toasts *')].some(e => e.textContent.includes("There's no web page open to remember."))`, "nothing to keep, said plainly")

	// beside an OK it needs, the sheet is about the OK
	if !eval[bool](t, ctx, `(() => { const s = document.querySelector('#bSheet'), m = s.dataset.mode; s.dataset.mode = 'approval'; const hidden = getComputedStyle(document.querySelector('#bRemember')).display === 'none'; s.dataset.mode = m; return hidden; })()`) {
		t.Fatal("Remember this page sits beside an OK")
	}
}
