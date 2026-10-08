package pagetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

const loaded = `typeof data !== 'undefined' && data !== null`

// On a phone the text box is on screen from the start, and big enough that
// iOS doesn't zoom the page when it is tapped.
func TestPhoneKeepsTheTextBoxInReach(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, phone(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	type box struct {
		Visible bool    `json:"visible"`
		Font    float64 `json:"font"`
		Top     float64 `json:"top"`
		Bottom  float64 `json:"bottom"`
		H       float64 `json:"h"`
	}
	got := eval[box](t, ctx, `(() => { const i = document.querySelector('#say'), r = i.getBoundingClientRect(), cs = getComputedStyle(i);
		return {visible: r.width > 0 && r.height > 0 && cs.visibility !== 'hidden', font: parseFloat(cs.fontSize), top: r.top, bottom: r.bottom, h: innerHeight}; })()`)
	if !got.Visible {
		t.Fatal("the text box is hidden on a phone until a key is pressed")
	}
	if got.Top < 0 || got.Bottom > got.H {
		t.Fatalf("the text box is off screen: top %.0f bottom %.0f of %.0f", got.Top, got.Bottom, got.H)
	}
	if got.Font < 16 {
		t.Fatalf("the text box is %.1fpx; under 16px iOS zooms the page on focus", got.Font)
	}
}

// A small wall display with no touch screen stays a wall: no text box until
// someone types.
func TestSmallWallStaysAWall(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(800, 480), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	if eval[bool](t, ctx, visible("#say")) {
		t.Fatal("a text box sits on an 800×480 wall display nobody has typed at")
	}
	run(t, ctx, chromedp.KeyEvent("h"))
	waitFor(t, ctx, visible("#say"), "typing shows the text box")
}

// A long reply shows in full on a phone, can be selected, and has a Copy button.
func TestPhoneShowsTheWholeReplyAndCopiesIt(t *testing.T) {
	long := strings.Repeat("The dentist moved your appointment to the new clinic. ", 10) + "New address: 42 Harbour Street, Level 3."
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("POST /message/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		half := len(long) / 2
		fmt.Fprintf(w, "event: delta\ndata: {\"text\":%s}\n\n", jsString(long[:half]))
		fmt.Fprintf(w, "event: delta\ndata: {\"text\":%s}\n\n", jsString(long[half:]))
		fmt.Fprintf(w, "event: done\ndata: {\"reply\":%s}\n\n", jsString(long))
	})
	ctx := tab(t)
	run(t, ctx, phone(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	run(t, ctx, chromedp.SendKeys("#say", "where is the dentist now?"+kb.Enter, chromedp.ByQuery))
	waitFor(t, ctx, `(() => { const l = [...document.querySelectorAll('#transcript .line.said')].pop(); return !!l && !l.classList.contains('live') && l.textContent.includes('Harbour Street'); })()`, "the reply arrived")
	type shape struct {
		Clamped bool   `json:"clamped"`
		Shown   bool   `json:"shown"`
		Select  string `json:"select"`
	}
	got := eval[shape](t, ctx, `(() => { const tx = [...document.querySelectorAll('#transcript .line.said .tx')].pop(), cs = getComputedStyle(tx);
		return {clamped: tx.scrollHeight > tx.clientHeight + 2, shown: tx.offsetHeight > 0, select: cs.userSelect || cs.webkitUserSelect}; })()`)
	if !got.Shown || got.Clamped {
		t.Fatalf("the reply is cut off on a phone: %+v", got)
	}
	if got.Select != "text" {
		t.Fatalf("the reply can't be selected: user-select %q", got.Select)
	}
	run(t, ctx, chromedp.Evaluate(`navigator.clipboard.writeText = t => { window.__copied = t; return Promise.resolve(); }; true`, nil))
	run(t, ctx, chromedp.Click(`#transcript .line.said:last-child .copy`, chromedp.ByQuery))
	waitFor(t, ctx, `window.__copied === `+jsString(long), "Copy put the whole reply on the clipboard")
	waitFor(t, ctx, `document.querySelector('#toasts').innerText.includes('Copied')`, "Copy said so")
}

// On a phone the transcript shows the last exchange, and the rest is one tap away.
func TestPhoneTranscriptShowsEarlierLines(t *testing.T) {
	var recent []any
	for i := 1; i <= 6; i++ {
		kind := "heard"
		if i%2 == 0 {
			kind = "said"
		}
		recent = append(recent, map[string]any{"kind": kind, "text": fmt.Sprintf("line %d", i), "at": time.Now().Add(time.Duration(i-10) * time.Minute)})
	}
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["recent"] = recent })))
	ctx := tab(t)
	run(t, ctx, phone(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	shown := `[...document.querySelectorAll('#transcript .line')].filter(l => l.offsetHeight > 0).length`
	if n := eval[int](t, ctx, shown); n != 2 {
		t.Fatalf("%d lines shown before expanding, want the last 2", n)
	}
	waitFor(t, ctx, visible("#txMore"), "a Show earlier button")
	run(t, ctx, chromedp.Click("#txMore", chromedp.ByQuery))
	if n := eval[int](t, ctx, shown); n != 6 {
		t.Fatalf("%d lines shown after expanding, want 6", n)
	}
	if e := eval[string](t, ctx, `document.querySelector('#txMore').getAttribute('aria-expanded')`); e != "true" {
		t.Fatalf("aria-expanded %q after expanding", e)
	}
}

// A device the daemon doesn't know is told so, with the next step, instead of
// "Reconnecting" forever. A daemon that is merely down still says Reconnecting.
func TestScreenSaysWhenThisDeviceIsNotPaired(t *testing.T) {
	t.Run("not_paired", func(t *testing.T) {
		d := newDaemon(t)
		hint := "On your Mac, open Add your phone… and scan the code."
		d.handle("GET /screen", notPairedH(hint))
		d.handle("GET /events", notPairedH(hint))
		ctx := tab(t)
		run(t, ctx, phone(), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, visible("#unpaired"), "the not-paired notice")
		text := textOf(t, ctx, "#unpaired")
		if !strings.Contains(text, "isn't paired") || !strings.Contains(text, hint) {
			t.Fatalf("notice reads %q", text)
		}
		if pill := textOf(t, ctx, "#state"); strings.Contains(pill, "Reconnecting") {
			t.Fatalf("pill still says %q", pill)
		}
		if f := eval[string](t, ctx, `document.activeElement ? document.activeElement.id : ''`); f != "unpairedRetry" {
			t.Fatalf("focus is on %q, not the Try again button", f)
		}
		// Paired meanwhile (another tab, a new code): Try again brings the screen back.
		d.handle("GET /screen", jsonH(screen(nil)))
		d.handle("GET /events", d.events)
		run(t, ctx, chromedp.Click("#unpairedRetry", chromedp.ByQuery))
		waitFor(t, ctx, `document.querySelector('#unpaired').hidden`, "the notice went away once paired")
		waitFor(t, ctx, `document.querySelector('#state').textContent === 'Idle'`, "the pill recovered")
	})
	t.Run("plain 401", func(t *testing.T) {
		d := newDaemon(t)
		d.handle("GET /screen", textH(401, "unauthorized"))
		d.handle("GET /events", textH(401, "unauthorized"))
		ctx := tab(t)
		run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, visible("#unpaired"), "the not-paired notice")
		if text := textOf(t, ctx, "#unpaired"); !strings.Contains(text, "menu bar") {
			t.Fatalf("a plain 401 gives no next step: %q", text)
		}
	})
	t.Run("daemon down", func(t *testing.T) {
		d := newDaemon(t)
		d.handle("GET /screen", textH(503, "starting"))
		d.handle("GET /events", textH(503, "starting"))
		ctx := tab(t)
		run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, `document.querySelector('#state').textContent.includes('Reconnecting')`, "Reconnecting")
		if eval[bool](t, ctx, visible("#unpaired")) {
			t.Fatal("a daemon that is down is not a pairing problem")
		}
	})
}

