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
	"github.com/chromedp/chromedp/kb"
)

// Nyra has her own character, on the screen and in the floating widget,
// chosen by /screen's persona. Mirrin is the man; every other persona keeps
// the penguin, and a switch shows on the next refresh
// without a reload.
func TestNyraHasHerOwnCharacter(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["persona"] = "nyra"; m["name"] = "Nyra" })))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!!document.querySelector('#talkOrb #nyShell') && document.querySelector('#talkOrb').classList.contains('nyra')`, "Nyra on the screen")
	uniqueIDs := `(() => { const ids = [...document.querySelectorAll('[id]')].map(e => e.id); return ids.length === new Set(ids).size; })()`
	for what, expr := range map[string]string{
		"one pair of eyes on the screen": `document.querySelectorAll('#talkOrb .eyes').length === 1 && document.querySelectorAll('#talkOrb svg').length === 1`,
		"no penguin left on the screen":  `!document.querySelector('#talkOrb #pgBody')`,
		"every id once":                  uniqueIDs,
		// a smile, with the mouth under it shut until she speaks
		"a smile": `!!document.querySelector('#talkOrb .head .smile') && getComputedStyle(document.querySelector('#talkOrb .lower')).transform === 'matrix(1, 0, 0, 0, 0, 0)'`,
		// each eye is open with a lid to blink, and only her brows stay
		// outside them, so a blink leaves no crease above the closed lid
		"two eyes that blink cleanly": `[...document.querySelectorAll('#talkOrb .eye')].filter(e => e.querySelector(':scope > .open .iris') && e.querySelector(':scope > path.lid')).length === 2 && document.querySelectorAll('#talkOrb .eyes > :not(.eye)').length === 1`,
		// the sparkle clip in her hair is her only part in the state's colour
		"the state's colour only on her clip": `(() => { const c = [...document.querySelectorAll('#talkOrb .bowc')]; return c.length > 0 && c.every(s => /^ny(Light|Halo)$/.test(s.parentNode.id)) && document.querySelectorAll('#talkOrb [fill="url(#nyLight)"], #talkOrb [fill="url(#nyHalo)"]').length === 2; })()`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("Nyra: %s", what)
		}
	}
	// She turns her head with her irises leading: they look the way --lx says.
	run(t, ctx, chromedp.Evaluate(`(() => { const o = document.querySelector('#talkOrb'); o.classList.add('follow'); o.style.setProperty('--lx', '1'); return true; })()`, nil))
	waitFor(t, ctx, `new DOMMatrix(getComputedStyle(document.querySelector('#talkOrb .iris')).transform).e > 1.5`, "her irises looking right")
	run(t, ctx, chromedp.Evaluate(`(() => { const o = document.querySelector('#talkOrb'); o.classList.remove('follow'); o.style.removeProperty('--lx'); return true; })()`, nil))
	// With no flippers to wave, she says hello with a tilt and a nod of her head.
	run(t, ctx, chromedp.Evaluate(`penguin('hi', 1700); true`, nil))
	waitFor(t, ctx, `document.querySelector('#talkOrb .head').getAnimations().some(a => a.animationName === 'nyrahi')`, "her hello nod")
	// A persona without its own character (Pickoo): the penguin is back on
	// the next refresh, with no reload.
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["persona"] = "pickoo"; m["name"] = "Pickoo" })))
	run(t, ctx, chromedp.Evaluate(`window.__same = true; load(); true`, nil))
	waitFor(t, ctx, `!!document.querySelector('#talkOrb #pgBody') && !document.querySelector('#nyShell') && !document.querySelector('#talkOrb').classList.contains('nyra')`, "the penguin back")
	if !eval[bool](t, ctx, `window.__same === true && document.querySelectorAll('#talkOrb .eyes').length === 1`) {
		t.Fatal("swapping back reloaded the page or left two characters")
	}
	// The floating widget draws her with its own gradient ids.
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["persona"] = "nyra" })))
	octx := orb(t, d)
	waitFor(t, octx, `!!document.querySelector('#widget .orb #nwShell')`, "Nyra in the widget")
	if !eval[bool](t, octx, uniqueIDs+` && !document.querySelector('#widget .orb #nyShell') && document.querySelectorAll('#widget .orb .eyes').length === 1`) {
		t.Fatal("the widget's Nyra shares ids with the screen's, or has two characters")
	}
	// A pack's persona with the same id is that character too; Mirrin is the
	// man, and so is the retired plain (and the rover she was drawn as, if
	// the screen last stored it); anyone else is the penguin; the stored
	// last character maps to itself.
	if got := eval[string](t, ctx, `[characterFor('nyra'), characterFor('starter/NYRA'), characterFor('mirrin'), characterFor('maverick'), characterFor('pack/mirrin'), characterFor('mavrk'), characterFor('pickoo'), characterFor('plain'), characterFor('pack/plain'), characterFor('robot'), characterFor('')].join(',')`); got != "nyra,nyra,maverick,maverick,maverick,maverick,penguin,maverick,maverick,maverick,penguin" {
		t.Fatalf("characterFor gave %s", got)
	}
}

// Mirrin is a man in a dinner jacket, on the screen and in the widget, and
// switching among the three characters leaves one drawing and unique ids.
func TestMaverickHasHisOwnCharacter(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["persona"] = "mirrin"; m["name"] = "Mirrin" })))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!!document.querySelector('#talkOrb #mgSuit') && document.querySelector('#talkOrb').classList.contains('maverick')`, "Mirrin on the screen")
	uniqueIDs := `(() => { const ids = [...document.querySelectorAll('[id]')].map(e => e.id); return ids.length === new Set(ids).size; })()`
	for what, expr := range map[string]string{
		"one drawing":  `document.querySelectorAll('#talkOrb svg').length === 1 && document.querySelectorAll('#talkOrb .eyes').length === 1 && !document.querySelector('#talkOrb #pgBody')`,
		"unique ids":   uniqueIDs,
		"mouth shut":   `getComputedStyle(document.querySelector('#talkOrb .lower')).transform === 'matrix(1, 0, 0, 0, 0, 0)'`,
		"eyes blink":   `[...document.querySelectorAll('#talkOrb .eye')].filter(e => e.querySelector(':scope > .open .iris') && e.querySelector(':scope > .lid')).length === 2`,
		"pin coloured": `(() => { const c = [...document.querySelectorAll('#talkOrb .bowc')]; return c.length > 0 && c.every(s => /^mg(Lt|Hl)$/.test(s.parentNode.id)); })()`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("Mirrin: %s", what)
		}
	}
	// Round the three, with no reload: Nyra, Pickoo's penguin, Mirrin again.
	run(t, ctx, chromedp.Evaluate(`window.__same = true; true`, nil))
	for _, c := range []struct{ persona, want string }{{"nyra", "#nyShell"}, {"pickoo", "#pgBody"}, {"mirrin", "#mgSuit"}} {
		d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["persona"] = c.persona })))
		run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
		waitFor(t, ctx, `!!document.querySelector('#talkOrb `+c.want+`') && document.querySelectorAll('#talkOrb svg').length === 1 && `+uniqueIDs, c.persona)
	}
	if !eval[bool](t, ctx, `window.__same === true && document.querySelector('#talkOrb').classList.contains('maverick') && !document.querySelector('#talkOrb').classList.contains('nyra')`) {
		t.Fatal("switching reloaded the page or left the wrong class")
	}
	// The widget draws him with its own ids.
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["persona"] = "mirrin" })))
	octx := orb(t, d)
	waitFor(t, octx, `!!document.querySelector('#widget .orb #mwSuit')`, "Mirrin in the widget")
	if !eval[bool](t, octx, uniqueIDs+` && !document.querySelector('#widget .orb #mgSuit')`) {
		t.Fatal("the widget's Mirrin shares ids with the screen's")
	}
}

