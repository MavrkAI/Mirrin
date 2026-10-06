package pagetest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// stateShot is one picture of the screen in a given state.
type stateShot struct {
	name    string
	data    map[string]any
	w, h    int64
	scheme  string
	path    string // "/ui" unless set ("/ui?mode=orb")
	browser bool   // a page open in the twin's browser
	touch   bool   // a phone held sideways: a touch screen, no hover
	events  []map[string]any
	after   func(t *testing.T, ctx context.Context, d *daemon) // anything to do before the picture
}

// MIRRIN_UI_SHOTS=<dir> also writes the screen in its other states: a quiet
// evening with nothing on, Mirrin thinking with a step showing, the browser
// open, a phone mid-conversation, Nyra, the floating widget with a long
// request, a small wall, a phone on its side, a small phone offline, paused
// here and on a phone, and Nyra dozing on the night clock.
func TestPresenceScreenStates(t *testing.T) {
	dir := os.Getenv("MIRRIN_UI_SHOTS")
	if dir == "" {
		t.Skip("set MIRRIN_UI_SHOTS to a folder to write screenshots of the screen's states")
	}
	_ = os.MkdirAll(dir, 0o755)
	nyra := func(m map[string]any) { m["persona"] = "nyra"; m["name"] = "Nyra" }
	longAsk := func(m map[string]any) {
		m["approvals"] = []any{map[string]any{"id": 31, "tool": "gmail_send", "risk": "write", "status": "pending", "created_at": time.Now().Format(time.RFC3339),
			"summary": "Email Sam Chen and Priya Patel the signed lease, the bond receipt and the inspection report, and ask them to confirm the move-in date of the fourteenth before Friday"}}
	}
	shots := []stateShot{
		{name: "state-empty-dark", data: screen(nil), w: 1440, h: 900, scheme: "dark"},
		{name: "state-empty-light", data: screen(nil), w: 1440, h: 900, scheme: "light"},
		{name: "state-thinking", data: busyDay(), w: 1440, h: 900, scheme: "dark", events: []map[string]any{
			{"kind": "heard", "text": "Find me flights to Bali on the 2nd"}, {"kind": "state", "text": "thinking"},
			{"kind": "note", "text": "Checking jetstar.com"}}},
		{name: "state-browser", data: busyDay(), w: 1440, h: 900, scheme: "light", browser: true},
		// the page as one dialog over the screen, to watch; and an OK about the page on a phone
		{name: "state-browser-sheet-dark", data: busyDay(), w: 1440, h: 900, scheme: "dark", browser: true, after: openBrowserSheet},
		{name: "state-browser-sheet-light", data: busyDay(), w: 1440, h: 900, scheme: "light", browser: true, after: openBrowserSheet},
		{name: "state-browser-approval-phone", data: withMut(busyDay(), func(m map[string]any) { m["approvals"] = []any{pageApproval("pending", true)} }), w: 390, h: 844, scheme: "light", browser: true, after: func(t *testing.T, ctx context.Context, d *daemon) {
			waitFor(t, ctx, sheetMode("approval"), "the OK about the page")
		}},
		{name: "state-phone-speaking", data: busyDay(), w: 390, h: 844, scheme: "dark", events: []map[string]any{{"kind": "state", "text": "speaking"}}},
		{name: "nyra-dark", data: withMut(busyDay(), nyra), w: 1440, h: 900, scheme: "dark"},
		{name: "nyra-light", data: withMut(screen(nil), nyra), w: 1440, h: 900, scheme: "light"},
		{name: "nyra-thinking", data: withMut(busyDay(), nyra), w: 1440, h: 900, scheme: "light", events: []map[string]any{{"kind": "state", "text": "thinking"}}},
		{name: "orb-nyra", data: withMut(screen(nil), nyra), w: 380, h: 300, scheme: "dark", path: "/ui?mode=orb", events: []map[string]any{{"kind": "state", "text": "speaking"}}},
		{name: "orb-long-approval", data: withMut(screen(nil), longAsk), w: 380, h: 300, scheme: "light", path: "/ui?mode=orb"},
		{name: "wall-800x480", data: busyDay(), w: 800, h: 480, scheme: "dark"},
		{name: "phone-landscape-844x390", data: busyDay(), w: 844, h: 390, scheme: "dark", touch: true},
		{name: "phone-320-offline", data: busyDay(), w: 320, h: 640, scheme: "light", after: func(t *testing.T, ctx context.Context, d *daemon) {
			d.handle("GET /screen", textH(503, "starting"))
			run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
			waitFor(t, ctx, `document.querySelector('#state').textContent === 'Reconnecting'`, "offline")
		}},
		{name: "paused", data: withMut(screen(nil), pausedUntil9), w: 1440, h: 900, scheme: "dark"},
		{name: "phone-paused", data: withMut(busyDay(), func(m map[string]any) { m["paused"] = true; m["can"] = []string{"view", "chat", "approve"} }), w: 390, h: 844, scheme: "light"},
		{name: "night-doze", data: withMut(screen(nil), nyra), w: 1440, h: 900, scheme: "dark", after: func(t *testing.T, ctx context.Context, d *daemon) {
			d.handle("GET /screen", jsonH(withMut(screen(nil), func(m map[string]any) { nyra(m); m["quiet_hours"] = "00:00-23:59"; m["ambient_after_seconds"] = 60 })))
			run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
			waitFor(t, ctx, `data.quiet_hours === '00:00-23:59'`, "the night")
			ambientNow(t, ctx)
		}},
	}
	for _, s := range shots {
		d := newDaemon(t)
		d.handle("GET /screen", jsonH(s.data))
		if s.browser {
			d.handle("GET /browser/state", jsonH(map[string]any{"open": true, "url": "https://booking.jetstar.com/au/en/booking/select-flights", "title": "Jetstar · Select flights", "held": false}))
			d.handle("GET /browser/stream", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: frame\ndata: {\"d\":%q,\"w\":1280,\"h\":800}\n\n", dotJPEG)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			serveShot(d)
		}
		path := s.path
		if path == "" {
			path = "/ui"
		}
		ctx := tab(t)
		view := chromedp.EmulateViewport(s.w, s.h)
		if s.touch {
			view = chromedp.EmulateViewport(s.w, s.h, chromedp.EmulateScale(2), chromedp.EmulateMobile, chromedp.EmulateTouch, chromedp.EmulateLandscape)
		}
		run(t, ctx, view, emulateScheme(s.scheme), chromedp.Navigate(d.url(path)))
		waitFor(t, ctx, loaded, "loaded")
		run(t, ctx, chromedp.Sleep(800*time.Millisecond))
		for _, ev := range s.events {
			d.push(ev)
		}
		if s.after != nil {
			s.after(t, ctx, d)
		}
		run(t, ctx, chromedp.Sleep(1500*time.Millisecond))
		var png []byte
		if s.w > 1000 || strings.Contains(path, "orb") || s.touch || s.h <= 480 {
			run(t, ctx, chromedp.CaptureScreenshot(&png))
		} else {
			run(t, ctx, chromedp.FullScreenshot(&png, 90))
		}
		_ = os.WriteFile(filepath.Join(dir, s.name+".png"), png, 0o644)
	}
}

// openBrowserSheet opens the browser sheet from the panel, to watch.
func openBrowserSheet(t *testing.T, ctx context.Context, d *daemon) {
	waitFor(t, ctx, visible("#bOpen"), "the browser panel")
	run(t, ctx, chromedp.Click("#bOpen", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("watch"), "the sheet")
}

// withMut changes a /screen answer for one picture.
func withMut(m map[string]any, mut func(map[string]any)) map[string]any {
	mut(m)
	return m
}

// pausedUntil9 is the twin paused from the menu bar until 9:00 UTC, as
// /screen says it on the computer it runs on (every scope).
func pausedUntil9(m map[string]any) {
	m["paused"] = true
	m["paused_until"] = "2026-10-03T09:00:00Z"
	m["can"] = []string{"view", "chat", "approve", "admin"}
}

// Paused from the menu bar, the screen looks paused: the eyes close and it
// sways slowly, the pill says until when, the text box that it's paused, and
// on the computer it runs on Resume lifts the pause (POST /pause, local and
// admin). Something that needs you opens the eyes; Reconnecting stays amber
// and looks nothing like paused.
func TestPausedLooksPaused(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(pausedUntil9)))
	resumed := make(chan string, 1)
	d.handle("POST /pause", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		b, _ := json.Marshal(in)
		resumed <- string(b)
		d.handle("GET /screen", jsonH(screen(nil)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"paused":false}`))
	})
	ctx := tab(t)
	run(t, ctx, utc(), clockAt("2026-10-03T08:10:00Z"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && document.body.classList.contains('paused')`, "the screen paused")
	if p := textOf(t, ctx, "#state span"); p != "Paused until 9:00 AM" {
		t.Fatalf("the pill reads %q", p)
	}
	if eval[bool](t, ctx, `document.querySelector('#state').classList.contains('off')`) {
		t.Fatal("paused wears Reconnecting's amber")
	}
	if w := textOf(t, ctx, "#stateLine"); w != "Paused from the menu bar" {
		t.Fatalf("the words under the character: %q", w)
	}
	if !eval[bool](t, ctx, visible("#resumeBtn")) {
		t.Fatal("no Resume on the computer it runs on")
	}
	if ph := eval[string](t, ctx, `document.querySelector('#say').placeholder + '|' + document.querySelector('#askBar').textContent`); ph != "I'm paused. Resume me first.|I'm paused. Resume me first." {
		t.Fatalf("the text box says %q", ph)
	}
	eyes := `(() => { const o = document.querySelector('#talkOrb'); return [...o.querySelectorAll('.eye .open')].every(e => getComputedStyle(e).opacity === '0') && [...o.querySelectorAll('.eye .lid')].every(e => getComputedStyle(e).opacity === '1'); })()`
	if !eval[bool](t, ctx, eyes) {
		t.Fatal("paused, the eyes are open")
	}
	if s := eval[string](t, ctx, `getComputedStyle(document.querySelector('#talkOrb .pb')).animationName + ' ' + getComputedStyle(document.querySelector('#talkOrb .pb')).animationDuration`); s != "sway 7s" {
		t.Fatalf("paused, it moves as %q", s)
	}

	// Something that needs you opens the eyes; the pill still says paused.
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { pausedUntil9(m); oneApproval(m) })))
	run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
	waitFor(t, ctx, `!document.body.classList.contains('paused') && !!document.querySelector('.need')`, "the eyes open for what needs you")
	if p := textOf(t, ctx, "#state span"); p != "Paused until 9:00 AM" {
		t.Fatalf("with something waiting, the pill reads %q", p)
	}

	// Offline: Reconnecting, in amber, not paused.
	d.handle("GET /screen", textH(503, "starting"))
	run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
	waitFor(t, ctx, `document.querySelector('#state span').textContent === 'Reconnecting'`, "Reconnecting")
	if !eval[bool](t, ctx, `document.querySelector('#state').classList.contains('off') && !document.querySelector('#state').classList.contains('paused')`) {
		t.Fatal("Reconnecting looks like paused")
	}
	if eval[bool](t, ctx, visible("#resumeBtn")) || textOf(t, ctx, "#stateLine") != "" {
		t.Fatalf("offline, it still offers Resume: %q", textOf(t, ctx, "#stateLine"))
	}

	// Back, paused; Resume lifts it.
	d.handle("GET /screen", jsonH(screen(pausedUntil9)))
	run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
	waitFor(t, ctx, `document.body.classList.contains('paused') && document.querySelector('#state span').textContent === 'Paused until 9:00 AM'`, "paused again")
	run(t, ctx, chromedp.Click("#resumeBtn", chromedp.ByQuery))
	select {
	case got := <-resumed:
		if got != `{"paused":false}` {
			t.Fatalf("Resume sent %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resume sent nothing")
	}
	waitFor(t, ctx, `!document.body.classList.contains('paused') && document.querySelector('#state span').textContent === 'Idle' && document.querySelector('#resumeBtn').hidden`, "resumed")
	if ph := eval[string](t, ctx, `document.querySelector('#say').placeholder`); ph != "Message Mirrin…" {
		t.Fatalf("resumed, the text box says %q", ph)
	}
}

