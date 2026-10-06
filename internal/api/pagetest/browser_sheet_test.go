package pagetest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// The browser sheet: when something needs to run in the twin's browser, or
// needs the owner there, the page comes up as one dialog over the screen,
// and the screen stays as it was underneath (the "In the browser" panel
// too, as a live thumbnail).

// browserFake is the twin's browser as the screen sees it, changed as a test
// goes; it records what the page sends to it.
type browserFake struct {
	mu     sync.Mutex
	state  map[string]any
	inputs []string
}

func newBrowserFake(d *daemon, state map[string]any) *browserFake {
	b := &browserFake{state: state}
	d.handle("GET /browser/state", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		s := maps.Clone(b.state)
		b.mu.Unlock()
		jsonH(s)(w, r)
	})
	d.handle("GET /browser/stream", oneFrame)
	d.handle("POST /browser/input", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		typ, _ := m["type"].(string)
		key, _ := m["key"].(string)
		text, _ := m["text"].(string)
		b.mu.Lock()
		b.inputs = append(b.inputs, typ+":"+key+text)
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return b
}

// set changes the browser's state: the next look at it sees this.
func (b *browserFake) set(state map[string]any) {
	b.mu.Lock()
	b.state = state
	b.mu.Unlock()
}

func (b *browserFake) sent() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.inputs, " ")
}

// The twin on a hotel page, and a page it handed over.
func onBooking(active bool) map[string]any {
	return map[string]any{"open": true, "url": "https://www.booking.com/hotel/bali", "title": "Villa Bali · Booking.com", "held": false, "active": active}
}

func handedOverPage() map[string]any {
	return map[string]any{"open": true, "url": "https://www.jetstar.com/", "title": "Jetstar", "held": true, "handover": true, "ask": "Solve the check, then tap Search.", "active": false}
}

// look has the page look at the browser now, and waits for it to finish.
func look(t *testing.T, ctx context.Context) {
	t.Helper()
	run(t, ctx, chromedp.Evaluate(`browserPoll().then(() => true)`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
}

const (
	sheetUp   = `!document.querySelector('#bLayer').hidden`
	sheetDown = `document.querySelector('#bLayer').hidden`
)

func sheetMode(mode string) string {
	return sheetUp + ` && document.querySelector('#bSheet').dataset.mode === ` + jsString(mode)
}

// A hand-over comes up by itself, over the screen, which stays as it was:
// the panel in its column, the Needs card and the chip to come back by.
// Later never hands back, and the same hand-over doesn't come up again.
func TestAHandOverComesToYouAndKeepsTheScreen(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	newBrowserFake(d, handedOverPage())
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, sheetMode("handover"), "the hand-over")
	for what, expr := range map[string]string{
		"says what it is":            `document.querySelector('#bsTitle').textContent === 'Mirrin needs you on this page'`,
		"the ask has focus":          `document.activeElement === document.querySelector('#bAsk')`,
		"the screen is out of reach": `document.querySelector('.app').inert`,
		"the panel stays put":        `(() => { const s = document.querySelector('#secBrowser'); return !s.hidden && s.parentElement === document.querySelector('#panels') && getComputedStyle(s).position !== 'fixed'; })()`,
		"Later, not Hand back":       `document.querySelector('#bsClose').textContent === 'Later'`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("hand-over: %s", what)
		}
	}
	run(t, ctx, chromedp.Click("#bsClose", chromedp.ByQuery))
	waitFor(t, ctx, sheetDown, "Later put it away")
	if n := d.count("POST /browser/control"); n != 0 {
		t.Fatalf("Later handed the page back (%d)", n)
	}
	// still waiting for you, where you'd look
	waitFor(t, ctx, `(() => { const c = document.querySelector('#needs .need.hero'); return !!c && c.classList.contains('hocard') && c.querySelector('.s').textContent.startsWith('Mirrin needs you on this page: Solve the check') && c.querySelector('.yes').textContent === 'Open the page'; })()`, "the hand-over leads Needs you")
	waitFor(t, ctx, visible("#bWait")+` && document.querySelector('#bWait').textContent.startsWith('Waiting for you: Solve the check')`, "the panel says it waits")
	waitFor(t, ctx, visible("#bChip")+` && document.querySelector('#bChip').classList.contains('need') && document.querySelector('#bChip').textContent === 'Mirrin needs you on jetstar.com · Open'`, "the amber chip")
	look(t, ctx)
	look(t, ctx)
	run(t, ctx, chromedp.Sleep(300*time.Millisecond))
	if eval[bool](t, ctx, sheetUp) {
		t.Fatal("the same hand-over came up again")
	}
	run(t, ctx, chromedp.Click("#needs .hocard .yes", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("handover")+` && document.activeElement === document.querySelector('#bAsk')`, "the card brought it back")
}

// keyDownOnly presses a key as a browser reports it to the page, without the
// separate character event the test's KeyEvent adds (which a real browser
// drops once the key is handled), so where the page puts it is all there is.
func keyDownOnly(key string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if err := input.DispatchKeyEvent(input.KeyDown).WithKey(key).Do(ctx); err != nil {
			return err
		}
		return input.DispatchKeyEvent(input.KeyUp).WithKey(key).Do(ctx)
	})
}

