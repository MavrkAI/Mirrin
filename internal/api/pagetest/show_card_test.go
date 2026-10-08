package pagetest

import (
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// A long answer said out loud comes up in full over the screen: on the
// "show" event to a screen already open, and at /ui#show to one opened for
// it. Close and Esc put it away, and the floating character never shows it.
func TestALongAnswerComesUpInFull(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("GET /show", jsonH(map[string]any{"text": "Subject: **Friday**\n\nHi Sam, are we still on for eight?"}))
	ctx := tab(t)
	const up = `document.querySelector('#showCard').open`

	// opened for it
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui#show")))
	waitFor(t, ctx, up+` && document.querySelector('#showText').textContent.startsWith('Subject: Friday\n\nHi Sam')`, "the card at #show")
	if !eval[bool](t, ctx, `document.activeElement === document.querySelector('#showClose') && location.hash === ''`) {
		t.Fatal("Close doesn't have focus, or the address still says #show")
	}
	run(t, ctx, chromedp.Click("#showClose", chromedp.ByQuery))
	waitFor(t, ctx, `!`+up, "Close put it away")

	// already open: the event brings it up
	d.push(map[string]any{"kind": "show", "text": "Here's the shopping:\n- milk\n- eggs\n- bread"})
	waitFor(t, ctx, up+` && document.querySelector('#showText').textContent.includes('- bread')`, "the card on the event")
	run(t, ctx, chromedp.KeyEvent(kb.Escape))
	waitFor(t, ctx, `!`+up, "Esc put it away")

	// the floating character is no screen to read on
	orb := tab(t)
	run(t, orb, chromedp.Navigate(d.url("/ui?mode=orb#show")))
	run(t, orb, chromedp.Evaluate(`showAnswer('A long one')`, nil))
	if eval[bool](t, orb, up) {
		t.Fatal("the orb showed the card")
	}
}
