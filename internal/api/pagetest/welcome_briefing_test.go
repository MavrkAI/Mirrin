package pagetest

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
)

// helloWithOffer answers the first hello with its words, then the offer of
// a morning briefing.
func helloWithOffer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: delta\ndata: \"Good afternoon, Akshay.\"\n\nevent: done\ndata: \"Good afternoon, Akshay.\"\n\n"+
		"event: offer\ndata: {\"id\":7,\"text\":\"Want me to brief you at seven tomorrow?\",\"time\":\"07:00\"}\n\n")
}

// Before the first hello, a calendar not yet connected is offered, and can
// be skipped: Connect opens the Accounts page in a window of its own, and
// coming back here notices it connected. Continue goes on to the hello.
func TestWelcomeOffersTheCalendarFirst(t *testing.T) {
	ctx, d, _ := welcomeStep2(t, false)
	var connected atomic.Bool
	d.handle("GET /welcome/calendar", func(w http.ResponseWriter, r *http.Request) {
		jsonH(map[string]bool{"connected": connected.Load()})(w, r)
	})
	d.handle("POST /welcome/hello", helloWithOffer)
	run(t, ctx, chromedp.Evaluate(`window.__opened = []; window.open = (u, n) => { __opened.push(u + ' ' + n); return null }; speechSynthesis.speak = () => {}; true`, nil))
	run(t, ctx, chromedp.SendKeys("#you", "Akshay", chromedp.ByQuery), chromedp.Click(`#nameform > button`, chromedp.ByQuery))
	waitFor(t, ctx, visible("#cal")+` && document.activeElement === document.querySelector('#calconnect')`, "the calendar step")
	if got := textOf(t, ctx, "#calq"); got != "Connect your calendar?" {
		t.Fatalf("the step asks %q", got)
	}
	if d.count("POST /welcome/hello") != 0 || !eval[bool](t, ctx, `document.querySelector('#hello').hidden`) {
		t.Fatal("the hello came before the calendar step")
	}
	if got := textOf(t, ctx, "#calgo"); got != "Skip for now" {
		t.Fatalf("the way on reads %q", got)
	}
	run(t, ctx, chromedp.Click("#calconnect", chromedp.ByQuery))
	waitFor(t, ctx, `__opened.length === 1 && __opened[0] === '/accounts mirrin-accounts'`, "the Accounts page opened")
	waitFor(t, ctx, visible("#calnext"), "what to do there")
	connected.Store(true)
	run(t, ctx, chromedp.Evaluate(`window.dispatchEvent(new Event('focus')); true`, nil))
	waitFor(t, ctx, visible("#calok")+` && document.querySelector('#calgo').textContent === 'Continue' && document.querySelector('#calconnect').hidden`, "connected, noticed on coming back")
	run(t, ctx, chromedp.Click("#calgo", chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#cal').hidden && document.querySelector('#reply').textContent === 'Good afternoon, Akshay.'`, "the hello")
}

// Skipping the calendar goes straight on to the hello; with one connected
// already, or no answer about it, there is no step at all.
func TestWelcomeCalendarStepIsSkippable(t *testing.T) {
	t.Run("skip", func(t *testing.T) {
		ctx, d, _ := welcomeStep2(t, false)
		d.handle("GET /welcome/calendar", jsonH(map[string]bool{"connected": false}))
		d.handle("POST /welcome/hello", helloWithOffer)
		run(t, ctx, chromedp.Evaluate(`speechSynthesis.speak = () => {}; true`, nil))
		run(t, ctx, chromedp.SendKeys("#you", "Akshay", chromedp.ByQuery), chromedp.Click(`#nameform > button`, chromedp.ByQuery))
		waitFor(t, ctx, visible("#cal"), "the calendar step")
		run(t, ctx, chromedp.Click("#calgo", chromedp.ByQuery))
		waitFor(t, ctx, `document.querySelector('#reply').textContent === 'Good afternoon, Akshay.'`, "the hello")
	})
	t.Run("connected already", func(t *testing.T) {
		ctx, d, _ := welcomeStep2(t, false)
		d.handle("GET /welcome/calendar", jsonH(map[string]bool{"connected": true}))
		d.handle("POST /welcome/hello", helloWithOffer)
		run(t, ctx, chromedp.Evaluate(`speechSynthesis.speak = () => {}; true`, nil))
		run(t, ctx, chromedp.SendKeys("#you", "Akshay", chromedp.ByQuery), chromedp.Click(`#nameform > button`, chromedp.ByQuery))
		waitFor(t, ctx, `document.querySelector('#reply').textContent === 'Good afternoon, Akshay.'`, "the hello")
		if !eval[bool](t, ctx, `document.querySelector('#cal').hidden`) {
			t.Fatal("asked to connect a connected calendar")
		}
	})
}

// After the hello, the offer: a card with the question, a time to choose
// (seven unless changed), Yes and Later. Yes sends the time chosen, and the
// card gives way to the twin's answer; Later sends no yes.
func TestWelcomeBriefingCard(t *testing.T) {
	for _, yes := range []bool{true, false} {
		t.Run(fmt.Sprint("yes ", yes), func(t *testing.T) {
			ctx, d, bodies := welcomeStep2(t, false)
			d.handle("POST /welcome/hello", helloWithOffer)
			answer := "No problem. Ask me for a morning briefing whenever you like."
			if yes {
				answer = "Done. I'll brief you at 8:00 am every morning, starting tomorrow."
			}
			d.handle("POST /welcome/briefing", bodies.record(map[string]string{"text": answer}))
			run(t, ctx, chromedp.Evaluate(`speechSynthesis.speak = () => {}; true`, nil))
			run(t, ctx, chromedp.SendKeys("#you", "Akshay", chromedp.ByQuery), chromedp.Click(`#nameform > button`, chromedp.ByQuery))
			waitFor(t, ctx, visible("#offer"), "the offer")
			if got := textOf(t, ctx, "#offerq"); got != "Want me to brief you at seven tomorrow?" {
				t.Fatalf("the offer reads %q", got)
			}
			if got := eval[string](t, ctx, `document.querySelector('#offertime').value`); got != "07:00" {
				t.Fatalf("suggested %q", got)
			}
			run(t, ctx, chromedp.SetValue("#offertime", "08:00", chromedp.ByQuery))
			button := "#offerlater"
			if yes {
				button = "#offeryes"
			}
			run(t, ctx, chromedp.Click(button, chromedp.ByQuery))
			waitFor(t, ctx, `document.querySelector('#offer').hidden && !document.querySelector('#offersaid').hidden && document.querySelector('#offersaid').textContent === `+jsString(answer), "the answer")
			got := bodies.last("/welcome/briefing")
			if got["Yes"] != yes || (yes && got["Time"] != "08:00") {
				t.Fatalf("posted %v", got)
			}
			// A hello said again doesn't bring the card back.
			run(t, ctx, chromedp.Evaluate(`hello(); true`, nil))
			d.waitCalled("POST /welcome/hello", 2)
			waitFor(t, ctx, `document.querySelector('#reply').textContent === 'Good afternoon, Akshay.'`, "the second hello")
			if !eval[bool](t, ctx, `document.querySelector('#offer').hidden`) {
				t.Fatal("the answered offer came back")
			}
		})
	}
}