// Typing never gets cut off: a run waits as a chip while there are words in
// the box, and a hand-over says it's there and waits while the words are
// being written (a pause to think included), keeping the draft. If the
// owner carries on writing once it's up, the letters are the draft's: the
// sheet goes to Later and the hand-over waits on the chip. Letters reach the
// page only once you've clicked it.
func TestTheBrowserNeverCutsInWhileYouType(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	b := newBrowserFake(d, onBooking(false))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `bActive === false && document.querySelector('#bThumbImg').src.startsWith('data:image/jpeg')`, "the panel")
	run(t, ctx, chromedp.Click("#askBar", chromedp.ByQuery), chromedp.SendKeys("#say", "remind me", chromedp.ByQuery))
	b.set(onBooking(true))
	d.push(map[string]any{"kind": "browser", "text": "active"})
	run(t, ctx, chromedp.Sleep(2*time.Second))
	for what, expr := range map[string]string{
		"no sheet":           sheetDown,
		"the chip offers it": visible("#bChip") + ` && document.querySelector('#bChip').textContent.endsWith('· Watch')`,
		"focus stays":        `document.activeElement === document.querySelector('#say')`,
		"the words stay":     `document.querySelector('#say').value === 'remind me'`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("a run while typing: %s", what)
		}
	}
	// A hand-over: said at once, up once the words have rested a while.
	run(t, ctx, chromedp.KeyEvent(kb.End))
	last := time.Now()
	b.set(handedOverPage())
	d.push(map[string]any{"kind": "browser", "text": "handover"})
	waitFor(t, ctx, `document.querySelector('#sr').textContent.includes('Mirrin needs you on jetstar.com. It opens when you stop typing')`, "the hand-over said it waits")
	waitFor(t, ctx, visible("#bChip")+` && document.querySelector('#bChip').classList.contains('need') && !!document.querySelector('#needs .hocard')`, "the chip and the card show it meanwhile")
	// a pause to think, past a few seconds, isn't the end of the sentence
	run(t, ctx, chromedp.Sleep(time.Until(last.Add(8*time.Second))))
	if eval[bool](t, ctx, sheetUp) {
		t.Fatal("the hand-over cut in 8s after a key, over a draft")
	}
	waitFor(t, ctx, sheetMode("handover")+` && document.activeElement === document.querySelector('#bAsk')`, "the hand-over once the typing stopped")
	if took := time.Since(last); took < 9500*time.Millisecond || took > 12*time.Second {
		t.Errorf("came up %v after the last key, over a draft (want about 10s)", took)
	}
	if got := eval[string](t, ctx, `document.querySelector('#say').value`); got != "remind me" {
		t.Fatalf("the draft is %q", got)
	}
	// Writing on: the letter (a space first) is the draft's, not the sheet's.
	run(t, ctx, keyDownOnly(" "))
	waitFor(t, ctx, sheetDown+` && document.activeElement === document.querySelector('#say') && document.querySelector('#say').value === 'remind me '`, "the sheet went to Later and the space went to the draft")
	run(t, ctx, chromedp.KeyEvent("to call mum"))
	if got := eval[string](t, ctx, `document.querySelector('#say').value`); got != "remind me to call mum" {
		t.Fatalf("the draft is %q", got)
	}
	if n := d.count("POST /browser/input") + d.count("POST /browser/control"); n != 0 {
		t.Fatalf("the draft's letters went to the page (%d): %q", n, b.sent())
	}
	waitFor(t, ctx, visible("#bChip")+` && document.querySelector('#bChip').classList.contains('need') && !!document.querySelector('#needs .hocard')`, "the hand-over waits on the chip and in Needs you")
	look(t, ctx)
	if eval[bool](t, ctx, sheetUp) {
		t.Fatal("the hand-over came back by itself")
	}
	run(t, ctx, chromedp.Click("#needs .hocard .yes", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("handover")+` && document.activeElement === document.querySelector('#bAsk')`, "the card brought it back")
	if got := textOf(t, ctx, "#bNote"); got != "Click the page to use it." {
		t.Fatalf("before the page is used, the note says %q", got)
	}
	// A letter before the page was clicked goes nowhere, and says why.
	run(t, ctx, chromedp.KeyEvent("x"), chromedp.Sleep(300*time.Millisecond))
	if d.count("POST /browser/input") != 0 {
		t.Fatalf("a letter went to the page unasked: %q", b.sent())
	}
	if got := textOf(t, ctx, "#bNote"); got != "Click the page to type on it." {
		t.Fatalf("the note says %q", got)
	}
	run(t, ctx, chromedp.Click("#bImg", chromedp.ByQuery), chromedp.KeyEvent("x"))
	for i := 0; i < 50 && !strings.Contains(b.sent(), "text:x"); i++ {
		run(t, ctx, chromedp.Sleep(20*time.Millisecond))
	}
	if !strings.Contains(b.sent(), "text:x") {
		t.Fatalf("clicked, the page got %q", b.sent())
	}
}

