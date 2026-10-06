package pagetest

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
)

// orb opens the presence screen as the floating orb draws it.
func orb(t *testing.T, d *daemon) context.Context {
	t.Helper()
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(380, 300), chromedp.Navigate(d.url("/ui?mode=orb")))
	waitFor(t, ctx, loaded, "the orb loaded")
	return ctx
}

// A request waiting for a yes shows beside the orb, with its buttons.
func TestOrbShowsAnApprovalBesideIt(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["approvals"] = []any{map[string]any{"id": 7, "tool": "send_message", "summary": "Text Sam the invoice", "risk": "write", "status": "pending"}}
	})))
	ctx := orb(t, d)
	waitFor(t, ctx, `document.querySelector('#bubble').classList.contains('show')`, "the bubble beside the orb")
	if got := textOf(t, ctx, "#bubble"); !strings.Contains(got, "Text Sam the invoice") {
		t.Fatalf("the bubble reads %q", got)
	}
	if !eval[bool](t, ctx, `!!document.querySelector('#bubble button')`) {
		t.Fatal("no Approve or Deny beside the orb")
	}
	if eval[bool](t, ctx, `document.body.classList.contains('collapsed')`) {
		t.Fatal("the orb stayed folded away with a request waiting")
	}
}

// Clicking the idle orb starts listening, as saying its name would.
func TestClickingTheOrbListens(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	var asked atomic.Int32
	d.handle("POST /voice/listen", func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		jsonH(map[string]bool{"listening": true})(w, r)
	})
	ctx := orb(t, d)
	run(t, ctx, chromedp.Click("#widget .orbdock", chromedp.ByQuery))
	waitFor(t, ctx, `document.body.classList.contains('listening')`, "the orb listening")
	if asked.Load() != 1 {
		t.Fatalf("asked voice to listen %d times", asked.Load())
	}
}

// With voice off, a click says why instead of doing nothing.
func TestClickingTheOrbWithVoiceOffSaysWhy(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("POST /voice/listen", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"voice_off","message":"Voice isn't listening on this computer.","fix":"Set up voice from the menu bar."}`))
	})
	ctx := orb(t, d)
	run(t, ctx, chromedp.Click("#widget .orbdock", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#bubble').textContent.includes('Set up voice')`, "the reason beside the orb")
}