// "Today" holds the next three days; each later day is named, and the header
// and ambient lines say which day, not just a time.
func TestTodayNamesTheDay(t *testing.T) {
	now := time.Now().UTC()
	day := func(n, h int) time.Time {
		y, m, dd := now.AddDate(0, 0, n).Date()
		return time.Date(y, m, dd, h, 0, 0, 0, time.UTC)
	}
	tomorrow, later := day(1, 9), day(2, 10)
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["events"] = []any{
			map[string]any{"title": "Dentist", "start": tomorrow, "end": tomorrow.Add(time.Hour), "all_day": false},
			map[string]any{"title": "Flight to Tokyo", "start": later, "end": later.Add(2 * time.Hour), "all_day": false},
			map[string]any{"title": "Mum's birthday", "start": day(2, 0), "end": day(3, 0), "all_day": true},
		}
	})))
	ctx := tab(t)
	run(t, ctx, utc(), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	text := textOf(t, ctx, "#day")
	wd := later.Weekday().String()
	for _, want := range []string{"Tomorrow", wd, "Dentist", "Flight to Tokyo", "Mum's birthday"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Today reads %q; missing %q", text, want)
		}
	}
	if strings.Index(text, "Tomorrow") > strings.Index(text, "Dentist") || strings.Index(text, wd) > strings.Index(text, "Flight to Tokyo") {
		t.Fatalf("the day labels are not above their events: %q", text)
	}
	if h := textOf(t, ctx, "#hToday"); strings.EqualFold(strings.TrimSpace(h), "Today") {
		t.Fatalf("the section is still titled %q with nothing today", h)
	}
	if q := textOf(t, ctx, "#quiet"); !strings.Contains(q, "tomorrow") {
		t.Fatalf("the header's next line gives no day: %q", q)
	}
	if a := eval[string](t, ctx, `document.querySelector('#ambsub').textContent`); !strings.Contains(a, "tomorrow") {
		t.Fatalf("ambient gives no day: %q", a)
	}
	// nothing has happened today, so there is no "else"
	if e := textOf(t, ctx, "#day .empty"); e != "Nothing today." {
		t.Fatalf("the empty day reads %q", e)
	}
	// the next row sits under its day heading and beside its time; its sub-line doesn't repeat them
	if sub := eval[string](t, ctx, `(() => { const s = document.querySelector('#day .row.next .sub'); return s ? s.textContent : ''; })()`); strings.Contains(sub, "tomorrow") || strings.Contains(sub, "9:00") {
		t.Fatalf("the next row's sub-line repeats the day and time: %q", sub)
	}
	labels := eval[[]string](t, ctx, `[
		dayLabel(new Date(Date.UTC(2026,8,27,23,0)), new Date(Date.UTC(2026,8,27,8,0))),
		dayLabel(new Date(Date.UTC(2026,8,28,9,0)), new Date(Date.UTC(2026,8,27,23,59))),
		dayLabel(new Date(Date.UTC(2026,8,29,9,0)), new Date(Date.UTC(2026,8,27,23,59))),
	]`)
	if strings.Join(labels, ",") != "Today,Tomorrow,Tuesday" {
		t.Fatalf("dayLabel gave %v", labels)
	}
}

