package pagetest

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// A noted fact with a date still to come offers, beside its Undo, the
// reminder the twin would set: one tap sets it and the row says when it
// will come. An offer that has gone says what to do instead and goes, and a
// fact with no date offers nothing.
func TestNotedDateOffersAReminder(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("POST /screen/facts/7/remind", jsonH(map[string]string{"said": "I'll remind you on the 11th, and every year after."}))
	d.handle("POST /screen/facts/8/remind", refusedH(409, "no_offer", "I can't set that one from here now. Ask me to remind you and I will.", ""))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && es && es.readyState === 1`, "the screen loaded")
	voice := map[string]any{"channel": "voice"}
	turn := func(heard, said string, fact map[string]any) {
		d.push(map[string]any{"kind": "heard", "text": heard, "data": voice})
		d.push(map[string]any{"kind": "remembered", "text": fact["text"], "data": fact["data"]})
		d.push(map[string]any{"kind": "said", "text": said, "data": voice})
		waitFor(t, ctx, `(() => { const l = document.querySelector('#transcript .line.said:last-child'); return !!l && l.textContent.includes(`+jsString(said)+`) && !!l.querySelector('.noted'); })()`, "the reply with its noted row")
	}
	row := `#transcript .line.said:last-child .noted`
	turn("Mum's birthday is on the 12th.", "Lovely.", map[string]any{"text": "Mum's birthday is on the 12th.",
		"data": map[string]any{"id": 7, "subject": "family", "remind": "Remind me on the 11th?"}})
	if text := textOf(t, ctx, row); !strings.HasPrefix(text, "Noted: Mum's birthday is on the 12th · Undo · Remind me on the 11th?") {
		t.Fatalf("noted row reads %q", text)
	}
	run(t, ctx, chromedp.Click(row+" .remind", chromedp.ByQuery))
	d.waitCalled("POST /screen/facts/7/remind", 1)
	waitFor(t, ctx, `document.querySelector(`+jsString(row)+`).textContent.includes("Undo · I'll remind you on the 11th, and every year after.") && !document.querySelector(`+jsString(row+" .remind")+`)`, "the row says when it will come")

	turn("Dad's birthday is on the 3rd.", "Noted.", map[string]any{"text": "Dad's birthday is on the 3rd.",
		"data": map[string]any{"id": 8, "subject": "family", "remind": "Remind me on the 2nd?"}})
	run(t, ctx, chromedp.Click(row+" .remind", chromedp.ByQuery))
	d.waitCalled("POST /screen/facts/8/remind", 1)
	waitFor(t, ctx, `!document.querySelector(`+jsString(row+" .remind")+`) && document.body.textContent.includes('Ask me to remind you and I will.')`, "a gone offer says what to do and goes")

	turn("I take my coffee black.", "Got it.", map[string]any{"text": "Akshay takes his coffee black.", "data": map[string]any{"id": 9, "subject": "preferences"}})
	if text := textOf(t, ctx, row); text != "Noted: Akshay takes his coffee black · Undo" {
		t.Fatalf("a fact with no date reads %q", text)
	}
}