// askedAloud is the owner asking the twin something out loud, at this computer.
var askedAloud = map[string]any{"kind": "heard", "text": "Find me a villa in Bali", "data": map[string]any{"channel": "voice"}}

// The twin starting on a website, asked here, comes up to watch, quietly
// (nothing in the accent but the page), and goes with its reply (not
// another chat's), once per run.
func TestABrowsingRunOpensToWatchAndClosesWithTheReply(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	b := newBrowserFake(d, onBooking(false))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `bActive === false && document.querySelector('#bThumbImg').src.startsWith('data:image/jpeg')`, "the thumbnail")
	d.push(askedAloud)
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes('Find me a villa')`, "the question")
	b.set(onBooking(true))
	d.push(map[string]any{"kind": "browser", "text": "active"})
	waitFor(t, ctx, sheetMode("watch"), "the sheet, to watch")
	for what, expr := range map[string]string{
		"says where":                `document.querySelector('#bsTitle').textContent === 'Mirrin is on booking.com'`,
		"focus on its title":        `document.activeElement === document.querySelector('#bsTitle')`,
		"nothing calls for a press": `![...document.querySelectorAll('#bSheet .primary')].some(e => e.getClientRects().length)`,
		"Take over is quiet":        `document.querySelector('#bTake').textContent === 'Take over' && !document.querySelector('#bTake').hidden`,
		"opens by itself, here":     `document.querySelector('#bsAuto').checked`,
		"no chip while it's up":     `document.querySelector('#bChip').hidden`,
		"the live page":             `document.querySelector('#bImg').src.startsWith('data:image/jpeg')`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("watching: %s", what)
		}
	}
	// a reply in another chat isn't this one's
	d.push(map[string]any{"kind": "said", "text": "Sam's flight lands at six.", "data": map[string]any{"channel": "telegram"}})
	run(t, ctx, chromedp.Sleep(2500*time.Millisecond))
	if !eval[bool](t, ctx, sheetMode("watch")) {
		t.Fatal("another chat's reply took the sheet")
	}
	d.push(map[string]any{"kind": "said", "text": "Villa Bali has two nights free.", "data": map[string]any{"channel": "voice"}})
	start := time.Now()
	waitFor(t, ctx, sheetDown, "the sheet went with the reply")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("it went %v after the reply", took)
	}
	waitFor(t, ctx, `document.activeElement === document.body && document.querySelector('#bChip').textContent === 'Mirrin is on booking.com · Watch'`, "the chip, with focus back on the page")
	look(t, ctx)
	look(t, ctx)
	if eval[bool](t, ctx, sheetUp) {
		t.Fatal("the same run came up again")
	}
	// a new run comes up again
	b.set(onBooking(false))
	look(t, ctx)
	b.set(onBooking(true))
	d.push(map[string]any{"kind": "browser", "text": "active"})
	waitFor(t, ctx, sheetMode("watch"), "a new run")
}

// "Open this by itself" is the owner's to turn off, and stays off; a browser
// that won't keep it (storage refused) still works, on its default.
func TestTheOwnersChoiceIsRemembered(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	b := newBrowserFake(d, onBooking(true))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#bChip"), "the chip")
	run(t, ctx, chromedp.Click("#bChip", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("watch")+` && document.querySelector('#bsAuto').checked`, "watching, from the chip")
	run(t, ctx, chromedp.Click("#bsAuto", chromedp.ByQuery))
	waitFor(t, ctx, `localStorage.getItem('mirrin.browser.opens') === 'off' && !document.querySelector('#bsAuto').checked`, "the choice kept")
	b.set(onBooking(false))
	run(t, ctx, chromedp.Reload())
	waitFor(t, ctx, `typeof bActive !== 'undefined' && bActive === false`, "reloaded")
	d.push(askedAloud) // asked here: only the choice keeps it down
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes('Find me a villa')`, "the question")
	b.set(onBooking(true))
	look(t, ctx)
	run(t, ctx, chromedp.Sleep(2*time.Second))
	if !eval[bool](t, ctx, sheetDown+` && `+visible("#bChip")) {
		t.Fatal("turned off, it came up anyway (or offered nothing)")
	}

	// storage that throws: the default holds, and the page doesn't break
	d2 := newDaemon(t)
	d2.handle("GET /screen", jsonH(screen(nil)))
	b2 := newBrowserFake(d2, onBooking(false))
	ctx2 := tab(t)
	run(t, ctx2, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(`window.__errs = [];
			addEventListener('error', e => __errs.push(String(e.message)));
			addEventListener('unhandledrejection', e => __errs.push(String(e.reason)));
			Object.defineProperty(window, 'localStorage', {configurable: true, get() { throw new DOMException('blocked', 'SecurityError'); }});`).Do(ctx)
		return err
	}), chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d2.url("/ui")))
	waitFor(t, ctx2, `bActive === false`, "the page, with storage refused")
	d2.push(askedAloud)
	waitFor(t, ctx2, `document.querySelector('#transcript').textContent.includes('Find me a villa')`, "the question")
	b2.set(onBooking(true))
	look(t, ctx2)
	waitFor(t, ctx2, sheetMode("watch")+` && document.querySelector('#bsAuto').checked`, "the default: open by itself")
	run(t, ctx2, chromedp.Click("#bsAuto", chromedp.ByQuery))
	waitFor(t, ctx2, `!document.querySelector('#bsAuto').checked && autoPref() === false`, "turned off for this page")
	if errs := eval[[]string](t, ctx2, `window.__errs`); len(errs) > 0 {
		t.Fatalf("page errors: %q", errs)
	}
}