// When the calendar can't be read the screen says so, instead of an empty day
// that looks like the truth. (calendar_error is optional; see the deferred note.)
func TestTodaySaysWhenTheCalendarFailed(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { m["calendar_error"] = "Google sign-in expired." })))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	text := textOf(t, ctx, "#day")
	if !strings.Contains(text, "Couldn't read your calendar") || !strings.Contains(text, "Google sign-in expired.") || strings.Contains(text, "Nothing on the board") {
		t.Fatalf("Today reads %q", text)
	}
	if !eval[bool](t, ctx, `!!document.querySelector('#day a[href="/accounts"]')`) {
		t.Fatal("no way to the Accounts page")
	}
}

func oneApproval(m map[string]any) {
	m["approvals"] = []any{map[string]any{"id": 7, "summary": "Send the invoice to Sam", "tool": "gmail_send", "chat": "telegram:1", "created_at": time.Now().Format(time.RFC3339)}}
	m["tasks"] = []any{map[string]any{"id": "t1", "title": "Plan the trip", "status": "running", "goal": "Tokyo in May",
		"steps": []any{map[string]any{"text": "Find flights", "done": true}, map[string]any{"text": "Book a hotel"}}}}
}

// Space on a focused Approve presses it; it used to open the text box instead.
func TestSpaceOnAFocusedButtonPressesIt(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(oneApproval)))
	d.handle("POST /approvals/7/approve", jsonH(map[string]string{"reply": "Sent."}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible(".need .yes"), "the approval card")
	run(t, ctx, chromedp.Focus(".need .yes", chromedp.ByQuery), chromedp.KeyEvent(" "))
	d.waitCalled("POST /approvals/7/approve", 1)
	if eval[bool](t, ctx, `document.querySelector('#composer').classList.contains('show') || document.activeElement === document.querySelector('#say')`) {
		t.Fatal("Space opened the text box")
	}
	// A task card's Details button works from the keyboard too, without the text box.
	run(t, ctx, chromedp.Focus(".task .toggle", chromedp.ByQuery), chromedp.KeyEvent(" "))
	waitFor(t, ctx, `document.querySelector('.task').classList.contains('open') && document.querySelector('.task .toggle').getAttribute('aria-expanded') === 'true'`, "the task opened")
	if eval[bool](t, ctx, `document.querySelector('#composer').classList.contains('show')`) {
		t.Fatal("Space on a task opened the text box")
	}
	// Typing a letter anywhere still starts a message. (chromedp delivers the
	// character separately from the key, so it may land twice here; a browser
	// drops it once the key is handled.)
	run(t, ctx, chromedp.Evaluate(`document.activeElement.blur(); true`, nil), chromedp.KeyEvent("h"))
	waitFor(t, ctx, `document.querySelector('#composer').classList.contains('show') && /^h+$/.test(document.querySelector('#say').value) && document.activeElement === document.querySelector('#say')`, "typing opened the text box")
}

// With reduced motion asked for, nothing loops and nothing slides.
func TestReducedMotionStopsAnimation(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) { oneApproval(m); m["state"] = "thinking" })))
	ctx := tab(t)
	run(t, ctx, reducedMotion(), desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	run(t, ctx, chromedp.Evaluate(`pushLine('said', 'working on it', null, {live: true}); toast('hello'); true`, nil))
	bad := eval[[]string](t, ctx, `(() => { const bad = [];
		for (const el of document.querySelectorAll('*')) for (const p of [null, '::before', '::after']) {
			const cs = getComputedStyle(el, p);
			if (!cs.animationName || cs.animationName === 'none') continue;
			const dur = Math.max(...cs.animationDuration.split(',').map(parseFloat));
			if (cs.animationIterationCount.includes('infinite') || dur > 0.05) bad.push((el.id || el.className || el.tagName) + (p || '') + ': ' + cs.animationName + ' ' + cs.animationDuration + ' x' + cs.animationIterationCount);
		}
		return bad; })()`)
	if len(bad) > 0 {
		t.Fatalf("still animating with reduced motion:\n%s", strings.Join(bad, "\n"))
	}
}

