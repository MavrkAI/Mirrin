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
)

// The conversation stays readable while it re-renders: every streamed word
// used to replay each line's fade-in, so it flickered and stayed faint.
func TestConversationStaysVisibleWhileItRedraws(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(busyDay()))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	// Redraw every 50 ms, as a streaming reply does, and look mid-stream.
	run(t, ctx, chromedp.Evaluate(`window.__redraw = setInterval(() => renderTranscript(), 50)`, nil), chromedp.Sleep(900*time.Millisecond))
	op := eval[float64](t, ctx, `Math.min(...[...document.querySelectorAll('#transcript .line')].map(l => +getComputedStyle(l).opacity))`)
	if op < 1 {
		t.Fatalf("a line is at opacity %.2f while the conversation redraws", op)
	}
}

// Approval cards say what will happen in words, not tool names.
func TestApprovalCardsUsePlainWords(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(busyDay()))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `document.querySelectorAll('#needs .need').length > 0`, "the requests")
	got := textOf(t, ctx, "#needs")
	if !strings.Contains(got, "Send an email") || !strings.Contains(got, "Changes something") {
		t.Fatalf("the card reads %q", got)
	}
	if strings.Contains(got, "send_email") || strings.Contains(got, "write") {
		t.Fatalf("tool jargon on the card: %q", got)
	}
}

// A task's question is answered on its card, and the answer names the task.
func TestATaskQuestionIsAnsweredOnItsCard(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(busyDay()))
	var mu sync.Mutex
	var sent string
	d.handle("POST /message/stream", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct{ Text string }
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		sent = m.Text
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: done\ndata: {\"reply\":\"Booked the comfortable one.\"}\n\n")
	})
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!!document.querySelector('#needs form.answer input')`, "the answer box")
	run(t, ctx, chromedp.SendKeys("#needs form.answer input", "the comfortable one", chromedp.ByQuery),
		chromedp.Click("#needs form.answer button", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes('Booked the comfortable one')`, "the reply")
	mu.Lock()
	defer mu.Unlock()
	if sent != `About "Plan the Bali trip": the comfortable one` {
		t.Fatalf("sent %q", sent)
	}
}

// connected says what the twin can reach, as /screen does.
func connected(calendar bool) func(m map[string]any) {
	return func(m map[string]any) {
		m["connected"] = map[string]any{"calendar": calendar, "mail": false, "voice": false}
	}
}

// chips is the suggestions under the character, as one line.
const chips = `[...document.querySelectorAll('#suggest button')].map(b => b.textContent).join(' | ')`

// The first suggestion is one that works with what is connected: with a
// calendar, what's on tomorrow; with nothing, a reminder in ten minutes,
// asked as soon as it is tapped.
func TestTheFirstChipsAlwaysWork(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(connected(true))))
	var mu sync.Mutex
	var sent string
	d.handle("POST /message/stream", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct{ Text string }
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		sent = m.Text
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: done\ndata: {\"reply\":\"Ten minutes. I'll find you.\"}\n\n")
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && !document.querySelector('#suggest').hidden`, "the suggestions")
	if got := eval[string](t, ctx, chips); got != "What's on tomorrow? | What can you do for me? | Remind me to…" {
		t.Fatalf("with a calendar: %q", got)
	}

	d.handle("GET /screen", jsonH(screen(connected(false))))
	run(t, ctx, chromedp.Evaluate(`load()`, nil))
	waitFor(t, ctx, `document.querySelector('#suggest button').textContent === 'Remind me in 10 minutes to stretch'`, "the chips with no calendar")
	if got := eval[string](t, ctx, chips); got != "Remind me in 10 minutes to stretch | What can you do for me? | Remind me to…" {
		t.Fatalf("with nothing connected: %q", got)
	}
	run(t, ctx, chromedp.Click(`#suggest button`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes("Ten minutes. I'll find you.")`, "the reply")
	mu.Lock()
	defer mu.Unlock()
	if sent != "Remind me in 10 minutes to stretch" {
		t.Fatalf("sent %q", sent)
	}
}

// With no calendar connected, an empty day says so, with a way to connect
// one on this computer, rather than "Nothing on your calendar today.".
func TestAnEmptyDaySaysTheCalendarIsntConnected(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(connected(false))))
	ctx := tab(t)
	run(t, ctx, utc(), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	if e := textOf(t, ctx, "#day .empty"); e != "Calendar not connected · Connect" {
		t.Fatalf("the empty day reads %q", e)
	}
	if !eval[bool](t, ctx, `document.querySelector('#day .empty a[href="/accounts"]').textContent === 'Connect'`) {
		t.Fatal("no way to connect a calendar")
	}

	// A calendar that is connected and empty is a free day.
	d.handle("GET /screen", jsonH(screen(connected(true))))
	run(t, ctx, chromedp.Evaluate(`load()`, nil))
	waitFor(t, ctx, `document.querySelector('#day .empty').textContent === 'Nothing on your calendar today.'`, "a free day")
}

// A tip under Left for you has Thanks, which folds it, and No more tips,
// which turns the tips off and leaves the card saying so.
func TestNoMoreTipsReplacesTheCard(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["left_for_you"] = []any{map[string]any{"id": "tip1", "source": "tip", "title": "From Mirrin", "at": "2026-10-03T09:30:00Z",
			"text": "I can run a briefing at 7 if you'd like one."}}
	})))
	d.handle("POST /screen/tips/off", jsonH(map[string]bool{"off": true}))
	ctx := tab(t)
	run(t, ctx, utc(), at("2026-10-03T10:00:00Z"), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible("#lefts .tipacts .thanks"), "the tip's buttons")
	if got := textOf(t, ctx, "#lefts .tipacts"); got != "ThanksNo more tips" {
		t.Fatalf("the tip's buttons read %q", got)
	}

	run(t, ctx, chromedp.Click(`#lefts .tipacts .thanks`, chromedp.ByQuery))
	waitFor(t, ctx, `!!document.querySelector('#lefts .left.fold') && !document.querySelector('#lefts .left p')`, "the tip folded")
	if got := textOf(t, ctx, "#lefts .left .lh"); got != "From Mirrin · 9:30 AM" {
		t.Fatalf("the folded tip reads %q", got)
	}
	if d.count("POST /screen/tips/off") != 0 {
		t.Fatal("Thanks turned the tips off")
	}

	run(t, ctx, chromedp.Click(`#lefts .left.fold .lh`, chromedp.ByQuery))
	waitFor(t, ctx, visible("#lefts .tipacts .notips"), "No more tips on the opened tip")
	run(t, ctx, chromedp.Click(`#lefts .tipacts .notips`, chromedp.ByQuery))
	d.waitCalled("POST /screen/tips/off", 1)
	waitFor(t, ctx, `document.querySelector('#lefts .left').textContent === 'No more tips. Ask me anything, any time.'`, "the card saying so")
	if eval[bool](t, ctx, `!!document.querySelector('#lefts button')`) {
		t.Error("the card still has buttons")
	}
}
