package pagetest

import (
	"testing"

	"github.com/chromedp/chromedp"
)

// A fact forgotten one way is gone the other way too: Forget under Why?
// leaves its Noted row saying Forgotten, with no Undo or Remind, and Undo
// leaves its Why? entry saying Forgotten, with no Forget.
func TestNotedAndWhyForgetTogether(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("POST /memory/why", jsonH(map[string]any{"known": true, "lead": "These matched most closely:",
		"facts": []map[string]any{{"id": 7, "subject": "family", "content": "Mum's birthday is on the 12th."}, {"id": 8, "subject": "family", "content": "Dad's birthday is on the 3rd."}}}))
	d.handle("DELETE /memory/facts/7", jsonH(map[string]bool{"ok": true}))
	d.handle("POST /screen/facts/8/undo", jsonH(map[string]bool{"ok": true}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && es && es.readyState === 1`, "the screen loaded")
	voice := map[string]any{"channel": "voice"}
	turn := func(heard, said string, fact map[string]any) {
		d.push(map[string]any{"kind": "heard", "text": heard, "data": voice})
		d.push(map[string]any{"kind": "remembered", "text": fact["text"], "data": fact["data"]})
		d.push(map[string]any{"kind": "said", "text": said, "data": voice})
		waitFor(t, ctx, `(() => { const l = document.querySelector('#transcript .line.said:last-child'); return !!l && l.textContent.includes(`+jsString(said)+`) && !!l.querySelector('.noted') && !!l.querySelector('.why'); })()`, "the reply with its noted row and Why?")
	}
	turn("Mum's birthday is on the 12th.", "Lovely.", map[string]any{"text": "Mum's birthday is on the 12th.",
		"data": map[string]any{"id": 7, "subject": "family", "remind": "Remind me on the 11th?"}})
	first := `#transcript .line.said:last-child`
	run(t, ctx, chromedp.Evaluate(`document.querySelector(`+jsString(first+" .why")+`).click(); true`, nil))
	d.waitCalled("POST /memory/why", 1)
	waitFor(t, ctx, `document.querySelectorAll(`+jsString(first+" .whybox li .forget")+`).length === 2`, "Why? lists both facts")

	// Forget under Why? and the Noted row has nothing left to offer.
	run(t, ctx, chromedp.Evaluate(`document.querySelector(`+jsString(first+" .whybox li .forget")+`).click(); true`, nil))
	d.waitCalled("DELETE /memory/facts/7", 1)
	waitFor(t, ctx, `(() => { const r = document.querySelector(`+jsString(first+" .noted")+`); return r.textContent.startsWith('Forgotten.') && !r.querySelector('.undo') && !r.querySelector('.remind'); })()`, "the Noted row says Forgotten, with no Undo or Remind")

	// Undo on a later row and the open Why? box says it's gone.
	turn("Dad's birthday is on the 3rd.", "Noted.", map[string]any{"text": "Dad's birthday is on the 3rd.",
		"data": map[string]any{"id": 8, "subject": "family"}})
	run(t, ctx, chromedp.Evaluate(`document.querySelector('#transcript .line.said:last-child .noted .undo').click(); true`, nil))
	d.waitCalled("POST /screen/facts/8/undo", 1)
	waitFor(t, ctx, `(() => { const ls = [...document.querySelectorAll('#transcript .whybox li')]; return ls.length === 2 && ls.every(l => l.textContent === 'Forgotten.') && !document.querySelector('#transcript .whybox .forget'); })()`, "the Why? entry says Forgotten, with no Forget")
}