// Approving from the screen shows the twin's reply once, although it arrives
// both as the answer and on the live feed.
func TestApprovalReplyIsShownOnce(t *testing.T) {
	for _, feedFirst := range []bool{true, false} {
		name := map[bool]string{true: "feed first", false: "answer first"}[feedFirst]
		t.Run(name, func(t *testing.T) {
			d := newDaemon(t)
			d.handle("GET /screen", jsonH(screen(oneApproval)))
			d.handle("POST /approvals/7/approve", func(w http.ResponseWriter, r *http.Request) {
				said := map[string]any{"kind": "said", "text": "Sent to Sam.", "at": time.Now()}
				if feedFirst {
					d.push(said)
					time.Sleep(150 * time.Millisecond)
				} else {
					go func() { time.Sleep(200 * time.Millisecond); d.push(said) }()
				}
				jsonH(map[string]string{"reply": "Sent to Sam."})(w, r)
			})
			ctx := tab(t)
			run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
			waitFor(t, ctx, visible(".need .yes"), "the approval card")
			waitFor(t, ctx, `(() => { try { return es && es.readyState === 1; } catch { return false; } })() || document.querySelector('#state').textContent === 'Idle'`, "the live feed is open")
			time.Sleep(300 * time.Millisecond)
			run(t, ctx, chromedp.Click(".need .yes", chromedp.ByQuery))
			count := `[...document.querySelectorAll('#transcript .line.said')].filter(l => l.textContent.includes('Sent to Sam.')).length`
			waitFor(t, ctx, count+` >= 1`, "the reply appeared")
			time.Sleep(600 * time.Millisecond)
			if n := eval[int](t, ctx, count); n != 1 {
				t.Fatalf("the reply is in the transcript %d times", n)
			}
			// the next turn can end in the same words; that is a new line
			d.push(map[string]any{"kind": "heard", "text": "did it go?", "at": time.Now()})
			d.push(map[string]any{"kind": "said", "text": "Sent to Sam.", "at": time.Now()})
			waitFor(t, ctx, count+` === 2`, "the next turn's reply")
		})
	}
}

// Two turns that get the same short reply show both replies. The approval
// dedupe used to swallow the second "Done.".
func TestSameReplyTwiceShowsTwice(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && es && es.readyState === 1`, "the live feed is open")
	for _, q := range []string{"turn off the lights", "lock the door"} {
		d.push(map[string]any{"kind": "heard", "text": q, "at": time.Now()})
		d.push(map[string]any{"kind": "said", "text": "Done.", "at": time.Now()})
	}
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes('lock the door')`, "the second question")
	count := `[...document.querySelectorAll('#transcript .line.said')].filter(l => l.querySelector('.tx').textContent === 'Done.').length`
	waitFor(t, ctx, count+` === 2`, "both replies")
}

// A long older line that is cut off opens from the keyboard, and the
// transcript's name counts for screen readers.
func TestOlderLinesOpenFromTheKeyboard(t *testing.T) {
	long := strings.Repeat("Your flight on Friday moved from gate 12 to gate 31, boarding is now at 9:40. ", 8)
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["recent"] = []any{
			map[string]any{"kind": "heard", "text": "any news on the flight?", "at": time.Now().Add(-3 * time.Minute)},
			map[string]any{"kind": "said", "text": long, "at": time.Now().Add(-3 * time.Minute)},
			map[string]any{"kind": "heard", "text": "thanks", "at": time.Now().Add(-time.Minute)},
			map[string]any{"kind": "said", "text": "Anytime.", "at": time.Now().Add(-time.Minute)},
		}
	})))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	if r := eval[string](t, ctx, `document.querySelector('#transcript').getAttribute('role') || ''`); r != "group" {
		t.Fatalf("#transcript has role %q; aria-label is ignored without one", r)
	}
	older := `[...document.querySelectorAll('#transcript .line')].find(l => l.textContent.includes('gate 31'))`
	waitFor(t, ctx, older+`.querySelector('button.more') !== null`, "a More button on the cut-off line")
	if n := eval[int](t, ctx, `document.querySelectorAll('#transcript button.more').length`); n != 1 {
		t.Fatalf("%d More buttons; only the cut-off line needs one", n)
	}
	run(t, ctx, chromedp.Evaluate(older+`.querySelector('button.more').focus(); true`, nil), chromedp.KeyEvent(kb.Enter))
	waitFor(t, ctx, older+`.classList.contains('full')`, "Enter opened the line")
	waitFor(t, ctx, `document.activeElement.matches('#transcript button.more') && document.activeElement.textContent === 'Less' && document.activeElement.getAttribute('aria-expanded') === 'true'`, "focus stays on the button, now Less")
	run(t, ctx, chromedp.KeyEvent(" "))
	waitFor(t, ctx, `!`+older+`.classList.contains('full') && document.activeElement.textContent === 'More'`, "Space closed it again")
}