// The retired persona plain is Mirrin now, so a screen told she is
// the persona draws him, on the screen and in the widget, with no rover
// left anywhere.
func TestRetiredPersonaIsDrawnAsMirrin(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["persona"] = "plain"; m["name"] = "Mirrin" })))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!!document.querySelector('#talkOrb #mgSuit') && document.querySelector('#talkOrb').classList.contains('maverick')`, "Mirrin on the screen")
	if !eval[bool](t, ctx, `document.querySelectorAll('#talkOrb svg').length === 1 && !document.querySelector('[id$="Enamel"]') && !document.querySelector('.robot')`) {
		t.Fatal("the screen still draws the rover")
	}
	octx := orb(t, d)
	waitFor(t, octx, `!!document.querySelector('#widget .orb #mwSuit')`, "Mirrin in the widget")
}

// Every character keeps the house rules: gradients only (no filter, mask,
// clip path or blur: Safari draws them as squares), every id under the
// prefix it is given, so two drawings on one page never share one, and
// every paint and shape it refers to is drawn within it. Each has the hooks
// every pose uses.
func TestCharactersKeepTheHouseRules(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `typeof Characters === 'object'`, "the drawings loaded")
	bad := eval[[]string](t, ctx, `(() => {
		const bad = [], hooks = ['pb', 'turn', 'front', 'head', 'eyes', 'eye', 'open', 'lid', 'lower', 'dots', 'bd'];
		for (const k of ['penguin', 'nyra', 'maverick']) {
			const s = Characters.draw(k, 'zz');
			if (/<filter|<mask|<clipPath|filter=|mask=|blur/i.test(s)) bad.push(k + ': a filter, mask, clip path or blur');
			const t = document.createElement('template');
			t.innerHTML = s;
			const svg = t.content.firstElementChild;
			if (!svg || svg.localName !== 'svg' || t.content.children.length !== 1) { bad.push(k + ': not one svg'); continue; }
			const ids = new Set();
			for (const e of svg.querySelectorAll('[id]')) {
				if (!e.id.startsWith('zz')) bad.push(k + ': id ' + e.id);
				if (ids.has(e.id)) bad.push(k + ': id twice ' + e.id);
				ids.add(e.id);
			}
			for (const e of svg.querySelectorAll('*')) for (const a of e.getAttributeNames()) {
				const v = e.getAttribute(a);
				for (const m of v.matchAll(/url\(#([^)]+)\)/g)) if (!ids.has(m[1])) bad.push(k + ': ' + a + ' url(#' + m[1] + ')');
				if (/^(xlink:)?href$/.test(a) && v.startsWith('#') && !ids.has(v.slice(1))) bad.push(k + ': href ' + v);
			}
			for (const h of hooks) if (!svg.querySelector('.' + h)) bad.push(k + ': no .' + h);
		}
		return bad;
	})()`)
	if len(bad) > 0 {
		t.Fatalf("the drawings break the house rules: %v", bad)
	}
}

// The floating widget's window is 380×300: a long request and a long reply
// both show their start inside it, never above its top edge.
func TestOrbFitsItsWindow(t *testing.T) {
	summary := "Email Sam Chen and Priya Patel the signed lease, the bond receipt and the inspection report, and ask them both to confirm the move-in date of the fourteenth before Friday afternoon, please"
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["approvals"] = []any{map[string]any{"id": 7, "tool": "gmail_send", "summary": summary, "risk": "write", "status": "pending"}}
	})))
	ctx := orb(t, d)
	waitFor(t, ctx, `document.querySelector('#bubble').classList.contains('show') && !!document.querySelector('#bubble .yes')`, "the request beside the character")
	run(t, ctx, chromedp.Sleep(500*time.Millisecond)) // the bubble's entrance
	type box struct{ Top, Bottom, Yes, H float64 }
	b := eval[box](t, ctx, `(() => { const r = document.querySelector('#bubble').getBoundingClientRect(), y = document.querySelector('#bubble .yes').getBoundingClientRect(); return {Top: r.top, Bottom: r.bottom, Yes: y.bottom, H: innerHeight}; })()`)
	if b.Top < 0 || b.Yes > b.H {
		t.Fatalf("a long request runs off the window: bubble top %.0f, Approve's bottom %.0f of %.0f", b.Top, b.Yes, b.H)
	}
	if got := textOf(t, ctx, "#bubble"); !strings.HasPrefix(got, "Needs you") || !strings.Contains(got, "Email Sam Chen") {
		t.Fatalf("the bubble reads %q", got)
	}

	// A long spoken reply shows its start.
	d2 := newDaemon(t)
	d2.handle("GET /screen", jsonH(screen(nil)))
	ctx2 := orb(t, d2)
	waitFor(t, ctx2, `es && es.readyState === 1`, "the live feed")
	reply := "Here is the plan for Friday. " + strings.Repeat("Then the next step, which takes a little while and needs a few words to explain properly. ", 10)
	d2.push(map[string]any{"kind": "said", "text": reply, "at": time.Now()})
	waitFor(t, ctx2, `document.querySelector('#bubble').classList.contains('show') && document.querySelector('#bubble').textContent.startsWith('Here is the plan')`, "the reply beside the character")
	run(t, ctx2, chromedp.Sleep(500*time.Millisecond))
	if top := eval[float64](t, ctx2, `document.querySelector('#bubble').getBoundingClientRect().top`); top < 0 {
		t.Fatalf("a %d-character reply starts %.0fpx above the window", len(reply), -top)
	}
}

// A click on the busy character opens the screen, where what it is doing
// shows; it used to only hop.
func TestClickingABusyOrbOpensTheScreen(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["state"] = "thinking" })))
	d.handle("POST /screen/open", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ctx := orb(t, d)
	// The fake feed opens with "idle", which could land after /screen's
	// "thinking": the feed says thinking too, once it is open, after its idle.
	waitFor(t, ctx, `es && es.readyState === 1`, "the live feed")
	d.push(map[string]any{"kind": "state", "text": "thinking"})
	waitFor(t, ctx, `document.body.classList.contains('thinking')`, "thinking")
	run(t, ctx, chromedp.Click("#widget .orbdock", chromedp.ByQuery))
	d.waitCalled("POST /screen/open", 1)
	if d.count("POST /voice/listen") != 0 {
		t.Fatal("a busy click started listening")
	}
}

// What was typed is never thrown away: Esc folds the text box, and it comes
// back with the words in it; focus goes to the bar that opens it, not the page.
func TestTypedWordsSurviveClosingTheTextBox(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	run(t, ctx, chromedp.Click("#askBar", chromedp.ByQuery), chromedp.SendKeys("#say", "remind me about the", chromedp.ByQuery), chromedp.KeyEvent(kb.Escape))
	waitFor(t, ctx, `!document.querySelector('#composer').classList.contains('show') && document.activeElement === document.querySelector('#askBar')`, "the box folded, focus on the bar")
	run(t, ctx, chromedp.Click("#askBar", chromedp.ByQuery))
	if v := eval[string](t, ctx, `document.querySelector('#say').value`); v != "remind me about the" {
		t.Fatalf("the words came back as %q", v)
	}
	// A tap on the character with words in the box goes back to them.
	run(t, ctx, chromedp.Evaluate(`document.querySelector('#askBar').focus(); true`, nil), chromedp.Click("#talkOrb", chromedp.ByQuery))
	waitFor(t, ctx, `document.activeElement === document.querySelector('#say') && document.querySelector('#composer').classList.contains('show')`, "the box kept open with its words")
	// A letter typed while a button has focus stays with the button.
	run(t, ctx, chromedp.Evaluate(`document.querySelector('#themeBtn').focus(); true`, nil), chromedp.KeyEvent("x"))
	if v := eval[string](t, ctx, `document.querySelector('#say').value`); strings.Contains(v, "x") {
		t.Fatalf("a letter typed on a focused button went to the text box: %q", v)
	}
}

// A half-typed answer survives the list refreshing, with focus and the caret.
func TestAnAnswerSurvivesARefresh(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(busyDay()))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!!document.querySelector('#needs form.answer input')`, "the answer box")
	run(t, ctx, chromedp.SendKeys("#needs form.answer input", "the comfortable", chromedp.ByQuery))
	// a new request arrives: the list re-renders
	d.handle("GET /screen", jsonH(withMut(busyDay(), func(m map[string]any) {
		m["approvals"] = append(m["approvals"].([]any), map[string]any{"id": 13, "tool": "pay", "summary": "Pay the plumber $180", "risk": "dangerous", "status": "pending"})
	})))
	run(t, ctx, chromedp.Evaluate(`window.__old = document.querySelector('#needs form.answer'); load(); true`, nil))
	waitFor(t, ctx, `document.querySelector('#needs form.answer') !== window.__old && document.querySelector('#needs').textContent.includes('plumber')`, "the list re-rendered")
	type st struct {
		Value   string
		Focused bool
		Caret   int
	}
	got := eval[st](t, ctx, `(() => { const i = document.querySelector('#needs form.answer input'); return {Value: i.value, Focused: document.activeElement === i, Caret: i.selectionStart}; })()`)
	if got.Value != "the comfortable" || !got.Focused || got.Caret != len("the comfortable") {
		t.Fatalf("after a refresh the answer box holds %+v", got)
	}
}

// A message that never reached the twin says so on the owner's own line,
// with Try again, instead of a red reply from the twin.
func TestAMessageNotSentOffersTryAgain(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	var mu sync.Mutex
	fail := true
	var sent []string
	d.handle("POST /message/stream", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct{ Text string }
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		f := fail
		sent = append(sent, m.Text)
		mu.Unlock()
		if f { // the connection drops before any answer: fetch fails
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				c.Close()
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: done\ndata: {\"reply\":\"Sunny all day.\"}\n\n")
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	run(t, ctx, chromedp.KeyEvent("w"), chromedp.Evaluate(`document.querySelector('#say').value = 'weather tomorrow?'; document.querySelector('#composer').requestSubmit(); true`, nil))
	waitFor(t, ctx, `!!document.querySelector('#transcript .line.heard.failed .retry')`, "Not sent, with Try again")
	if n := eval[int](t, ctx, `document.querySelectorAll('#transcript .line.said').length`); n != 0 {
		t.Fatalf("%d reply lines for a message that never left", n)
	}
	if v := eval[string](t, ctx, `document.querySelector('#say').value`); v != "weather tomorrow?" {
		t.Fatalf("what was typed wasn't kept: %q", v)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	run(t, ctx, chromedp.Click("#transcript .line.heard.failed .retry", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes('Sunny all day.') && !document.querySelector('#transcript .failed')`, "sent again, and answered")
	if n := eval[int](t, ctx, `[...document.querySelectorAll('#transcript .line.heard')].filter(l => l.textContent.includes('weather tomorrow?')).length`); n != 1 {
		t.Fatalf("the question shows %d times after Try again", n)
	}
}

// The twin's "stop asking?" takes one answer, whatever it is. A card's
// confirm row lasts only as long: other words sent go past it, and so does an
// Always allow that never reached the twin. A "no" from the row after that
// used to deny the request the twin had asked about last.
func TestTheConfirmRowLastsAsLongAsTheTwinsCheck(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["approvals"] = []any{map[string]any{"id": 7, "tool": "send_email", "summary": "Email the boss", "risk": "write", "status": "pending", "always_allow": true}}
	})))
	var mu sync.Mutex
	drop := false
	var sent []string
	d.handle("POST /message/stream", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct{ Text string }
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		f := drop
		sent = append(sent, m.Text)
		mu.Unlock()
		if f {
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				c.Close()
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: done\ndata: {\"reply\":\"Noted.\"}\n\n")
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && !!document.querySelector('#needs .need .always')`, "the request with Always allow")
	const row, idle = `!!document.querySelector('#needs .need.confirming .confirmrow')`, ` && !streaming`
	const gone = `!document.querySelector('#needs .confirmrow') && !document.querySelector('#needs .need.confirming') && !!document.querySelector('#needs .need .yes')`
	run(t, ctx, chromedp.Click("#needs .need .always", chromedp.ByQuery))
	waitFor(t, ctx, row+idle, "the confirm row, once the twin has the question")
	run(t, ctx, chromedp.Evaluate(`document.querySelector('#say').value = 'what is on today?'; document.querySelector('#composer').requestSubmit(); true`, nil))
	waitFor(t, ctx, gone+idle, "the row gone once other words reached the twin")
	mu.Lock()
	drop = true
	mu.Unlock()
	run(t, ctx, chromedp.Click("#needs .need .always", chromedp.ByQuery))
	waitFor(t, ctx, `!!document.querySelector('#transcript .line.heard.failed')`+idle, "Always allow not sent")
	waitFor(t, ctx, gone, "no row for a question the twin never got")
	mu.Lock()
	defer mu.Unlock()
	want := []string{"yes 7, always", "what is on today?", "yes 7, always"}
	for len(sent) > len(want) && sent[len(sent)-1] == "yes 7, always" {
		sent = sent[:len(sent)-1] // Chrome may try again on a connection that closed unanswered
	}
	if strings.Join(sent, "|") != strings.Join(want, "|") {
		t.Fatalf("the twin was sent %q, want %q", sent, want)
	}
}

// Driving the page, the sheet is a dialog: everything behind it is out of
// reach, the ask has focus, Shift+Tab leaves the page and Esc twice reaches
// Hand back. Keys used to go to the page with no way out. Handed back, it
// watches the twin carry on, and goes with the twin's reply.
func TestTheBrowserSheetHasAWayOut(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	var mu sync.Mutex
	state := map[string]any{"open": true, "url": "https://www.jetstar.com/", "title": "Jetstar", "held": true, "handover": true, "ask": "Solve the check, then tap Search.", "active": false}
	d.handle("GET /browser/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		jsonH(state)(w, r)
	})
	d.handle("GET /browser/stream", oneFrame)
	// handed back, the twin carries on in it
	d.handle("POST /browser/control", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		state = map[string]any{"open": true, "url": "https://www.jetstar.com/", "title": "Jetstar", "held": false, "active": true}
		mu.Unlock()
		jsonH(map[string]any{"open": true, "held": false, "active": true})(w, r)
	})
	var keys []string
	d.handle("POST /browser/input", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		typ, _ := m["type"].(string)
		key, _ := m["key"].(string)
		text, _ := m["text"].(string)
		mu.Lock()
		keys = append(keys, typ+":"+key+text)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!document.querySelector('#bLayer').hidden && document.querySelector('#bSheet').dataset.mode === 'handover'`, "the sheet")
	for what, expr := range map[string]string{
		"a modal dialog":           `document.querySelector('#bSheet').getAttribute('role') === 'dialog' && document.querySelector('#bSheet').getAttribute('aria-modal') === 'true'`,
		"the page behind is inert": `document.querySelector('.app').inert`,
		"the ask has focus":        `document.activeElement === document.querySelector('#bAsk')`,
		"Hand back is the way out": `document.querySelector('#bTake').textContent === 'Hand back' && !document.querySelector('#bTake').hidden`,
		"the ask says what to do":  `document.querySelector('#bAsk').textContent.includes('press Hand back')`,
		"the view says it is live": `document.querySelector('#bView').getAttribute('role') === 'application'`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("missing: %s", what)
		}
	}
	run(t, ctx, chromedp.Focus("#bView", chromedp.ByQuery), chromedp.KeyEvent("a"), chromedp.KeyEvent(kb.Escape), chromedp.KeyEvent(kb.Escape))
	waitFor(t, ctx, `document.activeElement === document.querySelector('#bTake')`, "Esc twice reached Hand back")
	if eval[bool](t, ctx, `document.querySelector('#composer').classList.contains('show')`) {
		t.Fatal("a letter typed on the page opened the hidden text box")
	}
	run(t, ctx, chromedp.Sleep(300*time.Millisecond))
	mu.Lock()
	got := strings.Join(keys, " ")
	mu.Unlock()
	if strings.Count(got, "Escape") != 1 || !strings.Contains(got, "text:a") {
		t.Fatalf("the page got %q; want the letter and one Esc (the second leaves)", got)
	}
	// Space there presses Hand back; it used to be typed into the page.
	run(t, ctx, chromedp.KeyEvent(" "))
	d.waitCalled("POST /browser/control", 1)
	// handed back, it watches the twin carry on
	waitFor(t, ctx, `document.querySelector('#bSheet').dataset.mode === 'watch' && document.querySelector('#bsTitle').textContent === 'Mirrin has the browser again' && !document.querySelector('#bLayer').hidden`, "watching the twin carry on")
	mu.Lock()
	if len(keys) != 2 {
		t.Fatalf("Space on Hand back went to the page too: %q", keys)
	}
	mu.Unlock()
	// and goes with the twin's reply
	d.push(map[string]any{"kind": "said", "text": "Found three flights."})
	waitFor(t, ctx, `document.querySelector('#bLayer').hidden`, "the sheet went with the reply")
}

// oneFrame serves the twin's browser as one still frame.
func oneFrame(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: frame\ndata: {\"d\":%q,\"w\":1280,\"h\":800}\n\n", dotJPEG)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// On a busy day the Now column scrolls, and its fading edge is a mask; a
// mask clips everything inside it, the fixed browser sheet too, which left
// the dimmed room on top of Hand back and the page. The sheet takes clicks
// whatever the column holds.
func TestTheBrowserSheetTakesClicksOnABusyDay(t *testing.T) {
	for _, size := range [][2]int64{{1440, 900}, {1280, 720}, {800, 480}} {
		d := newDaemon(t)
		d.handle("GET /screen", jsonH(busyDay()))
		d.handle("GET /browser/state", jsonH(map[string]any{"open": true, "url": "https://www.jetstar.com/", "title": "Jetstar", "held": true, "handover": true, "ask": "Solve the check, then tap Search."}))
		d.handle("GET /browser/stream", oneFrame)
		ctx := tab(t)
		run(t, ctx, chromedp.EmulateViewport(size[0], size[1]), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, `!document.querySelector('#bLayer').hidden && document.querySelector('#panels').classList.contains('scrolls')`, "the sheet over a scrolling column")
		run(t, ctx, chromedp.Sleep(400*time.Millisecond)) // the sheet's entrance
		for _, sel := range []string{"#bTake", "#bView"} {
			hit := fmt.Sprintf(`(() => { const e = document.querySelector(%q), r = e.getBoundingClientRect();
				const h = document.elementFromPoint(r.left + r.width / 2, (r.top + Math.min(r.bottom, innerHeight)) / 2);
				return h ? (e.contains(h) ? '' : h.tagName + '.' + h.className) : 'nothing'; })()`, sel)
			if got := eval[string](t, ctx, hit); got != "" {
				t.Errorf("%dx%d: a click on %s lands on %s", size[0], size[1], sel, got)
			}
		}
	}
}

// Typing a letter talks from wherever focus was simply left: the bar Esc
// gives it to, the card that keeps it after Approve, a task's Details just
// clicked. The hint says "just start typing", and these used to eat the
// letters.
func TestTypingTalksFromWhereFocusWasLeft(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(oneApproval)))
	d.handle("POST /approvals/7/approve", jsonH(map[string]string{"reply": "Sent."}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible(".need .yes"), "the approval card")
	typed := func(where string) {
		t.Helper()
		run(t, ctx, chromedp.KeyEvent("h"))
		waitFor(t, ctx, `document.querySelector('#composer').classList.contains('show') && /^h+$/.test(document.querySelector('#say').value) && document.activeElement === document.querySelector('#say')`, "a letter typed "+where+" opened the text box")
		run(t, ctx, chromedp.Evaluate(`document.querySelector('#say').value = ''; true`, nil), chromedp.KeyEvent(kb.Escape))
		waitFor(t, ctx, `!document.querySelector('#composer').classList.contains('show')`, "the box folded")
	}
	run(t, ctx, chromedp.Click("#askBar", chromedp.ByQuery), chromedp.KeyEvent(kb.Escape))
	waitFor(t, ctx, `document.activeElement === document.querySelector('#askBar')`, "Esc left focus on the bar")
	typed("after Esc")
	run(t, ctx, chromedp.Click(".task .toggle", chromedp.ByQuery))
	waitFor(t, ctx, `document.activeElement === document.querySelector('.task .toggle')`, "Details has focus")
	typed("after clicking Details")
	run(t, ctx, chromedp.Click(".need .yes", chromedp.ByQuery))
	d.waitCalled("POST /approvals/7/approve", 1)
	waitFor(t, ctx, `!!document.activeElement && document.activeElement.classList.contains('need')`, "the card keeps focus")
	typed("after Approve")
}

// On a phone or a tablet held upright, with little said yet, the
// conversation card hugs what it holds: its rows used to stretch to fill
// the page, leaving wide gaps between the character, the line under it and
// the suggestions.
func TestThePhoneConversationCardHugsItsContent(t *testing.T) {
	for _, size := range [][2]int64{{390, 844}, {820, 1180}} {
		d := newDaemon(t)
		d.handle("GET /screen", jsonH(screen(nil)))
		ctx := tab(t)
		run(t, ctx, chromedp.EmulateViewport(size[0], size[1], chromedp.EmulateScale(2), chromedp.EmulateMobile, chromedp.EmulateTouch), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, loaded+` && !document.querySelector('#suggest').hidden`, "the suggestions")
		type gaps struct{ Head, Under, Chips, Footer, H, Scroll float64 }
		g := eval[gaps](t, ctx, `(() => { const b = s => document.querySelector(s).getBoundingClientRect();
			const rail = b('.rail'), orb = b('#talkOrb'), tx = b('#transcript'), sg = b('#suggest'), ft = b('footer');
			return {Head: orb.top - rail.top, Under: tx.top - orb.bottom, Chips: sg.top - tx.bottom, Footer: ft.top, H: innerHeight, Scroll: document.documentElement.scrollHeight}; })()`)
		if g.Head > 24 || g.Under > 24 || g.Chips > 24 {
			t.Errorf("%dx%d: the card stretches: %.0fpx above the character, %.0fpx under it, %.0fpx before the suggestions", size[0], size[1], g.Head, g.Under, g.Chips)
		}
		// the page still fills the screen, with the footer at its foot and nothing to scroll
		if g.Footer < g.H/2 || g.Scroll > g.H {
			t.Errorf("%dx%d: the footer starts at %.0fpx of %.0f, the page is %.0fpx tall", size[0], size[1], g.Footer, g.H, g.Scroll)
		}
	}
}
