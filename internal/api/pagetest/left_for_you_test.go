package pagetest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// leftDay is what the twin left on 3 October: the morning briefing at 7:00
// and a reminder at 8:15.
func leftDay(m map[string]any) {
	m["left_for_you"] = []any{
		map[string]any{"id": "b1", "source": "protocol", "title": "Morning briefing", "briefing": true, "at": "2026-10-03T07:00:00Z",
			"text": "Good morning.\nDentist at 3.\nBins out tonight."},
		map[string]any{"id": "r1", "source": "reminder", "title": "Reminder", "at": "2026-10-03T08:15:00Z", "text": "Reminder: call the plumber"},
	}
}

// at makes the page's clock read now (an RFC 3339 time), still ticking, and
// starts it with seen as the ids this device has already shown.
func at(now string, seen ...string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		ids := "[]"
		if len(seen) > 0 {
			ids = `["` + strings.Join(seen, `","`) + `"]`
		}
		_, err := page.AddScriptToEvaluateOnNewDocument(fmt.Sprintf(`(() => {
  const Real = Date, off = new Real(%q).getTime() - Real.now();
  class Fake extends Real { constructor(...a) { if (a.length) super(...a); else super(Real.now() + off); } static now() { return Real.now() + off; } }
  window.Date = Fake;
  localStorage.setItem('mirrin.left.seen', %q);
})()`, now, ids)).Do(ctx)
		return err
	})
}

// leftCards is each card under Left for you, as "head | text" ("" when
// folded).
const leftCards = `[...document.querySelectorAll('#lefts .left')].map(c => c.querySelector('.lh').textContent + ' | ' + (c.querySelector('p') ? c.querySelector('p').innerText : '')).join('\n')`

// At 9:30 the briefing and the reminder are under Needs you's neighbour,
// newest first, each with its title, time and words; the section has no
// count, badge or dot.
func TestLeftForYouShowsWhatItDidOnItsOwn(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(leftDay)))
	ctx := tab(t)
	run(t, ctx, utc(), at("2026-10-03T09:30:00Z"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#secLeft"), "Left for you")
	want := "Reminder · 8:15 AM | Reminder: call the plumber\nMorning briefing · 7:00 AM | Good morning.\nDentist at 3.\nBins out tonight."
	if got := eval[string](t, ctx, leftCards); got != want {
		t.Fatalf("cards:\n%s\nwant:\n%s", got, want)
	}
	if !eval[bool](t, ctx, `document.querySelector('#secNeeds').nextElementSibling === document.querySelector('#secLeft')`) {
		t.Error("Left for you isn't right after Needs you")
	}
	if h := eval[string](t, ctx, `document.querySelector('#hLeft').textContent`); h != "Left for you" {
		t.Errorf("the heading reads %q", h)
	}
	if eval[bool](t, ctx, `!!document.querySelector('#secLeft .n, #secLeft .dot, #secLeft .badge') || /\d/.test(document.querySelector('#hLeft').textContent)`) {
		t.Error("Left for you has a count, badge or dot")
	}
	if eval[bool](t, ctx, `!!document.querySelector('#secNotices')`) {
		t.Error("the Noticed section is still there")
	}
	// Both were shown open to someone here: this device has seen them.
	if got := eval[string](t, ctx, `localStorage.getItem('mirrin.left.seen')`); !strings.Contains(got, `"r1"`) || !strings.Contains(got, `"b1"`) {
		t.Errorf("seen %s", got)
	}
}

// One this device has seen folds to its title and time, and opens when
// tapped; the morning briefing stays open until noon.
func TestLeftForYouFoldsOnceSeen(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(leftDay)))

	ctx := tab(t)
	run(t, ctx, utc(), at("2026-10-03T09:30:00Z", "b1", "r1"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#secLeft"), "Left for you")
	want := "Reminder · 8:15 AM | \nMorning briefing · 7:00 AM | Good morning.\nDentist at 3.\nBins out tonight."
	if got := eval[string](t, ctx, leftCards); got != want {
		t.Fatalf("before noon:\n%s\nwant:\n%s", got, want)
	}
	if got := eval[string](t, ctx, `document.querySelector('#lefts .left.fold .lh').getAttribute('aria-expanded')`); got != "false" {
		t.Fatalf("the folded row says expanded=%s", got)
	}
	run(t, ctx, chromedp.Click(`#lefts .left.fold .lh`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#lefts .left.fold p') && document.querySelector('#lefts .left.fold p').innerText === 'Reminder: call the plumber'`, "the reminder opened on a tap")
	if !eval[bool](t, ctx, `document.activeElement === document.querySelector('#lefts .left.fold .lh') && document.activeElement.getAttribute('aria-expanded') === 'true'`) {
		t.Error("the opened row lost focus, or doesn't say it is open")
	}

	ctx = tab(t)
	run(t, ctx, utc(), at("2026-10-03T12:30:00Z", "b1", "r1"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#secLeft"), "Left for you")
	if got := eval[string](t, ctx, leftCards); got != "Reminder · 8:15 AM | \nMorning briefing · 7:00 AM | " {
		t.Fatalf("after noon:\n%s", got)
	}
}

// Nothing left: no section at all. A wall screen gets titles only, and
// shows them as lines.
func TestLeftForYouHiddenWhenEmptyAndTitlesOnlyOnAWall(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["left_for_you"] = []any{} })))
	ctx := tab(t)
	run(t, ctx, utc(), at("2026-10-03T09:30:00Z"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	if !eval[bool](t, ctx, `document.querySelector('#secLeft').hidden`) {
		t.Fatal("an empty Left for you is shown")
	}

	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["can"] = []string{"view"}
		m["left_for_you"] = []any{map[string]any{"id": "w1", "source": "watch", "title": "Inbox", "at": "2026-10-03T08:00:00Z"}}
	})))
	run(t, ctx, chromedp.Evaluate(`load()`, nil))
	waitFor(t, ctx, visible("#secLeft"), "Left for you on a wall")
	if got := eval[string](t, ctx, leftCards); got != "Inbox · 8:00 AM | " {
		t.Fatalf("on a wall: %q", got)
	}
	if eval[bool](t, ctx, `!!document.querySelector('#lefts button')`) {
		t.Error("a title-only line can be tapped open")
	}
}

// A message arriving says its words under the character and refreshes the
// screen, with no toast.
func TestAMessageIsSaidWithoutAToast(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	d.waitCalled("GET /events", 1)
	before := d.count("GET /screen")
	d.push(map[string]any{"kind": "message", "text": "Good morning. Dentist at 3.", "data": map[string]any{"id": "b1", "title": "Morning briefing"}})
	waitFor(t, ctx, `document.querySelector('#caption').textContent === 'Good morning. Dentist at 3.'`, "the briefing under the character")
	d.waitCalled("GET /screen", before+1)
	if got := textOf(t, ctx, "#toasts"); got != "" {
		t.Fatalf("a message came with a toast: %q", got)
	}
}
