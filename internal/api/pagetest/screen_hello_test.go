package pagetest

import (
	"context"
	"fmt"
	"testing"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// clockAt makes the page's clock read now (an RFC 3339 time), still
// ticking. A test moves it on by adding milliseconds to window.__skew.
func clockAt(now string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fmt.Sprintf(`(() => {
  const Real = Date;
  window.__skew = new Real(%q).getTime() - Real.now();
  class Fake extends Real { constructor(...a) { if (a.length) super(...a); else super(Real.now() + window.__skew); } static now() { return Real.now() + window.__skew; } }
  window.Date = Fake;
})()`, now)).Do(ctx)
		return err
	})
}

// mavrkHellos are Mirrin's hellos as the twin sends them to its owner.
func mavrkHellos(m map[string]any) {
	m["hellos"] = map[string]string{"morning": "Good morning, sir.", "afternoon": "Good afternoon, sir.", "evening": "Good evening, sir.", "late": "Late one, sir."}
}

// The screen greets the owner in the persona's words for the part of the
// day: "Good morning, sir." at 7:40, "Late one, sir." at 1am. Without
// them (a wall screen) it says the plain words, and "Good evening" stops
// at 11pm rather than running until 5.
func TestScreenGreetsYouForThePartOfTheDay(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(mavrkHellos)))
	ctx := tab(t)
	run(t, ctx, utc(), clockAt("2026-10-03T07:40:00Z"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	greet := func(skewMins int) string {
		t.Helper()
		run(t, ctx, chromedp.Evaluate(fmt.Sprintf(`window.__skew += %d * 60000; tick(); true`, skewMins), nil))
		return textOf(t, ctx, "#greet")
	}
	if g := greet(0); g != "Good morning, sir." {
		t.Fatalf("at 7:40: %q", g)
	}
	if g := greet(17*60 + 20); g != "Late one, sir." { // 1am
		t.Fatalf("at 1am: %q", g)
	}
	// What a wall screen gets: no hellos.
	d.handle("GET /screen", jsonH(screen(nil)))
	run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
	waitFor(t, ctx, `!data.hellos`, "the screen without hellos")
	for _, c := range []struct {
		skew int
		want string
	}{{0, "Hello"}, {6 * 60, "Good morning"}, {6 * 60, "Good afternoon"}, {6 * 60, "Good evening"}, {4 * 60, "Hello"}} { // 1am, 7am, 1pm, 7pm, 11pm
		if g := greet(c.skew); g != c.want {
			t.Errorf("plain greeting %q, want %q", g, c.want)
		}
	}
}

// Back at the wall screen after half an hour away, the character turns and
// waves once. Back again within the half hour, it doesn't wave again, nor
// for someone who was only away a few minutes. The twin's own work, or a
// message sent from away, wakes the screen without a wave: nobody came back.
func TestScreenWavesOnceWhenYouComeBack(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["ambient_after_seconds"] = 60 })))
	ctx := tab(t)
	run(t, ctx, utc(), clockAt("2026-10-03T09:00:00Z"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && document.body.classList.contains('idle')`, "the screen loaded")
	// the wave on arrival is over
	waitFor(t, ctx, `performance.now() > 2600 && !document.querySelector('#talkOrb').classList.contains('hi')`, "the arrival wave over")
	// A wave is the character taking the class 'hi' (penguin('hi', …)), counted
	// once for each time the page changes it.
	run(t, ctx, chromedp.Evaluate(`window.waves = 0;
new MutationObserver(ms => { if (ms[0].target.classList.contains('hi') && ms.some(m => !(m.oldValue || '').split(' ').includes('hi'))) waves++; })
  .observe(document.querySelector('#talkOrb'), {attributes: true, attributeFilter: ['class'], attributeOldValue: true}); true`, nil))
	away := func(expr string) {
		t.Helper()
		run(t, ctx, chromedp.Evaluate(expr+`; tick(); true`, nil))
		waitFor(t, ctx, `document.body.classList.contains('ambient')`, "the clock")
	}
	back := func() int {
		t.Helper()
		run(t, ctx, chromedp.Evaluate(`document.dispatchEvent(new PointerEvent('pointerdown', {bubbles: true})); true`, nil))
		if eval[bool](t, ctx, `document.body.classList.contains('ambient')`) {
			t.Fatal("still the clock after a touch")
		}
		return eval[int](t, ctx, `waves`)
	}
	away(`window.__skew += 31 * 60000`)
	if n := back(); n != 1 {
		t.Fatalf("%d waves after half an hour away, want 1", n)
	}
	if !eval[bool](t, ctx, `document.querySelector('#talkOrb').classList.contains('hi')`) {
		t.Fatal("no wave on the character")
	}
	away(`window.__skew += 5 * 60000`)
	if n := back(); n != 1 {
		t.Fatalf("waved after five minutes away (%d waves)", n)
	}
	// Quiet half an hour by the activity clock, but someone was here minutes ago.
	away(`lastActivity = Date.now() - 31 * 60000`)
	if n := back(); n != 1 {
		t.Fatalf("waved twice within 30 minutes (%d waves)", n)
	}
	// Half an hour on, it waves again.
	away(`window.__skew += 31 * 60000`)
	if n := back(); n != 2 {
		t.Fatalf("%d waves after another half hour away, want 2", n)
	}

	// Half an hour on, the twin's own work wakes the screen, and nobody is
	// waved at: a message from Telegram, then its thinking.
	woke := func(what string) {
		t.Helper()
		waitFor(t, ctx, `!document.body.classList.contains('ambient')`, what+" woke the screen")
		if n := eval[int](t, ctx, `waves`); n != 2 {
			t.Fatalf("waved at an empty room for %s (%d waves)", what, n)
		}
	}
	away(`window.__skew += 31 * 60000`)
	d.push(map[string]any{"kind": "heard", "text": "Running late, tell Priya", "data": map[string]any{"channel": "telegram"}})
	woke("a message from Telegram")
	away(`window.__skew += 31 * 60000; lastActivity = Date.now() - 31 * 60000`)
	run(t, ctx, chromedp.Evaluate(`setState('thinking'); true`, nil))
	woke("its thinking")
	run(t, ctx, chromedp.Evaluate(`setState('idle'); true`, nil))
	// Someone at this computer, by voice or by hand, is back: it waves.
	away(`lastActivity = Date.now() - 31 * 60000`)
	run(t, ctx, chromedp.Evaluate(`setState('listening'); setState('idle'); true`, nil))
	if n := eval[int](t, ctx, `waves`); n != 3 {
		t.Fatalf("%d waves for a voice at this computer after an hour away, want 3", n)
	}
	away(`window.__skew += 31 * 60000`)
	if n := back(); n != 4 {
		t.Fatalf("%d waves after another half hour away, want 4", n)
	}
}