// Elsewhere (a phone, or a device that may not change settings) there is
// no Resume: the words say where to resume. A pause for no set time says
// just "Paused", and a standby's pause says where it moved to.
func TestPausedElsewhereSaysWhereToResume(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["paused"] = true
		m["can"] = []string{"view", "chat", "approve"}
	})))
	ctx := tab(t)
	// *.localhost reaches this computer, but isn't where the page knows it runs.
	run(t, ctx, phone(), chromedp.Navigate(strings.Replace(d.url("/ui"), "127.0.0.1", "phone.localhost", 1)))
	waitFor(t, ctx, loaded+` && document.body.classList.contains('paused')`, "the phone paused")
	if p := textOf(t, ctx, "#state span"); p != "Paused" {
		t.Fatalf("the pill reads %q", p)
	}
	if w := textOf(t, ctx, "#stateLine"); w != "Resume me from the menu bar on your Mac." {
		t.Fatalf("the words under the character: %q", w)
	}
	if eval[bool](t, ctx, `ON_THIS_COMPUTER || !document.querySelector('#resumeBtn').hidden`) {
		t.Fatal("Resume on a phone")
	}
	// Even a phone that may change settings: POST /pause is the computer's.
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { pausedUntil9(m); delete(m, "paused_until") })))
	run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
	waitFor(t, ctx, `may('admin')`, "the phone with admin")
	if eval[bool](t, ctx, `!document.querySelector('#resumeBtn').hidden`) || textOf(t, ctx, "#stateLine") != "Resume me from the menu bar on your Mac." {
		t.Fatalf("a phone with admin: %q", textOf(t, ctx, "#stateLine"))
	}

	// On the computer it runs on, without admin: no Resume either.
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["paused"] = true
		m["can"] = []string{"view", "chat"}
	})))
	ctx = tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && document.body.classList.contains('paused')`, "paused here")
	if eval[bool](t, ctx, `!document.querySelector('#resumeBtn').hidden`) || textOf(t, ctx, "#stateLine") != "Resume me from the menu bar on your Mac." {
		t.Fatalf("a device without admin: %q", textOf(t, ctx, "#stateLine"))
	}

	// A standby wasn't paused from the menu bar.
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { pausedUntil9(m); delete(m, "paused_until"); m["standing_by"] = "New Mac" })))
	run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
	waitFor(t, ctx, `document.querySelector('#stateLine').textContent === 'Standing by: moved to New Mac'`, "the standby's words")
	if eval[bool](t, ctx, `!document.querySelector('#resumeBtn').hidden`) {
		t.Fatal("Resume on a standby")
	}
}

// ambientNow puts the screen on its clock, as a minute without a touch does.
func ambientNow(t *testing.T, ctx context.Context) {
	t.Helper()
	run(t, ctx, chromedp.Evaluate(`lastActivity = Date.now() - 61000; tick(); true`, nil))
	waitFor(t, ctx, `document.body.classList.contains('ambient')`, "the clock")
}

// dozePaint lists the paints and shapes the dozing character refers to
// that aren't its own ('am') or aren't drawn in it.
const dozePaint = `(() => { const z = document.querySelector('#doze'), bad = []; for (const e of z.querySelectorAll('[fill^="url(#"], [stroke^="url(#"], [href^="#"]')) for (const a of ['fill', 'stroke', 'href']) { const v = e.getAttribute(a) || '', m = a === 'href' ? /^#(.+)$/.exec(v) : /^url\(#(.+)\)$/.exec(v); if (m && (!m[1].startsWith('am') || !z.querySelector('#' + CSS.escape(m[1])))) bad.push(m[1]); } return bad; })()`

// At 11:30pm the clock's dot gives way to the character, dozing in the
// corner with its lids half down, drawn with ids of its own; it moves a
// pixel a minute, wakes when the clock goes, and by day the dot is back.
// The owner's quiet hours set the night; "off" is never. Nyra dozes as
// herself, her lids half down.
func TestTheNightClockDozes(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["ambient_after_seconds"] = 60 })))
	ctx := tab(t)
	run(t, ctx, utc(), clockAt("2026-10-03T23:30:00Z"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && !!document.querySelector('#talkOrb svg')`, "the screen loaded")
	ambientNow(t, ctx)
	waitFor(t, ctx, `document.querySelector('#ambient').classList.contains('night') && !!document.querySelector('#doze svg')`, "the dozing character")
	if !eval[bool](t, ctx, visible("#doze svg")) || eval[bool](t, ctx, visible("#ambient .orbmini")) {
		t.Fatal("at night the clock shows the dot, not the character")
	}
	if !eval[bool](t, ctx, `document.querySelector('#doze').classList.contains('maverick')`) {
		t.Fatal("Mirrin's screen dozes as someone else")
	}
	// Every id on the page once, and the dozing character's paint its own.
	if dup := eval[[]string](t, ctx, `(() => { const seen = new Set(), dup = []; for (const e of document.querySelectorAll('[id]')) { if (seen.has(e.id)) dup.push(e.id); seen.add(e.id); } return dup; })()`); len(dup) > 0 {
		t.Fatalf("ids used twice: %v", dup)
	}
	if bad := eval[[]string](t, ctx, dozePaint); len(bad) > 0 {
		t.Fatalf("the dozing character paints with %v", bad)
	}
	clip := `getComputedStyle(document.querySelector('#doze .eye .open')).clipPath`
	waitFor(t, ctx, clip+` === 'inset(50% 0px 0px)'`, "its lids half down")
	if s := eval[string](t, ctx, `getComputedStyle(document.querySelector('#doze .pb')).animationName + ' ' + getComputedStyle(document.querySelector('#doze .pb')).animationDuration`); s != "breathe 7s" {
		t.Fatalf("it breathes as %q", s)
	}
	// A pixel a minute.
	type xy struct{ X, Y float64 }
	at := func() xy {
		t.Helper()
		return eval[xy](t, ctx, `(() => { const m = /translate\((-?\d+)px, (-?\d+)px\)/.exec(document.querySelector('#doze').style.transform); return m ? {X: +m[1], Y: +m[2]} : {X: 99, Y: 99}; })()`)
	}
	a := at()
	run(t, ctx, chromedp.Evaluate(`window.__skew += 60000; lastActivity = Date.now() - 61000; tick(); true`, nil))
	b := at()
	if a.X == 99 || b.X == 99 || math.Abs(a.X-b.X)+math.Abs(a.Y-b.Y) != 1 {
		t.Fatalf("over a minute it moved from %v to %v", a, b)
	}
	// A touch: the clock goes and it wakes.
	run(t, ctx, chromedp.Evaluate(`document.dispatchEvent(new PointerEvent('pointerdown', {bubbles: true})); true`, nil))
	waitFor(t, ctx, `!document.body.classList.contains('ambient') && `+clip+` === 'inset(0px)'`, "awake")
	// By day, the dot.
	run(t, ctx, chromedp.Evaluate(`window.__skew += 15 * 3600000; tick(); true`, nil)) // 2:31pm
	ambientNow(t, ctx)
	if eval[bool](t, ctx, `document.querySelector('#ambient').classList.contains('night') || !!document.querySelector('#doze svg')`) || !eval[bool](t, ctx, visible("#ambient .orbmini")) {
		t.Fatal("by day the character dozes")
	}
	// The owner's quiet hours: from 2pm it's night; off, never.
	for _, c := range []struct {
		hours string
		night bool
	}{{"14:00-18:00", true}, {"off", false}} {
		d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["ambient_after_seconds"] = 60; m["quiet_hours"] = c.hours })))
		run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
		waitFor(t, ctx, `data.quiet_hours === `+jsString(c.hours), "the quiet hours "+c.hours)
		run(t, ctx, chromedp.Evaluate(`tick(); true`, nil))
		if got := eval[bool](t, ctx, `document.querySelector('#ambient').classList.contains('night')`); got != c.night {
			t.Errorf("quiet hours %s at 2:31pm: night %v", c.hours, got)
		}
	}
	// Nyra dozes as herself, with ids of her own, her lids half down.
	ad := newDaemon(t)
	ad.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["persona"] = "nyra"; m["name"] = "Nyra"; m["ambient_after_seconds"] = 60 })))
	actx := tab(t)
	run(t, actx, utc(), clockAt("2026-10-03T23:30:00Z"), desktop(), chromedp.Navigate(ad.url("/ui")))
	waitFor(t, actx, loaded+` && !!document.querySelector('#talkOrb #nyShell')`, "Nyra's screen loaded")
	ambientNow(t, actx)
	waitFor(t, actx, `document.querySelector('#ambient').classList.contains('night') && document.querySelector('#doze').classList.contains('nyra') && !!document.querySelector('#doze #amShell')`, "Nyra dozing")
	if dup := eval[[]string](t, actx, `(() => { const seen = new Set(), dup = []; for (const e of document.querySelectorAll('[id]')) { if (seen.has(e.id)) dup.push(e.id); seen.add(e.id); } return dup; })()`); len(dup) > 0 {
		t.Fatalf("Nyra dozing: ids used twice: %v", dup)
	}
	if bad := eval[[]string](t, actx, dozePaint); len(bad) > 0 {
		t.Fatalf("Nyra dozing paints with %v", bad)
	}
	waitFor(t, actx, clip+` === 'inset(50% 0px 0px)' && [...document.querySelectorAll('#doze .eye .open')].length === 2`, "her lids half down")
	if s := eval[string](t, actx, `getComputedStyle(document.querySelector('#doze .pb')).animationName + ' ' + getComputedStyle(document.querySelector('#doze .pb')).animationDuration`); s != "breathe 7s" {
		t.Fatalf("Nyra breathes as %q", s)
	}
}

// With reduced motion, paused and dozing on the night clock hold still:
// nothing on the page is animating, and the character doesn't drift.
func TestPausedAndDozingHoldStillWithReducedMotion(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { pausedUntil9(m); m["ambient_after_seconds"] = 60 })))
	ctx := tab(t)
	run(t, ctx, reducedMotion(), utc(), clockAt("2026-10-03T23:30:00Z"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && document.body.classList.contains('paused')`, "the screen paused")
	ambientNow(t, ctx)
	waitFor(t, ctx, `!!document.querySelector('#doze svg')`, "the dozing character")
	run(t, ctx, chromedp.Sleep(300*time.Millisecond))
	if moving := eval[[]string](t, ctx, `document.getAnimations().filter(a => a.playState === 'running').map(a => (a.animationName || a.transitionProperty) + ' on ' + (a.effect && a.effect.target ? a.effect.target.id || a.effect.target.getAttribute('class') : '?'))`); len(moving) > 0 {
		t.Fatalf("still animating with reduced motion: %v", moving)
	}
	if tr := eval[string](t, ctx, `document.querySelector('#doze').style.transform`); tr != "" {
		t.Fatalf("it drifts with reduced motion: %q", tr)
	}
}

