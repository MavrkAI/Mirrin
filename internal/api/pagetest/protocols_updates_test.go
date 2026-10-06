package pagetest

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
)

// The store shows each pack's commit, checks for updates without changing
// anything, applies one on request, and previews a pack before installing.
func TestProtocolsPinsUpdatesAndPreview(t *testing.T) {
	const from, to = "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pack := map[string]any{"dir": "news", "name": "news", "repo": "https://example.com/news", "installed": true, "commit": from}
	d := newDaemon(t)
	d.handle("GET /protocols/installed", jsonH(map[string]any{"protocols": []any{}, "packs": []any{pack}}))
	withUpdate := map[string]any{}
	for k, v := range pack {
		withUpdate[k] = v
	}
	withUpdate["update"] = map[string]any{"dir": "news", "from": from, "to": to, "pending": true, "changes": []string{"changed protocols/news.yaml"}}
	d.handle("POST /protocols/check-updates", jsonH(map[string]any{"packs": []any{withUpdate}}))
	var applied, installed atomic.Int32
	d.handle("POST /protocols/apply-update", func(w http.ResponseWriter, r *http.Request) {
		applied.Add(1)
		jsonH(map[string]string{"updated": "news"})(w, r)
	})
	d.handle("GET /protocols/search", jsonH([]any{map[string]any{"name": "travel", "description": "Trips"}}))
	d.handle("POST /protocols/preview", jsonH(map[string]any{"name": "travel", "protocols": []any{map[string]any{"name": "trip check", "schedule": "0 8 * * *", "requires": []string{"calendar"}, "prompt": "Look at tomorrow's trips."}}}))
	d.handle("POST /protocols/install", func(w http.ResponseWriter, r *http.Request) {
		installed.Add(1)
		jsonH(map[string]string{"installed": "travel"})(w, r)
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/protocols")))
	waitFor(t, ctx, `document.querySelector('#packs').textContent.includes('1111111')`, "the pack's commit")

	run(t, ctx, chromedp.Click("#checkUpdates", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#packs').textContent.includes('Update ready') && !!document.querySelector('#packs .apply')`, "the update")
	if applied.Load() != 0 {
		t.Fatal("checking applied the update")
	}
	run(t, ctx, chromedp.Click("#packs .apply", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#packs').textContent.includes('1111111')`, "the page reloaded")
	if applied.Load() != 1 {
		t.Fatalf("applied %d times", applied.Load())
	}

	run(t, ctx, chromedp.SetValue("#q", "travel", chromedp.ByQuery), chromedp.Click("#search button", chromedp.ByQuery))
	waitFor(t, ctx, `!!document.querySelector('#results .row button')`, "a result")
	run(t, ctx, chromedp.Click("#results .row button", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#results .preview') && !document.querySelector('#results .preview').hidden`, "the preview")
	text := textOf(t, ctx, "#results")
	for _, want := range []string{"trip check", "0 8 * * *", "calendar", "Look at tomorrow's trips.", "Nothing is installed yet"} {
		if !strings.Contains(text, want) {
			t.Errorf("preview lacks %q: %s", want, text)
		}
	}
	if installed.Load() != 0 {
		t.Fatal("the preview installed the pack")
	}
	run(t, ctx, chromedp.Click("#results .row button", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#results .row button').textContent === 'Installed'`, "installed")
	if installed.Load() != 1 {
		t.Fatalf("installed %d times", installed.Load())
	}
}