// pageApproval is an OK about the page: a payment with a screenshot of what
// it will submit.
func pageApproval(status string, page bool) map[string]any {
	return map[string]any{"id": 7, "tool": "check_spend", "risk": "dangerous", "status": status, "summary": "Pay Jetstar $389 for two seats to Bali",
		"chat": "screen:local", "created_at": time.Now().Format(time.RFC3339), "screenshot": "/screen/shot?path=x", "page": page}
}

// serveShot serves the approval's screenshot.
func serveShot(d *daemon) {
	jpeg, _ := base64.StdEncoding.DecodeString(dotJPEG)
	d.handle("GET /screen/shot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpeg)
	})
}

// An OK about the page comes up with what it will submit, Deny beside
// Approve and focus on neither; Esc is Later, never a no. Approved, it shows
// the twin carrying on, then goes.
func TestAPageApprovalComesWithItsScreenshot(t *testing.T) {
	d := newDaemon(t)
	var mu sync.Mutex
	status := "pending"
	d.handle("GET /screen", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		s := status
		mu.Unlock()
		jsonH(screen(func(m map[string]any) { m["approvals"] = []any{pageApproval(s, s == "pending")} }))(w, r)
	})
	d.handle("POST /approvals/7/approve", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond) // the twin submits, then replies
		mu.Lock()
		status = "approved"
		mu.Unlock()
		jsonH(map[string]string{"reply": "Paid."})(w, r)
	})
	serveShot(d)
	newBrowserFake(d, map[string]any{"open": true, "url": "https://www.jetstar.com/booking/payment", "title": "Payment · Jetstar", "held": false, "active": true})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, sheetMode("approval"), "the OK, with the page")
	waitFor(t, ctx, visible("#bsShotImg")+` && document.querySelector('#bsShotImg').naturalWidth > 0`, "what it will submit")
	for what, expr := range map[string]string{
		"says what it is":      `document.querySelector('#bsTitle').textContent === 'Mirrin needs your OK'`,
		"Deny beside Approve":  `(() => { const n = document.querySelector('#bsNeed .no').getBoundingClientRect(), y = document.querySelector('#bsNeed .yes').getBoundingClientRect(); return Math.abs(n.top - y.top) < 2 && n.right <= y.left && Math.abs(n.width - y.width) < 2; })()`,
		"focus on the request": `document.activeElement === document.querySelector('#bsNeed')`,
		"the caption":          `document.querySelector('#bsShotCap').textContent === 'What Mirrin will submit'`,
		"Later, not Deny":      `document.querySelector('#bsClose').textContent === 'Later'`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("the OK: %s", what)
		}
	}
	run(t, ctx, chromedp.KeyEvent(kb.Escape))
	waitFor(t, ctx, sheetDown, "Esc put it away")
	if d.count("POST /approvals/7/deny") != 0 {
		t.Fatal("Esc denied it")
	}
	waitFor(t, ctx, `!!document.querySelector('#needs .need .apage')`, "the card stays, with See the page")
	run(t, ctx, chromedp.Evaluate(`load(); true`, nil), chromedp.Sleep(1200*time.Millisecond))
	if eval[bool](t, ctx, sheetUp) {
		t.Fatal("the same OK came up again by itself")
	}
	run(t, ctx, chromedp.Click("#needs .need .apage", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("approval"), "See the page brought it back")
	run(t, ctx, chromedp.Click("#bsNeed .yes", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#bsTitle').textContent === 'Mirrin is carrying on'`, "carrying on while the twin submits")
	waitFor(t, ctx, sheetDown+` && document.querySelector('#toasts').textContent.includes('Approved:')`, "done, it goes")
}

// A request that isn't about the page stays a card: no sheet, no "See the page".
func TestAnApprovalNotAboutThePageStaysACard(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		a := pageApproval("pending", false)
		a["tool"], a["summary"] = "send_email", "Email Sam the receipt"
		m["approvals"] = []any{a}
	})))
	serveShot(d)
	newBrowserFake(d, onBooking(true))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#needs .need .yes"), "the card")
	run(t, ctx, chromedp.Sleep(2*time.Second))
	if eval[bool](t, ctx, sheetUp) || eval[bool](t, ctx, `!!document.querySelector('.apage')`) {
		t.Fatal("an email came up with the page")
	}
}