// watchReactions records each reaction class the character gains or loses
// from now on, with when, in window.__r ("+r-pleased@1234"): what changed
// by the end of each task, as the screen draws it.
const watchReactions = `(() => {
  window.__r = [];
  const o = document.querySelector(new URLSearchParams(location.search).get('mode') === 'orb' ? '#widget .orb' : '#talkOrb');
  let was = new Set(o.classList);
  new MutationObserver(() => {
    for (const k of ['r-pleased', 'r-sorry', 'r-attentive']) {
      const has = o.classList.contains(k);
      if (has !== was.has(k)) window.__r.push((has ? '+' : '-') + k + '@' + Math.round(performance.now()));
    }
    was = new Set(o.classList);
  }).observe(o, {attributes: true, attributeFilter: ['class']});
  return true;
})()`

// reactionsSeen is what watchReactions recorded, without the times.
const reactionsSeen = `window.__r.map(x => x.split('@')[0])`

// feedOpen waits for the page's live feed.
func feedOpen(t *testing.T, ctx context.Context) {
	t.Helper()
	waitFor(t, ctx, loaded+` && es && es.readyState === 1 && !!document.querySelector('#talkOrb svg')`, "the live feed")
}

// The twin's reaction plays once and is gone after 1.2 seconds: Mirrin
// nods, and a request for you is a glance towards Needs. The floating
// widget plays it too.
func TestAReactionPlaysOnceThenSettles(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	feedOpen(t, ctx)
	run(t, ctx, chromedp.Sleep(1800*time.Millisecond)) // after the hello wave
	run(t, ctx, chromedp.Evaluate(watchReactions, nil))
	d.push(map[string]any{"kind": "react", "text": "pleased", "data": map[string]string{"why": "a task finished"}})
	waitFor(t, ctx, `document.querySelector('#talkOrb').classList.contains('r-pleased')`, "the nod")
	if a := eval[string](t, ctx, `getComputedStyle(document.querySelector('#talkOrb .head')).animationName`); a != "rnod" {
		t.Fatalf("Mirrin reacts with %q, not a nod", a)
	}
	waitFor(t, ctx, `!document.querySelector('#talkOrb').classList.contains('r-pleased')`, "the nod to end")
	run(t, ctx, chromedp.Sleep(300*time.Millisecond))
	if got := eval[[]string](t, ctx, reactionsSeen); strings.Join(got, " ") != "+r-pleased -r-pleased" {
		t.Fatalf("played %v, want once", got)
	}
	if ms := eval[float64](t, ctx, `(() => { const at = window.__r.map(x => +x.split('@')[1]); return at[1] - at[0]; })()`); ms < 1100 || ms > 1600 {
		t.Fatalf("the nod lasted %.0fms, want 1.2s", ms)
	}

	// Something needs you: the eyes go towards Needs, on the right, and back.
	d.push(map[string]any{"kind": "react", "text": "attentive", "data": map[string]string{"why": "something needs you"}})
	waitFor(t, ctx, `parseFloat(getComputedStyle(document.querySelector('#talkOrb')).getPropertyValue('--lx')) > .4`, "a glance towards Needs")
	waitFor(t, ctx, `!document.querySelector('#talkOrb').classList.contains('r-attentive')`, "the glance to end")

	// The widget, smaller (a stand-in of its own: each feed goes to one page).
	wd := newDaemon(t)
	wd.handle("GET /screen", jsonH(screen(nil)))
	w := tab(t)
	run(t, w, chromedp.EmulateViewport(380, 300), chromedp.Navigate(wd.url("/ui?mode=orb")))
	waitFor(t, w, loaded+` && es && es.readyState === 1 && !!document.querySelector('#widget .orb svg')`, "the widget")
	wd.push(map[string]any{"kind": "react", "text": "sorry", "data": map[string]string{"why": "a task stopped after a problem"}})
	waitFor(t, w, `document.querySelector('#widget .orb').classList.contains('r-sorry')`, "the widget's tilt")
	if a := eval[string](t, w, `getComputedStyle(document.querySelector('#widget .orb .head')).animationName + ' ' + getComputedStyle(document.querySelector('#widget .orb')).getPropertyValue('--rk').trim()`); a != "rsorry .6" {
		t.Fatalf("the widget reacts as %q", a)
	}
}

