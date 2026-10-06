package pagetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// The presence screen on this computer offers voice setup until voice is
// set up.
func TestScreenOffersVoiceSetup(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint("ready=", ready), func(t *testing.T) {
			d := newDaemon(t)
			d.handle("GET /screen", jsonH(screen(nil)))
			d.handle("GET /voice/setup/status", jsonH(map[string]bool{"ready": ready}))
			ctx := tab(t)
			run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
			waitFor(t, ctx, loaded, "the screen loaded")
			d.waitCalled("GET /voice/setup/status", 1)
			if ready {
				if eval[bool](t, ctx, visible("#setupVoice")) {
					t.Fatal("offered voice setup when voice is set up")
				}
				return
			}
			waitFor(t, ctx, visible("#setupVoice"), "the Set up voice button")
			if href := eval[string](t, ctx, `document.querySelector('#setupVoice').getAttribute('href')`); href != "/voice/setup" {
				t.Fatalf("it links %q", href)
			}
		})
	}
}

// The Set up voice page shows each step as the twin streams it, then how it
// all went.
func TestVoiceSetupPageShowsEachStep(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /voice/setup/status", jsonH(map[string]bool{"ready": false}))
	release := make(chan struct{})
	d.handle("POST /voice/setup", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(ev string, v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
			fl.Flush()
		}
		send("step", map[string]any{"label": "Hearing", "running": true})
		<-release
		send("step", map[string]any{"label": "Hearing", "ok": true, "notes": "whisper small.en (English)"})
		send("step", map[string]any{"label": "Voice", "ok": true, "notes": "Kokoro, offline"})
		send("step", map[string]any{"label": "Wake word", "ok": false, "notes": "the detector didn't install (pip failed)"})
		send("done", map[string]any{"ok": false, "steps": []any{
			map[string]any{"label": "Hearing", "ok": true, "notes": "whisper small.en (English)"},
			map[string]any{"label": "Voice", "ok": true, "notes": "Kokoro, offline"},
			map[string]any{"label": "Wake word", "ok": false, "notes": "the detector didn't install (pip failed)"},
		}})
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/voice/setup")))
	waitFor(t, ctx, `document.querySelector('#status').textContent.includes("isn’t set up")`, "the status")
	run(t, ctx, chromedp.Click("#start", chromedp.ByQuery))
	waitFor(t, ctx, `!!document.querySelector('#steps .dot.run')`, "Hearing running")
	close(release)
	waitFor(t, ctx, `document.querySelectorAll('#steps .dot.ok').length === 2 && document.querySelectorAll('#steps .dot.fail').length === 1`, "every step's outcome")
	// Regression: the page's refresh after setup wiped what it had shown.
	d.waitCalled("GET /voice/setup/status", 2)
	waitFor(t, ctx, `!document.querySelector('#start').disabled`, "the button is ready again")
	text := textOf(t, ctx, "#steps") + " " + textOf(t, ctx, "#startMsg")
	for _, want := range []string{"whisper small.en", "Kokoro, offline", "pip failed", "didn’t finish. Fix what it says"} {
		if !strings.Contains(text, want) {
			t.Errorf("page lacks %q: %s", want, text)
		}
	}
	if eval[bool](t, ctx, `document.querySelector('#start').disabled`) {
		t.Fatal("the button stayed disabled")
	}
}