// Elsewhere (a phone, another computer) the page is to watch: a hand-over
// is offered, never brought up, since only the computer the twin runs on
// may take over; an OK about the page comes up, as it can be given there.
func TestTheSheetOnAnotherDevice(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	b := newBrowserFake(d, handedOverPage())
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.remoteURL("/ui")))
	waitFor(t, ctx, `typeof ON_THIS_COMPUTER !== 'undefined' && !ON_THIS_COMPUTER && bHand`, "the hand-over, seen from elsewhere")
	look(t, ctx)
	look(t, ctx)
	if eval[bool](t, ctx, sheetUp) {
		t.Fatal("a hand-over came up on another device")
	}
	waitFor(t, ctx, `(() => { const c = document.querySelector('#needs .hocard'); return !!c && c.querySelector('.hint').textContent === 'Do this on the computer Mirrin runs on.' && c.querySelector('.howatch').textContent === 'Watch'; })()`, "the card offers to watch")
	waitFor(t, ctx, visible("#bChip")+` && document.querySelector('#bChip').classList.contains('need') && document.querySelector('#bChip').textContent.endsWith('Watch')`, "the chip offers to watch")
	run(t, ctx, chromedp.Click("#needs .hocard .howatch", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("handover"), "watching the hand-over")
	for what, expr := range map[string]string{
		"no Take over":     `document.querySelector('#bTake').hidden`,
		"where to do it":   `document.querySelector('#bAsk').textContent.endsWith('You can watch here.')`,
		"Close, not Later": `document.querySelector('#bsClose').textContent === 'Close'`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("watching elsewhere: %s", what)
		}
	}

	// a phone: an OK about the page fills the screen, its answers in reach
	b.set(map[string]any{"open": true, "url": "https://www.jetstar.com/booking/payment", "title": "Payment · Jetstar", "held": false, "active": true})
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["approvals"] = []any{pageApproval("pending", true)} })))
	serveShot(d)
	pctx := tab(t)
	run(t, pctx, phone(), chromedp.Navigate(d.remoteURL("/ui")))
	waitFor(t, pctx, sheetMode("approval"), "the OK on a phone")
	run(t, pctx, chromedp.Sleep(400*time.Millisecond)) // its entrance
	type fit struct{ L, T, W, H, IW, IH, SW, YesB, NoB, YesH, CloseH float64 }
	f := eval[fit](t, pctx, `(() => { const s = document.querySelector('#bSheet').getBoundingClientRect(), y = document.querySelector('#bsNeed .yes').getBoundingClientRect(), n = document.querySelector('#bsNeed .no').getBoundingClientRect();
		return {L: s.left, T: s.top, W: s.width, H: s.height, IW: innerWidth, IH: innerHeight, SW: document.documentElement.scrollWidth, YesB: y.bottom, NoB: n.bottom, YesH: y.height, CloseH: document.querySelector('#bsClose').getBoundingClientRect().height}; })()`)
	if f.L != 0 || f.T != 0 || f.W != f.IW || f.H != f.IH {
		t.Errorf("the sheet is %+v, not the screen", f)
	}
	if f.YesB > f.IH || f.NoB > f.IH {
		t.Errorf("Approve or Deny below the fold: %+v", f)
	}
	if f.SW > f.IW {
		t.Errorf("the page scrolls sideways: %v > %v", f.SW, f.IW)
	}
	if f.YesH < 44 || f.CloseH < 44 {
		t.Errorf("targets under 44px: Approve %v, Close %v", f.YesH, f.CloseH)
	}
}

// Every control in the sheet shows where focus is, in each mode; with
// reduced motion the sheet simply appears.
func TestEveryControlInTheSheetShowsFocus(t *testing.T) {
	type at struct {
		In      bool
		Who, OS string
	}
	// walk moves focus n times and checks each control it lands on in the sheet
	walk := func(ctx context.Context, mode string, shift bool, n int) {
		t.Helper()
		seen := map[string]bool{}
		for i := 0; i < n; i++ {
			if shift {
				run(t, ctx, chromedp.KeyEvent(kb.Tab, chromedp.KeyModifiers(input.ModifierShift)))
			} else {
				run(t, ctx, chromedp.KeyEvent(kb.Tab))
			}
			a := eval[at](t, ctx, `(() => { const e = document.activeElement; return {In: !!e && document.querySelector('#bSheet').contains(e), Who: e ? (e.id || e.className || e.tagName) : '', OS: e ? getComputedStyle(e).outlineStyle : ''}; })()`)
			if !a.In {
				continue
			}
			seen[a.Who] = true
			if a.OS == "none" {
				t.Errorf("%s: %s has focus and no ring", mode, a.Who)
			}
		}
		t.Logf("%s: the keyboard reached %v", mode, seen)
		if len(seen) < 2 {
			t.Errorf("%s: the keyboard reached only %v", mode, seen)
		}
	}
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	b := newBrowserFake(d, onBooking(true))
	d.handle("POST /browser/control", func(w http.ResponseWriter, r *http.Request) {
		st := onBooking(false)
		st["held"] = true
		b.set(st)
		jsonH(st)(w, r)
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#bChip"), "the chip")
	run(t, ctx, chromedp.Click("#bChip", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("watch"), "watching")
	walk(ctx, "watch", false, 5)
	// driving, Tab is the page's: Shift+Tab leaves it
	run(t, ctx, chromedp.Click("#bTake", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("driving")+` && document.activeElement === document.querySelector('#bView')`, "driving")
	walk(ctx, "driving", true, 4)

	d2 := newDaemon(t)
	d2.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["approvals"] = []any{pageApproval("pending", true)} })))
	serveShot(d2)
	newBrowserFake(d2, onBooking(true))
	actx := tab(t)
	run(t, actx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d2.url("/ui")))
	waitFor(t, actx, sheetMode("approval"), "the OK")
	walk(actx, "approval", false, 5)

	rctx := tab(t)
	run(t, rctx, reducedMotion(), chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d2.url("/ui")))
	waitFor(t, rctx, sheetUp, "the OK, with reduced motion")
	if s := eval[float64](t, rctx, `parseFloat(getComputedStyle(document.querySelector('#bSheet')).animationDuration)`); s >= 0.001 {
		t.Fatalf("the sheet animates for %vs with reduced motion", s)
	}
}