// On the clock, or with reduced motion, nothing moves.
func TestNoReactionOnTheClockOrWithReducedMotion(t *testing.T) {
	for _, c := range []string{"the clock", "reduced motion"} {
		t.Run(c, func(t *testing.T) {
			d := newDaemon(t)
			d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["ambient_after_seconds"] = 60 })))
			ctx := tab(t)
			if c == "reduced motion" {
				run(t, ctx, reducedMotion())
			}
			run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
			feedOpen(t, ctx)
			if c == "the clock" {
				ambientNow(t, ctx)
			}
			run(t, ctx, chromedp.Evaluate(watchReactions, nil))
			d.push(map[string]any{"kind": "react", "text": "pleased", "data": map[string]string{"why": "a task finished"}})
			d.push(map[string]any{"kind": "state", "text": "idle"}) // something after it, to know it arrived
			run(t, ctx, chromedp.Sleep(500*time.Millisecond))
			if got := eval[[]string](t, ctx, reactionsSeen); len(got) != 0 {
				t.Fatalf("with %s it reacted: %v", c, got)
			}
		})
	}
}

// A short thank-you is pleased as the reply starts; "no thanks" isn't.
func TestAThankYouIsPleasedButNoThanksIsNot(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("POST /message/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\"My pleasure.\"}\n\nevent: done\ndata: {\"reply\":\"My pleasure.\"}\n\n")
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	feedOpen(t, ctx)
	run(t, ctx, chromedp.Sleep(1800*time.Millisecond), chromedp.Evaluate(watchReactions, nil))
	for _, said := range []string{"no thanks", "thanks but no", "perfect, now book the 9pm one too", "what's on today?"} {
		run(t, ctx, chromedp.Evaluate(`send(`+jsString(said)+`).then(() => true)`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	}
	run(t, ctx, chromedp.Sleep(300*time.Millisecond))
	if got := eval[[]string](t, ctx, reactionsSeen); len(got) != 0 {
		t.Fatalf("reacted to a decline or a request: %v", got)
	}
	run(t, ctx, chromedp.Evaluate(`send("Thanks, perfect").then(() => true)`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	waitFor(t, ctx, `window.__r.length === 2`, "the thank-you's reaction to play and end")
	if got := eval[[]string](t, ctx, reactionsSeen); strings.Join(got, " ") != "+r-pleased -r-pleased" {
		t.Fatalf("a thank-you played %v", got)
	}
}

// What finished more than an hour ago, today, is one quiet line at the end
// of Tasks: up to three titles, then "and more", never a count. One that
// finished in the last hour is still its card, and at midnight the line
// clears.
func TestDoneTodayIsOneQuietLine(t *testing.T) {
	at := func(s string) string { return "2026-10-03T" + s + ":00Z" }
	done := func(id, title, when string) map[string]any {
		return map[string]any{"id": id, "title": title, "status": "done", "updated": when, "steps": []any{}}
	}
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["tasks"] = []any{
			done("a", "Booked Ottolenghi", at("09:00")),
			done("b", "Moved the dentist", at("11:30")),
			done("c", "Found a plumber", at("14:40")),              // twenty minutes ago: still a card
			done("d", "Chased the refund", "2026-10-02T22:00:00Z"), // yesterday
			map[string]any{"id": "e", "title": "Renew the passport", "status": "failed", "updated": at("10:00"), "steps": []any{}},
		}
	})))
	ctx := tab(t)
	run(t, ctx, utc(), clockAt("2026-10-03T15:00:00Z"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!!document.querySelector('#tasks .donetoday')`, "Done today")
	if got := textOf(t, ctx, "#tasks .donetoday"); got != "Done today: Booked Ottolenghi · Moved the dentist" {
		t.Fatalf("the line reads %q", got)
	}
	cards := eval[[]string](t, ctx, `[...document.querySelectorAll('#tasks .task .ti')].map(e => e.textContent)`)
	if !slices.Equal(cards, []string{"Renew the passport", "Found a plumber"}) {
		t.Fatalf("cards %q", cards)
	}
	if eval[bool](t, ctx, `/\d/.test(document.querySelector('#tasks .donetoday').textContent) || !document.querySelector('#taskN').hidden`) {
		t.Fatal("Done today shows a count")
	}

	// An hour on, the plumber joins the line; with four, "and more".
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["tasks"] = []any{
			done("a", "Booked Ottolenghi", at("09:00")), done("b", "Moved the dentist", at("11:30")),
			done("c", "Found a plumber", at("14:40")), done("f", "Paid the council", at("12:00")),
		}
	})))
	run(t, ctx, chromedp.Evaluate(`window.__skew += 3600000; load(); true`, nil))
	waitFor(t, ctx, `document.querySelector('#tasks .donetoday').textContent.endsWith('and more')`, "and more")
	if got := textOf(t, ctx, "#tasks .donetoday"); got != "Done today: Booked Ottolenghi · Moved the dentist · Paid the council and more" {
		t.Fatalf("the line reads %q", got)
	}

	// Midnight: it clears, and Tasks goes with it.
	run(t, ctx, chromedp.Evaluate(`window.__skew += 8 * 3600000; load(); true`, nil)) // 00:00 the next day
	waitFor(t, ctx, `!document.querySelector('#tasks .donetoday') && document.querySelector('#secTasks').hidden`, "the line cleared at midnight")
}
