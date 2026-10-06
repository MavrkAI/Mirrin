package pagetest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// namedPage serves a page as the API does for a twin called Ada: with the
// name in its head (internal/api/pwa.go injectHead).
func namedPage(d *daemon, t *testing.T, path, file string) {
	html := strings.Replace(readPage(t, file), "</head>", `<meta name="mirrin-name" content="Ada"></head>`, 1)
	d.handle("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	})
}

// unpairedH is the API's 401 with where to pair, when there is a place.
func unpairedH(fix, pairURL string) http.HandlerFunc {
	m := map[string]string{"error": "not_paired", "message": "This device isn't paired with Ada.", "fix": fix}
	if pairURL != "" {
		m["pair_url"] = pairURL
	}
	b, _ := json.Marshal(m)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(b)
	}
}

// A fresh browser (nothing remembered) that isn't paired sees the twin's own
// name, never the default, and on this computer a link to pair.
func TestUnpairedGateUsesTheTwinsNameAndLinksToPairing(t *testing.T) {
	for _, p := range settingsPages {
		t.Run(strings.TrimPrefix(p.path, "/"), func(t *testing.T) {
			d := newDaemon(t)
			file := pageFiles[p.path]
			if file == "" {
				file = localPageFiles[p.path]
			}
			namedPage(d, t, p.path, file)
			for _, r := range p.reads {
				d.handle(r, unpairedH("Open it again from Ada's menu.", "/devices/add"))
			}
			ctx := tab(t)
			run(t, ctx, desktop(), chromedp.Navigate(d.url(p.path)))
			waitFor(t, ctx, visible("#gate")+` && `+visible("#gatePair"), "the not-paired notice with a pairing link")
			if text := eval[string](t, ctx, `document.body.innerText + ' ' + document.title`); strings.Contains(text, "Mirrin") || !strings.Contains(text, "Ada") {
				t.Fatalf("page reads %q", text)
			}
			if href := eval[string](t, ctx, `document.querySelector('#gatePair a').getAttribute('href')`); href != "/devices/add" {
				t.Fatalf("pair link goes to %q", href)
			}
		})
	}
	t.Run("another device", func(t *testing.T) {
		d := newDaemon(t)
		namedPage(d, t, "/memory", pageFiles["/memory"])
		for _, r := range settingsPages[1].reads {
			d.handle(r, unpairedH("On the computer Ada runs on, run `mirrin pair --screen` and open the link it shows on this device.", ""))
		}
		ctx := tab(t)
		run(t, ctx, desktop(), chromedp.Navigate(d.url("/memory")))
		waitFor(t, ctx, visible("#gate"), "the not-paired notice")
		if text := textOf(t, ctx, "#gate"); !strings.Contains(text, "mirrin pair --screen") || eval[bool](t, ctx, visible("#gatePair")) {
			t.Fatalf("notice reads %q, pair link shown %v", text, eval[bool](t, ctx, visible("#gatePair")))
		}
	})
	t.Run("screen", func(t *testing.T) {
		d := newDaemon(t)
		namedPage(d, t, "/ui", pageFiles["/ui"])
		h := unpairedH("Open this screen again from Ada's menu.", "/devices/add")
		d.handle("GET /screen", h)
		d.handle("GET /events", h)
		ctx := tab(t)
		run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, visible("#unpaired")+` && `+visible("#unpairedPair"), "the not-paired notice with a pairing link")
		if text := textOf(t, ctx, "#unpaired"); strings.Contains(text, "Mirrin") || !strings.Contains(text, "Ada") || !strings.Contains(text, "Pair this device") {
			t.Fatalf("notice reads %q", text)
		}
	})
}

// A twin renamed since the daemon started still has the old name in the
// page's head; a browser that remembers the new name shows that one.
func TestRememberedNameWinsOverAStaleHeadName(t *testing.T) {
	for _, p := range []string{"/memory", "/ui"} {
		t.Run(strings.TrimPrefix(p, "/"), func(t *testing.T) {
			d := newDaemon(t)
			namedPage(d, t, p, pageFiles[p])
			h := unpairedH("Open it again from the menu.", "")
			if p == "/ui" {
				d.handle("GET /screen", h)
				d.handle("GET /events", h)
			} else {
				for _, r := range settingsPages[1].reads {
					d.handle(r, h)
				}
			}
			ctx := tab(t)
			run(t, ctx, desktop(), chromedp.Navigate(d.url(p)))
			run(t, ctx, chromedp.Evaluate(`localStorage.setItem('antbot.name','Grace')`, nil), chromedp.Reload())
			if p == "/ui" {
				waitFor(t, ctx, visible("#unpaired"), "the not-paired notice")
			} else {
				waitFor(t, ctx, visible("#gate"), "the not-paired notice")
			}
			if got := eval[string](t, ctx, `document.title + ' ' + (document.querySelector('#unpaired,#gate')||{}).textContent`); !strings.Contains(got, "Grace") || strings.Contains(got, "Ada") {
				t.Fatalf("page reads %q, want the remembered name", got)
			}
		})
	}
}