// /ui#browser (the floating character, a notification) brings the sheet up
// only when something is happening there; a page left open just shows in
// its panel, and a sheet you opened stays through the twin's reply.
func TestTheBrowserLinkOpensOnlyWhenSomethingIsHappening(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	newBrowserFake(d, onBooking(false))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui#browser")))
	waitFor(t, ctx, `location.hash === '' && document.activeElement === document.querySelector('#hBrowser')`, "the panel, for a page at rest")
	run(t, ctx, chromedp.Sleep(1500*time.Millisecond))
	if eval[bool](t, ctx, sheetUp) {
		t.Fatal("a page at rest popped up")
	}

	d2 := newDaemon(t)
	d2.handle("GET /screen", jsonH(screen(nil)))
	newBrowserFake(d2, onBooking(true))
	ctx2 := tab(t)
	run(t, ctx2, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d2.url("/ui#browser")))
	waitFor(t, ctx2, sheetMode("watch")+` && sheet.auto === false && location.hash === ''`, "the twin at work, opened from the link")
	d2.push(map[string]any{"kind": "said", "text": "Two nights are free."})
	run(t, ctx2, chromedp.Sleep(2500*time.Millisecond))
	if !eval[bool](t, ctx2, sheetUp) {
		t.Fatal("a sheet you opened went with the reply")
	}
	// watching, a letter talks to the twin: the sheet goes and the text box has it
	run(t, ctx2, chromedp.KeyEvent("h"))
	// (the test's key event may type the letter twice: see TestTypingTalksFromWhereFocusWasLeft)
	waitFor(t, ctx2, sheetDown+` && /^h+$/.test(document.querySelector('#say').value) && document.activeElement === document.querySelector('#say')`, "a letter started a message")
}

