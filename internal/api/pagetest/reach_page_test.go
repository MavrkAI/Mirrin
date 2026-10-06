package pagetest

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The Reach page: three equal cards, the paid handle last, and the
// security panel with Verify now.
func init() {
	settingsPages = append(settingsPages,
		settingsPage{path: "/reach", reads: []string{"GET /reach/info"},
			empty: map[string]any{"GET /reach/info": map[string]any{"mode": "off", "cards": []any{}}}, says: "Only on this computer"})
}

// reachInfo is a machine on Tailscale, with a relay not set up, and the
// paid handle available (or, before launch, coming).
func reachInfo(available bool) map[string]any {
	return map[string]any{"mode": "tailscale",
		// The backend's order is not the page's: the page lists Cloud last.
		"cards": []any{
			map[string]any{"kind": "cloud", "available": available},
			map[string]any{"kind": "tailscale", "using": true, "ready": true, "address": "https://mac.tail1234.ts.net"},
			map[string]any{"kind": "relay", "detail": "No relay of your own is set up.", "fix": "If you run mirrin-relay on a server of yours, run `mirrin reach use relay` on this computer."},
		},
		"security": map[string]any{"host": "mac.tail1234.ts.net", "served": "q1Wz3e1XgFz2wqk3qGSG7m1b2b6yx8gFf8W1Q2x4yJ0", "next": "Zp7fQv0l2kz9x1yH3m4n5b6v7c8x9z0a1s2d3f4g5h6",
			"caa": "Tailscale controls this name.", "caa_ok": false,
			"ct": map[string]any{"state": "ok", "detail": "No stray certificates", "checked": time.Now().Add(-time.Hour)}},
	}
}

func TestReachPageListsCloudLastAndVerifies(t *testing.T) {
	d := localDaemon(t, nil, nil)
	d.handle("POST /reach/verify", jsonH(map[string]any{"ok": true, "checks": []any{
		map[string]any{"name": "certificate", "ok": true, "detail": "the key is this computer's"},
		map[string]any{"name": "caa", "ok": true, "warn": true, "detail": "no ACME account yet"},
	}}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/reach")))
	waitFor(t, ctx, `document.querySelectorAll('#cards .card').length === 3`, "the three cards")
	kinds := eval[[]string](t, ctx, `[...document.querySelectorAll('#cards .card')].map(c => c.dataset.kind)`)
	if strings.Join(kinds, ",") != "tailscale,relay,cloud" {
		t.Fatalf("cards %v", kinds)
	}
	cloud := textOf(t, ctx, `#cards .card[data-kind=cloud]`)
	if !strings.Contains(cloud, "Mirrin Cloud") || !strings.Contains(cloud, "Coming") || strings.Contains(cloud, "Link and use") {
		t.Fatalf("before launch the card says it is coming: %q", cloud)
	}
	if text := textOf(t, ctx, `#cards .card[data-kind=relay]`); !strings.Contains(text, "mirrin reach use relay") || strings.Contains(text, "`") {
		t.Fatalf("relay card %q", text)
	}
	run(t, ctx, chromedp.Click("#verifyNow", chromedp.ByQuery))
	d.waitCalled("POST /reach/verify", 1)
	waitFor(t, ctx, `document.querySelector('#verifyMsg').textContent.includes('checks out') && document.querySelectorAll('#checks li').length === 2`, "the verify result")

	// Once it is available, one click: the handle asked for goes to the
	// daemon, and the checkout opens in a new tab.
	d.handle("GET /reach/info", jsonH(reachInfo(true)))
	var asked string
	d.handle("POST /reach/cloud", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 256)
		n, _ := r.Body.Read(b)
		asked = string(b[:n])
		jsonH(map[string]any{"checkout_url": "about:blank"})(w, r)
	})
	run(t, ctx, chromedp.Navigate(d.url("/reach")))
	waitFor(t, ctx, `!!document.querySelector('#useCloud')`, "the Cloud button")
	if !eval[bool](t, ctx, `!document.querySelector('#switchNote').hidden`) {
		t.Fatal("no warning that the address changes")
	}
	run(t, ctx, chromedp.SendKeys("#handle", "ember-otter-42", chromedp.ByQuery), chromedp.Click("#useCloud", chromedp.ByQuery))
	d.waitCalled("POST /reach/cloud", 1)
	waitFor(t, ctx, `document.querySelector('#cloudMsg').textContent.includes('Finish paying')`, "the checkout note")
	if !strings.Contains(asked, "ember-otter-42") {
		t.Fatalf("asked %q", asked)
	}
}
