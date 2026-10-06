package pagetest

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// aPortrait is a portrait on the screen, written at when, with its line on
// what's new.
func aPortrait(when string, mut func(m map[string]any)) map[string]any {
	return screen(func(m map[string]any) {
		m["portrait"] = strings.Repeat("You like quiet mornings and plain answers, and you keep Sundays for the family. ", 4) +
			"You'd rather be asked than guessed about."
		m["portrait_new"] = "you've been guarding Friday afternoons."
		m["portrait_at"] = when
		m["can"] = []string{"view", "chat", "approve"}
		if mut != nil {
			mut(m)
		}
	})
}

// reload fetches the screen again and waits until it is drawn.
func reload(t *testing.T, ctx context.Context) {
	t.Helper()
	run(t, ctx, chromedp.Evaluate(`window.__drawn = false; load().then(() => { window.__drawn = true }); true`, nil))
	waitFor(t, ctx, `window.__drawn === true`, "the screen redrawn")
}

// "How Mirrin sees you" shows the portrait in full and what's new this week,
// and asks whether it's right. Not quite opens the text box to say what's
// off; That's you puts the question away until the next portrait.
func TestThePortraitAsksWhetherItsRight(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(aPortrait("2026-09-27T08:00:00Z", nil)))
	d.handle("POST /screen/portrait/ack", jsonH(map[string]bool{"acked": true}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#portraitActs .right"), "the portrait's buttons")
	if got := textOf(t, ctx, "#portrait"); !strings.HasSuffix(got, "You'd rather be asked than guessed about.") {
		t.Fatalf("the portrait reads %q", got)
	}
	if clamp := eval[string](t, ctx, `getComputedStyle(document.querySelector('#portrait')).webkitLineClamp`); clamp != "none" {
		t.Fatalf("the portrait is cut to %s lines", clamp)
	}
	if got := textOf(t, ctx, "#portraitNew"); got != "New this week: you've been guarding Friday afternoons." {
		t.Fatalf("what's new reads %q", got)
	}
	if got := textOf(t, ctx, "#portraitActs"); got != "That's youNot quite" {
		t.Fatalf("the buttons read %q", got)
	}

	run(t, ctx, chromedp.Click("#portraitActs .notquite", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#composer').classList.contains('show')`, "the text box")
	if got := eval[string](t, ctx, `document.querySelector('#say').value`); got != "About how you see me: " {
		t.Fatalf("Not quite seeded %q", got)
	}
	if d.count("POST /screen/portrait/ack") != 0 {
		t.Fatal("Not quite said it was right")
	}

	run(t, ctx, chromedp.Click("#portraitActs .right", chromedp.ByQuery))
	d.waitCalled("POST /screen/portrait/ack", 1)
	waitFor(t, ctx, `document.querySelector('#portraitActs').hidden`, "the buttons put away")
	if !eval[bool](t, ctx, visible("#portrait")) {
		t.Fatal("That's you hid the portrait")
	}
	// They stay away on the next refresh, and when the twin says it was
	// answered.
	reload(t, ctx)
	if eval[bool](t, ctx, visible("#portraitActs")) {
		t.Fatal("the buttons came back on a refresh")
	}
	d.handle("GET /screen", jsonH(aPortrait("2026-09-27T08:00:00Z", func(m map[string]any) { m["portrait_ack"] = true })))
	reload(t, ctx)
	if eval[bool](t, ctx, visible("#portraitActs")) {
		t.Fatal("the buttons came back for the same portrait")
	}

	// The next portrait asks again; one with nothing new says nothing new.
	d.handle("GET /screen", jsonH(aPortrait("2026-10-04T08:00:00Z", func(m map[string]any) { delete(m, "portrait_new") })))
	reload(t, ctx)
	waitFor(t, ctx, visible("#portraitActs .right"), "the buttons for the next portrait")
	if eval[bool](t, ctx, visible("#portraitNew")) {
		t.Fatalf("a portrait with nothing new shows %q", textOf(t, ctx, "#portraitNew"))
	}
}

// A wall screen that only looks never shows the portrait (the API doesn't
// send it one), nor asks about it.
func TestAWallScreenShowsNoPortrait(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(aPortrait("2026-09-27T08:00:00Z", func(m map[string]any) { m["can"] = []string{"view"} })))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && document.body.classList.contains('nochat')`, "the screen loaded")
	if eval[bool](t, ctx, visible("#secPortrait")) || eval[bool](t, ctx, visible("#portraitActs")) {
		t.Fatal("a wall screen shows the portrait")
	}
	// The owner's own device, with the same portrait, does.
	d.handle("GET /screen", jsonH(aPortrait("2026-09-27T08:00:00Z", nil)))
	reload(t, ctx)
	waitFor(t, ctx, visible("#secPortrait")+` && `+visible("#portraitActs .right"), "the portrait on the owner's device")
}