// A look at the page before it was handed over isn't leave to type on it:
// letters reach a handed-over page only once you click it, or come to it,
// after the hand-over. Tab from the ask passes the page on its way to Hand
// back, sending it nothing; once you've typed on the page, Tab is the
// page's. Handed back, focus is on what the sheet says now.
func TestALookBeforeAHandOverIsNotLeaveToType(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	b := newBrowserFake(d, onBooking(true))
	d.handle("POST /browser/control", func(w http.ResponseWriter, r *http.Request) {
		b.set(onBooking(true))
		jsonH(map[string]any{"open": true, "held": false, "active": true})(w, r)
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1280, 800), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#bChip"), "the chip")
	run(t, ctx, chromedp.Click("#bChip", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("watch"), "watching")
	// a click on the picture, and focus on it, while only watching
	run(t, ctx, chromedp.Click("#bImg", chromedp.ByQuery), chromedp.Focus("#bView", chromedp.ByQuery))
	if eval[bool](t, ctx, `sheet.armed`) {
		t.Fatal("watching, a click on the picture armed it")
	}
	b.set(handedOverPage())
	d.push(map[string]any{"kind": "browser", "text": "handover"})
	waitFor(t, ctx, sheetMode("handover")+` && document.activeElement === document.querySelector('#bAsk')`, "stepped up to the hand-over")
	if got := textOf(t, ctx, "#bNote"); got != "Click the page to use it." {
		t.Errorf("before the page is used, the note says %q", got)
	}
	run(t, ctx, chromedp.KeyEvent("z"), chromedp.Sleep(300*time.Millisecond))
	if got := b.sent(); got != "" {
		t.Fatalf("a letter went to the page unasked: %q", got)
	}
	// Tab: past the page, sending nothing, on to Hand back
	run(t, ctx, chromedp.KeyEvent(kb.Tab))
	waitFor(t, ctx, `document.activeElement === document.querySelector('#bView')`, "Tab came to the page")
	if got := textOf(t, ctx, "#bNote"); !strings.Contains(got, "Press Shift+Tab or Esc twice to leave the page.") {
		t.Errorf("on the page, the note says %q", got)
	}
	run(t, ctx, chromedp.KeyEvent(kb.Tab))
	waitFor(t, ctx, `document.activeElement === document.querySelector('#bTake') && document.querySelector('#bTake').textContent === 'Hand back'`, "Tab went on to Hand back")
	run(t, ctx, chromedp.Sleep(200*time.Millisecond))
	if got := b.sent(); got != "" {
		t.Fatalf("tabbing past the page sent it %q", got)
	}
	// back on the page, and typing on it: Tab is the page's
	run(t, ctx, chromedp.KeyEvent(kb.Tab, chromedp.KeyModifiers(input.ModifierShift)))
	waitFor(t, ctx, `document.activeElement === document.querySelector('#bView')`, "Shift+Tab back to the page")
	run(t, ctx, chromedp.KeyEvent("q"), chromedp.KeyEvent(kb.Tab))
	want := func() bool {
		s := b.sent()
		return len(s) == len("text:q key:Tab") && strings.Contains(s, "text:q") && strings.Contains(s, "key:Tab")
	}
	for i := 0; i < 50 && !want(); i++ {
		run(t, ctx, chromedp.Sleep(20*time.Millisecond))
	}
	if !want() || !eval[bool](t, ctx, `document.activeElement === document.querySelector('#bView')`) {
		t.Fatalf("typing on the page sent %q, and Tab stayed on it: want the letter and the Tab", b.sent())
	}
	// Hand back: focus on what it says now, not a button that reads Take over
	run(t, ctx, chromedp.Focus("#bTake", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	d.waitCalled("POST /browser/control", 1)
	waitFor(t, ctx, sheetMode("watch")+` && document.activeElement === document.querySelector('#bsTitle') && document.querySelector('#bsTitle').textContent === 'Mirrin has the browser again' && !sheet.armed`, "handed back, focus on the title")
}

// The sheet hides the screen under it (no words show through), names the
// screenshot of what will be submitted above it, where it's seen first,
// in both themes; and "Open this by itself" is named by its own words, its
// hint a description, and on a phone it's a target a thumb can hit.
func TestTheSheetIsOpaqueAndSaysWhatItShows(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["approvals"] = []any{pageApproval("pending", true)} })))
	serveShot(d) // a tall screenshot at this width
	newBrowserFake(d, map[string]any{"open": true, "url": "https://www.jetstar.com/booking/payment", "title": "Payment · Jetstar", "held": false, "active": true})
	for _, scheme := range []string{"dark", "light"} {
		ctx := tab(t)
		run(t, ctx, chromedp.EmulateViewport(1280, 800), emulateScheme(scheme), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, sheetMode("approval")+` && document.querySelector('#bsShotImg').naturalWidth > 0`, "the OK, with what it will submit")
		run(t, ctx, chromedp.Sleep(400*time.Millisecond)) // its entrance
		if !eval[bool](t, ctx, `(() => { const cs = getComputedStyle(document.querySelector('#bSheet')), c = (cs.backgroundColor.match(/[\d.]+/g) || []).map(Number);
			return c.length >= 3 && (c.length === 3 || c[3] === 1); })()`) {
			t.Errorf("%s: the sheet's background lets the screen through: %s", scheme, eval[string](t, ctx, `getComputedStyle(document.querySelector('#bSheet')).background`))
		}
		type at struct{ CapTop, CapBottom, ImgTop, ImgH, IH float64 }
		a := eval[at](t, ctx, `(() => { const c = document.querySelector('#bsShotCap').getBoundingClientRect(), i = document.querySelector('#bsShotImg').getBoundingClientRect();
			return {CapTop: c.top, CapBottom: c.bottom, ImgTop: i.top, ImgH: i.height, IH: innerHeight}; })()`)
		if a.CapTop > a.ImgTop || a.CapBottom > a.IH || a.CapTop < 0 {
			t.Errorf("%s: the caption is out of sight, or under the picture: %+v", scheme, a)
		}
		if a.ImgH < a.IH/2 {
			t.Logf("%s: the screenshot is only %vpx tall here", scheme, a.ImgH)
		}
	}

	// the choice to open by itself: its words name it, the hint describes it
	d2 := newDaemon(t)
	d2.handle("GET /screen", jsonH(screen(nil)))
	newBrowserFake(d2, onBooking(true))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1280, 800), chromedp.Navigate(d2.url("/ui")))
	waitFor(t, ctx, visible("#bChip"), "the chip")
	run(t, ctx, chromedp.Click("#bChip", chromedp.ByQuery))
	waitFor(t, ctx, sheetMode("watch")+` && `+visible("#bsAutoHint"), "watching, with the choice and its hint")
	type named struct{ Label, Hint, By string }
	n := eval[named](t, ctx, `(() => { const i = document.querySelector('#bsAuto'); return {Label: i.labels[0].textContent, Hint: document.querySelector('#bsAutoHint').textContent, By: i.getAttribute('aria-describedby')}; })()`)
	if n.Label != "Open this by itself when Mirrin starts on a website" || n.By != "bsAutoHint" || n.Hint != "When Mirrin needs you here, this opens anyway." {
		t.Errorf("the choice is named %q, described by %q (%q)", n.Label, n.By, n.Hint)
	}
	// on a phone, a target a thumb can hit
	pctx := tab(t)
	run(t, pctx, phone(), chromedp.Navigate(d2.remoteURL("/ui")))
	waitFor(t, pctx, visible("#bChip"), "the chip on a phone")
	run(t, pctx, chromedp.Click("#bChip", chromedp.ByQuery))
	waitFor(t, pctx, sheetMode("watch")+` && `+visible("#bsAutoRow"), "watching on a phone")
	if h := eval[float64](t, pctx, `document.querySelector('#bsAutoRow label').getBoundingClientRect().height`); h < 44 {
		t.Errorf("on a phone, Open this by itself is %vpx tall", h)
	}
}

