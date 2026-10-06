package pagetest

import (
	"context"
	"net/http"
	"testing"

	"github.com/chromedp/chromedp"
)

// welcomeRestored opens the welcome page with report as GET
// /welcome/restore/report answers it after a restore.
func welcomeRestored(t *testing.T, report map[string]any) context.Context {
	t.Helper()
	d := newDaemon(t)
	html := []byte(readPage(t, "../welcome.html"))
	d.handle("GET /welcome", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html)
	})
	d.handle("GET /welcome/brains", jsonH([]any{}))
	d.handle("GET /welcome/personas", jsonH(welcomeCast))
	d.handle("GET /voice/setup/status", jsonH(map[string]bool{"ready": false}))
	d.handle("GET /welcome/restore/report", jsonH(report))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/welcome")))
	waitFor(t, ctx, visible("#restored"), "the restore section")
	return ctx
}

// Moving in: the twin waves once and welcomes you back with what came
// along, then lists what is still to do here, each linking to its page,
// and says the old Mac stands by. The restore's own report is folded away.
func TestWelcomeBackAfterARestore(t *testing.T) {
	const line = "Welcome back, Akshay. Same Mirrin, new Mac: 214 things I remember, your morning briefing and 2 more routines, and 3 reminders came along."
	ctx := welcomeRestored(t, map[string]any{
		"line": line, "persona": "mirrin", "character": "maverick", "facts": 214, "reminders": 3,
		"routines": []string{"morning briefing", "weekly review", "reply like me"},
		"todo": []map[string]string{
			{"text": "Review your devices.", "url": "/restore/review"},
			{"text": "WhatsApp needs one scan.", "url": "/channels"},
			{"text": "Sign in to sites again in the browser; sign-ins stay on the old Mac."},
		},
		"stand_by": "Akshay's MacBook Pro stands by once it sees the move, and stays paused until you run `mirrin backup resume` there.",
		"detail":   "Your twin is home.\nThis snapshot is 3 days old.",
	})
	// Checked first: the hello plays for 1.6s, then the class goes.
	if !eval[bool](t, ctx, `document.querySelector('#backpic').classList.contains('hi')`) {
		t.Error("welcome back: Mirrin's hello didn't play")
	}
	for what, expr := range map[string]string{
		"the welcome":        `document.querySelector('#backline').textContent === ` + jsString(line),
		"Mirrin drawn":       `!!document.querySelector('#backpic.maverick #r0Suit')`,
		"still to do, first": `document.querySelector('#back').textContent.includes('Still to do here:')`,
		"each line":          `[...document.querySelectorAll('#todo li')].map(li => li.textContent).join('|') === 'Review your devices.|WhatsApp needs one scan.|Sign in to sites again in the browser; sign-ins stay on the old Mac.'`,
		"its page":           `[...document.querySelectorAll('#todo li')].map(li => (li.querySelector('a') || {}).pathname || '').join('|') === '/restore/review|/channels|'`,
		"the old Mac":        `document.querySelector('#standby').textContent.includes('mirrin backup resume') && !document.querySelector('#standby').hidden`,
		"the report folded":  `!document.querySelector('details').open && document.querySelector('#notes').textContent.includes('3 days old') && document.querySelector('#report').hidden`,
		"no second review":   `document.querySelector('#reviewlink').hidden`,
		"no model step":      `document.querySelector('#brain').hidden`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("welcome back: %s", what)
		}
	}
}

// When the restored twin couldn't be read, the restore's report shows as
// it always did, with the device review under it.
func TestWelcomeRestoreReportAlone(t *testing.T) {
	ctx := welcomeRestored(t, map[string]any{"facts": 0, "reminders": 0, "routines": nil, "todo": nil, "detail": "Your twin is home.\nAkshay's iPhone (0123abcd)"})
	for what, expr := range map[string]string{
		"the report":     `document.querySelector('#report').textContent === "Your twin is home.\nAkshay's iPhone (0123abcd)"`,
		"no welcome":     `document.querySelector('#back').hidden`,
		"the review":     `!document.querySelector('#reviewlink').hidden && document.querySelector('#reviewlink a').pathname === '/restore/review'`,
		"no model step":  `document.querySelector('#brain').hidden`,
		"restore again?": `document.querySelector('#moving').textContent === 'Restore another backup'`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("report alone: %s", what)
		}
	}
}