// The screen has one main landmark, a heading, named navigation, a keyboard
// reachable orb, and a screenshot viewer that takes and returns focus.
func TestScreenLandmarksAndFocus(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		oneApproval(m)
		m["approvals"].([]any)[0].(map[string]any)["screenshot"] = "/screen/shot?path=x.png"
	})))
	d.handle("GET /screen/shot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="400" height="300"><rect width="400" height="300" fill="#ccc"/></svg>`))
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, visible(".need .yes"), "the approval card")
	checks := map[string]string{
		"one main landmark":           `document.querySelectorAll('main').length === 1`,
		"a page heading":              `!!document.querySelector('h1') && document.querySelector('h1').textContent.trim() !== ''`,
		"named navigation":            `!!document.querySelector('footer nav[aria-label]')`,
		"the orb is a button":         `document.querySelector('#talkOrb').getAttribute('role') === 'button' && document.querySelector('#talkOrb').tabIndex === 0`,
		"the screenshot is a button":  `document.querySelector('.need .shot').tagName === 'BUTTON'`,
		"approve says what it is for": `!!document.querySelector('.need .yes').getAttribute('aria-describedby')`,
	}
	for what, expr := range checks {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("missing: %s", what)
		}
	}
	run(t, ctx, chromedp.Focus("#talkOrb", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	waitFor(t, ctx, `document.activeElement === document.querySelector('#say')`, "Enter on the orb opens the text box")
	run(t, ctx, chromedp.KeyEvent(kb.Escape), chromedp.Focus(".need .shot", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	waitFor(t, ctx, `document.querySelector('#lightbox').classList.contains('show') && document.activeElement === document.querySelector('#lightboxClose')`, "the screenshot opened with focus inside")
	run(t, ctx, chromedp.KeyEvent(kb.Escape))
	waitFor(t, ctx, `!document.querySelector('#lightbox').classList.contains('show') && document.activeElement === document.querySelector('.need .shot')`, "focus came back to the screenshot")
}

// A message typed on this screen is marked with the page's id, and the live
// feed's copy of it (when the daemon echoes it for other screens) is not shown
// twice; the same words from another screen are.
func TestScreenSkipsItsOwnEcho(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	sent := make(chan map[string]any, 1)
	d.handle("POST /message/stream", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: done\ndata: {\"reply\":\"It's 18 degrees.\"}\n\n")
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && es && es.readyState === 1`, "the live feed is open")
	client := eval[string](t, ctx, `CLIENT`)
	run(t, ctx, chromedp.KeyEvent("w"), chromedp.Evaluate(`document.querySelector('#say').value = 'weather?'; document.querySelector('#composer').requestSubmit(); true`, nil))
	select {
	case body := <-sent:
		if body["client"] != client {
			t.Fatalf("the message went out as %v, without this page's id %q", body, client)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the message was never sent")
	}
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes("It's 18 degrees.")`, "the reply")
	d.push(map[string]any{"kind": "heard", "text": "weather?", "data": map[string]any{"origin": client}})
	d.push(map[string]any{"kind": "heard", "text": "and tomorrow?", "data": map[string]any{"origin": "the-wall"}})
	waitFor(t, ctx, `document.querySelector('#transcript').textContent.includes('and tomorrow?')`, "another screen's line")
	if n := eval[int](t, ctx, `[...document.querySelectorAll('#transcript .line.heard')].filter(l => l.textContent.includes('weather?')).length`); n != 1 {
		t.Fatalf("what this screen typed shows %d times", n)
	}
}

// Every refusal the API gives now ({error, message, fix}: devices' auth) is
// shown in its own words and its next step, as a toast, without the not-paired
// notice for what is only a scope: the approval stays to be answered and
// what was typed stays to be sent again. (ui's problem() had no test.)
func TestScreenSaysWhatTheAPIRefused(t *testing.T) {
	const why = "This device was paired to talk only, so it can't do that."
	const fix = "To give it more, pair it again: ... run `mirrin pair --screen --scopes view,chat,approve`."
	t.Run("approve", func(t *testing.T) {
		d := newDaemon(t)
		d.handle("GET /screen", jsonH(screen(oneApproval)))
		d.handle("POST /approvals/7/approve", refusedH(403, "not_allowed", why, fix))
		ctx := tab(t)
		run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, visible(".need .yes"), "the approval card")
		run(t, ctx, chromedp.Click(".need .yes", chromedp.ByQuery))
		d.waitCalled("POST /approvals/7/approve", 1)
		waitFor(t, ctx, `document.querySelector('#toasts').textContent.includes('talk only')`, "the refusal in words")
		toast := textOf(t, ctx, "#toasts")
		if !strings.Contains(toast, "Couldn't approve #7") || !strings.Contains(toast, "mirrin pair --screen --scopes") || strings.Contains(toast, "`") {
			t.Fatalf("toast reads %q", toast)
		}
		if eval[bool](t, ctx, visible("#unpaired")) {
			t.Fatal("a scope refusal showed the not-paired notice")
		}
		waitFor(t, ctx, `!document.querySelector('.need .yes').disabled`, "the buttons back, to answer elsewhere or again")
	})
	t.Run("send", func(t *testing.T) {
		d := newDaemon(t)
		d.handle("GET /screen", jsonH(screen(nil)))
		d.handle("POST /message/stream", refusedH(403, "not_allowed", why, fix))
		ctx := tab(t)
		run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, loaded, "the screen loaded")
		run(t, ctx, chromedp.KeyEvent("w"), chromedp.Evaluate(`document.querySelector('#say').value = 'weather?'; document.querySelector('#composer').requestSubmit(); true`, nil))
		d.waitCalled("POST /message/stream", 1)
		waitFor(t, ctx, `document.querySelector('#toasts').textContent.includes("Couldn't send that")`, "the refusal in words")
		if toast := textOf(t, ctx, "#toasts"); !strings.Contains(toast, "talk only") || !strings.Contains(toast, "pair it again") || strings.Contains(toast, "`") {
			t.Fatalf("toast reads %q", toast)
		}
		if v := eval[string](t, ctx, `document.querySelector('#say').value`); v != "weather?" {
			t.Fatalf("what was typed wasn't kept: %q", v)
		}
		if eval[bool](t, ctx, visible("#unpaired")) {
			t.Fatal("a scope refusal showed the not-paired notice")
		}
	})
}

// A wall screen paired to look (devices' kiosk: view only) gets no text box,
// even on a touch tablet, no "type anywhere", and no Approve/Deny it could
// only be refused on: /screen says what the device may do.
func TestAWallScreenOffersOnlyWhatItMayDo(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		oneApproval(m)
		m["can"] = []string{"view"}
		// as the API cuts a task down for a wall: counts, not the steps
		m["tasks"] = []any{map[string]any{"id": "t1", "title": "Plan the trip", "status": "running", "steps_done": 1, "steps_total": 2}}
	})))
	ctx := tab(t)
	run(t, ctx, phone(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && document.body.classList.contains('nochat')`, "the screen loaded")
	waitFor(t, ctx, `document.querySelector('#tasks').textContent.includes('1 of 2 steps')`, "the task's progress from its counts")
	if eval[bool](t, ctx, visible("#composer")) {
		t.Fatal("a view-only screen shows the text box")
	}
	waitFor(t, ctx, `!!document.querySelector('.need')`, "the approval card")
	if eval[bool](t, ctx, `!!document.querySelector('.need .yes')`) || !strings.Contains(textOf(t, ctx, ".need"), "device that may approve") {
		t.Fatalf("a view-only screen offers Approve/Deny: %q", textOf(t, ctx, ".need"))
	}
	run(t, ctx, desktop(), chromedp.Evaluate(`document.activeElement && document.activeElement.blur(); true`, nil), chromedp.KeyEvent("h"))
	if eval[bool](t, ctx, `document.querySelector('#composer').classList.contains('show')`) {
		t.Fatal("typing opened a text box on a view-only screen")
	}
	// A device that may do everything sees both.
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		oneApproval(m)
		m["can"] = []string{"view", "chat", "approve"}
	})))
	run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
	waitFor(t, ctx, `!document.body.classList.contains('nochat') && !!document.querySelector('.need .yes')`, "the controls back")
}

// Mid-conversation the twin keeps something the owner said: a quiet row
// under its reply, even one still streaming, says so with an Undo, and
// Undo forgets it. The first time on a device the row also says where
// everything it remembers is.
func TestNotedShowsUnderTheReplyWithUndo(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	release := make(chan struct{})
	d.handle("POST /message/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: delta\ndata: {\"text\":\"Good to know. \"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "event: done\ndata: {\"reply\":\"Good to know. I'll plan around it.\"}\n\n")
	})
	d.handle("POST /screen/facts/7/undo", jsonH(map[string]bool{"forgotten": true}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "the screen loaded")
	run(t, ctx, chromedp.KeyEvent("i"), chromedp.Evaluate(`document.querySelector('#say').value = "I don't eat meat, by the way."; document.querySelector('#composer').requestSubmit(); true`, nil))
	waitFor(t, ctx, `[...document.querySelectorAll('#transcript .line.said.live')].some(l => l.textContent.includes('Good to know'))`, "the reply streaming")
	d.push(map[string]any{"kind": "remembered", "text": "Akshay doesn't eat meat.", "data": map[string]any{"id": 7, "subject": "preferences"}})
	row := `#transcript .line.said:last-child .noted`
	waitFor(t, ctx, `!!document.querySelector(`+jsString(row)+`)`, "a noted row under the reply still streaming")
	close(release)
	waitFor(t, ctx, `!document.querySelector('#transcript .line.said:last-child').classList.contains('live')`, "the reply finished")
	text := textOf(t, ctx, row)
	if !strings.HasPrefix(text, "Noted: Akshay doesn't eat meat · Undo") || !strings.Contains(text, "Everything I remember is under Your twin… in the menu, and stays on this Mac.") {
		t.Fatalf("noted row reads %q", text)
	}
	if n := eval[int](t, ctx, `document.querySelectorAll('#transcript .noted').length`); n != 1 {
		t.Fatalf("%d noted rows, want 1", n)
	}
	run(t, ctx, chromedp.Click(row+" .undo", chromedp.ByQuery))
	d.waitCalled("POST /screen/facts/7/undo", 1)
	waitFor(t, ctx, `document.querySelector(`+jsString(row)+`).textContent === 'Forgotten.'`, "Undo says Forgotten.")
}

// Several things kept in one reply are one row, "Noted 2 things", which on
// this computer opens the memory page. Where everything is kept is said once
// on a device, not under every reply, and an Undo that comes too late says
// what to do instead.
func TestNotedSeveralThingsAndTheFirstTimeOnce(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("POST /screen/facts/9/undo", refusedH(409, "too_late", "Too late to undo here. Say “forget that” and I will.", ""))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && es && es.readyState === 1`, "the screen loaded")
	voice := map[string]any{"channel": "voice"}
	turn := func(heard, said string, facts ...map[string]any) {
		d.push(map[string]any{"kind": "heard", "text": heard, "data": voice})
		for _, f := range facts {
			d.push(map[string]any{"kind": "remembered", "text": f["text"], "data": map[string]any{"id": f["id"], "subject": "people"}})
		}
		d.push(map[string]any{"kind": "said", "text": said, "data": voice})
		waitFor(t, ctx, `(() => { const l = document.querySelector('#transcript .line.said:last-child'); return !!l && l.textContent.includes(`+jsString(said)+`) && !!l.querySelector('.noted'); })()`, "the reply with its noted row")
	}
	row := `#transcript .line.said:last-child .noted`
	turn("My sister Priya lives in Pune, and I'm vegetarian.", "Noted both.",
		map[string]any{"id": 7, "text": "Priya is Akshay's sister and lives in Pune."}, map[string]any{"id": 8, "text": "Akshay is vegetarian."})
	if text := textOf(t, ctx, row); !strings.HasPrefix(text, "Noted 2 things") || !strings.Contains(text, "Everything I remember") {
		t.Fatalf("noted row reads %q", text)
	}
	if href := eval[string](t, ctx, `document.querySelector(`+jsString(row+" a")+`).getAttribute('href')`); href != "/memory" {
		t.Fatalf("Noted 2 things links to %q", href)
	}
	if eval[bool](t, ctx, `!!document.querySelector('#transcript .noted .undo')`) {
		t.Fatal("a row for two things offers one Undo")
	}

	turn("Also, I take my coffee black.", "Got it.", map[string]any{"id": 9, "text": "Akshay takes his coffee black."})
	if text := textOf(t, ctx, row); text != "Noted: Akshay takes his coffee black · Undo" {
		t.Fatalf("second noted row reads %q", text)
	}
	if n := eval[int](t, ctx, `document.querySelectorAll('#transcript .noted .where').length`); n != 1 {
		t.Fatalf("where it's all kept was said %d times", n)
	}
	run(t, ctx, chromedp.Click(row+" .undo", chromedp.ByQuery))
	d.waitCalled("POST /screen/facts/9/undo", 1)
	waitFor(t, ctx, `document.querySelector(`+jsString(row)+`).textContent === 'Too late to undo here. Say “forget that” and I will.'`, "a late Undo says what to do instead")

	// Not again on this device, after a reload either.
	run(t, ctx, chromedp.Reload())
	waitFor(t, ctx, loaded+` && es && es.readyState === 1`, "the screen loaded again")
	turn("And I'm off to Tokyo in May.", "Have a great trip.", map[string]any{"id": 10, "text": "Akshay is going to Tokyo in May."})
	if text := textOf(t, ctx, row); strings.Contains(text, "Everything I remember") {
		t.Fatalf("said where it's all kept again: %q", text)
	}
}

// The floating orb shows no noted row, and so doesn't use up the sentence
// that says where everything is kept.
func TestOrbShowsNothingNoted(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := orb(t, d)
	waitFor(t, ctx, `es && es.readyState === 1`, "the orb hearing news")
	d.push(map[string]any{"kind": "heard", "text": "I don't eat meat.", "data": map[string]any{"channel": "voice"}})
	d.push(map[string]any{"kind": "remembered", "text": "Akshay doesn't eat meat.", "data": map[string]any{"id": 7, "subject": "preferences"}})
	d.push(map[string]any{"kind": "said", "text": "Got it.", "data": map[string]any{"channel": "voice"}})
	waitFor(t, ctx, `LINES.some(l => l.text === 'Got it.')`, "the reply")
	if eval[bool](t, ctx, `LINES.some(l => l.noted) || !!document.querySelector('.noted') || localStorage.getItem('mirrin.noted.seen') !== null`) {
		t.Fatal("the orb noted what the twin kept")
	}
}

// A reminder on the day has a tick on a device that may chat. It says what
// it ticks off; a tap ticks it off, draws a check and the row fades away,
// and the next refresh doesn't bring it back. Under reduced motion the
// check is drawn at once. A wall screen that only looks has no tick.
func TestATickTicksAReminderOff(t *testing.T) {
	due := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	plumber := func(m map[string]any) {
		m["reminders"] = []any{map[string]any{"id": 12, "due": due, "text": "Call the plumber", "kind": "remind"}}
	}
	tick := `#day .row.rem .tick`
	for _, still := range []bool{false, true} {
		t.Run(map[bool]string{false: "moving", true: "reduced motion"}[still], func(t *testing.T) {
			d := newDaemon(t)
			d.handle("GET /screen", jsonH(screen(plumber)))
			d.handle("POST /screen/reminders/12/done", jsonH(map[string]bool{"done": true}))
			ctx := tab(t)
			if still {
				run(t, ctx, reducedMotion())
			}
			run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
			waitFor(t, ctx, loaded+` && !!document.querySelector(`+jsString(tick)+`)`, "the reminder with its tick")
			if label := eval[string](t, ctx, `document.querySelector(`+jsString(tick)+`).getAttribute('aria-label')`); label != "Done: Call the plumber" {
				t.Fatalf("the tick is labelled %q", label)
			}
			path := `getComputedStyle(document.querySelector(` + jsString(tick+" path") + `))`
			if off := eval[string](t, ctx, path+`.strokeDashoffset`); off != "1px" {
				t.Fatalf("a check drawn before the tick: %s", off)
			}
			want := "0.4s"
			if still {
				want = "1e-05s"
			}
			if dur := eval[string](t, ctx, path+`.transitionDuration`); dur != want {
				t.Fatalf("the check draws in %s, want %s", dur, want)
			}
			d.handle("GET /screen", jsonH(screen(nil))) // what /screen says once it is done
			run(t, ctx, chromedp.Click(tick, chromedp.ByQuery))
			d.waitCalled("POST /screen/reminders/12/done", 1)
			waitFor(t, ctx, `!!document.querySelector('#day .row.rem.ticked') && `+path+`.strokeDashoffset === '0px'`, "the check drawn")
			waitFor(t, ctx, `!document.querySelector('#day .row.rem') && document.querySelector('#day').textContent.includes('Nothing on your calendar today.')`, "the row gone, and the day empty")
			run(t, ctx, chromedp.Evaluate(`load(); true`, nil))
			run(t, ctx, chromedp.Sleep(200*time.Millisecond))
			if eval[bool](t, ctx, `!!document.querySelector('#day .row')`) {
				t.Fatal("the refresh brought the reminder back")
			}
		})
	}
	t.Run("a wall screen", func(t *testing.T) {
		d := newDaemon(t)
		d.handle("GET /screen", jsonH(screen(func(m map[string]any) { plumber(m); m["can"] = []string{"view"} })))
		ctx := tab(t)
		run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
		waitFor(t, ctx, loaded+` && document.body.classList.contains('nochat') && document.querySelector('#day').textContent.includes('Call the plumber')`, "the reminder on the day")
		if eval[bool](t, ctx, `!!document.querySelector(`+jsString(tick)+`)`) {
			t.Fatal("a wall screen that only looks has a tick")
		}
	})
}

// A follow-up on the day reads as what the twin will check, not as a
// reminder, and is never "overdue": the looking is the twin's to do.
func TestAFollowUpOnTheDayReadsAsACheck(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["reminders"] = []any{
			map[string]any{"id": 7, "due": time.Now().Add(-time.Hour).Format(time.RFC3339), "text": "Acme's reply about the refund", "kind": "check"},
			map[string]any{"id": 8, "due": time.Now().Add(3 * time.Hour).Format(time.RFC3339), "text": "Call the plumber", "kind": "remind"},
		}
	})))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded+` && document.querySelectorAll('#day .row.rem').length === 2`, "the follow-up and the reminder on the day")
	rows := eval[[]string](t, ctx, `[...document.querySelectorAll('#day .row.rem')].map(r => r.querySelector('.b').textContent + ' | ' + (r.querySelector('.sub') || {}).textContent)`)
	if len(rows) != 2 || rows[0] != "Check: Acme's reply about the refund | now · follow-up" || rows[1] != "Call the plumber | reminder" {
		t.Fatalf("the day reads %q", rows)
	}
}