// A run nobody asked for here (a routine, or a task carrying on by itself)
// is offered on the chip, never brought up over the screen; nor is one
// asked for in another chat. Asked here, out loud, the next one comes up.
func TestARunOpensToWatchOnlyWhenAskedHere(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	b := newBrowserFake(d, onBooking(false))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `bActive === false && document.querySelector('#bThumbImg').src.startsWith('data:image/jpeg')`, "the thumbnail")
	d.push(map[string]any{"kind": "heard", "text": "Book the villa", "data": map[string]any{"channel": "telegram"}})
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes('Book the villa')`, "a question in another chat")
	b.set(onBooking(true))
	d.push(map[string]any{"kind": "browser", "text": "active"})
	waitFor(t, ctx, visible("#bChip")+` && document.querySelector('#bChip').textContent === 'Mirrin is on booking.com · Watch'`, "the chip offers it")
	run(t, ctx, chromedp.Sleep(2*time.Second))
	if eval[bool](t, ctx, sheetUp) {
		t.Fatal("a run nobody asked for here came up over the screen")
	}
	b.set(onBooking(false))
	look(t, ctx)
	d.push(askedAloud)
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes('Find me a villa')`, "asked out loud")
	b.set(onBooking(true))
	d.push(map[string]any{"kind": "browser", "text": "active"})
	waitFor(t, ctx, sheetMode("watch")+` && sheet.auto`, "asked here, the run comes up to watch")
}

// On a phone, an OK about the page that comes in while words are being
// written says so and waits for them; once it's up, the next letter (a
// space) is still the words': the sheet goes to Later, nothing is answered,
// and the OK waits in Needs you.
func TestAnOKWaitsForWordsBeingWritten(t *testing.T) {
	d := newDaemon(t)
	var mu sync.Mutex
	approvals := []any{}
	d.handle("GET /screen", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		a := approvals
		mu.Unlock()
		jsonH(screen(func(m map[string]any) { m["approvals"] = a }))(w, r)
	})
	serveShot(d)
	newBrowserFake(d, map[string]any{"open": true, "url": "https://www.jetstar.com/booking/payment", "title": "Payment · Jetstar", "held": false, "active": true})
	ctx := tab(t)
	run(t, ctx, phone(), chromedp.Navigate(d.remoteURL("/ui")))
	waitFor(t, ctx, loaded+` && bActive === true && `+visible("#say"), "the screen, with the twin at work")
	run(t, ctx, chromedp.Click("#say", chromedp.ByQuery), chromedp.KeyEvent("book the"))
	mu.Lock()
	approvals = []any{pageApproval("pending", true)}
	mu.Unlock()
	d.push(map[string]any{"kind": "approval"})
	waitFor(t, ctx, `document.querySelector('#sr').textContent === 'Mirrin needs your OK on jetstar.com. It opens when you stop typing.'`, "the OK said it waits")
	waitFor(t, ctx, `!!document.querySelector('#needs .need .apage')`, "its card meanwhile")
	run(t, ctx, chromedp.Sleep(3500*time.Millisecond))
	if eval[bool](t, ctx, sheetUp) {
		t.Fatal("the OK cut in over words being written")
	}
	// the words rest past their while
	run(t, ctx, chromedp.Evaluate(`keyAt = Date.now() - 10000; maybeOpenSheet(); true`, nil))
	waitFor(t, ctx, sheetMode("approval")+` && document.activeElement === document.querySelector('#bsNeed')`, "the OK, once the words rested")
	run(t, ctx, keyDownOnly(" "))
	waitFor(t, ctx, sheetDown+` && document.activeElement === document.querySelector('#say') && document.querySelector('#say').value === 'book the '`, "the space went to the words, and the sheet to Later")
	run(t, ctx, chromedp.KeyEvent("window seat"))
	if got := eval[string](t, ctx, `document.querySelector('#say').value`); got != "book the window seat" {
		t.Fatalf("the words are %q", got)
	}
	if n := d.count("POST /approvals/7/approve") + d.count("POST /approvals/7/deny"); n != 0 {
		t.Fatalf("the words answered the OK (%d)", n)
	}
	waitFor(t, ctx, `!!document.querySelector('#needs .need .apage')`, "the OK waits in Needs you")
}
