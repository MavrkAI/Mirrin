package pagetest

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/chromedp/chromedp"
)

// welcomeStep3 takes the welcome page through the hello, with GET
// /welcome/channels answering ways (nil for no such route), and returns
// once the hello is on the page.
func welcomeStep3(t *testing.T, ways []string) (context.Context, *daemon) {
	t.Helper()
	ctx, d, _ := welcomeStep2(t, false)
	if ways != nil {
		d.handle("GET /welcome/channels", jsonH(ways))
	}
	d.handle("POST /welcome/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: delta\ndata: \"Good evening, Akshay.\"\n\nevent: done\ndata: \"Good evening, Akshay.\"\n\n")
	})
	run(t, ctx, chromedp.Evaluate(`window.__opened = []; window.open = (u, n) => { __opened.push(u + ' ' + n); return null }; speechSynthesis.speak = () => {}; true`, nil))
	run(t, ctx, chromedp.SendKeys("#you", "Akshay", chromedp.ByQuery), chromedp.Click(`#nameform > button`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#reply').textContent === 'Good evening, Akshay.'`, "the hello")
	return ctx, d
}

// After the hello, step 3 asks where to reach you away from this Mac:
// Telegram, WhatsApp, or just this Mac, which is chosen already. Telegram
// opens the Channels page in a window of its own; Done closes the step and
// waits for nothing.
func TestWelcomeAsksWhereToReachYou(t *testing.T) {
	ctx, _ := welcomeStep3(t, []string{"telegram", "whatsapp"})
	waitFor(t, ctx, visible("#reach"), "step 3")
	if got := textOf(t, ctx, "#reachq"); got != "3. When you're away from this Mac, where should I reach you?" {
		t.Fatalf("step 3 asks %q", got)
	}
	choices := `[...document.querySelectorAll('#reach .way')].filter(b => !b.hidden).map(b => b.querySelector('b').textContent + ': ' + b.querySelector('small').textContent).join('|')`
	want := "Telegram: Messages arrive like any chat and buzz your phone.|" +
		"WhatsApp: Links as one of your WhatsApp devices through an unofficial client. Some people prefer Telegram.|" +
		"Just this Mac for now: I'll leave things on the screen and send a Mac notification."
	if got := eval[string](t, ctx, choices); got != want {
		t.Fatalf("the choices read %q", got)
	}
	pressed := `[...document.querySelectorAll('#reach .way[aria-pressed=true]')].map(b => b.dataset.way).join(',')`
	if got := eval[string](t, ctx, pressed); got != "mac" {
		t.Fatalf("chosen at first: %q", got)
	}

	run(t, ctx, chromedp.Click(`#reach .way[data-way=telegram]`, chromedp.ByQuery))
	waitFor(t, ctx, `__opened.length === 1 && __opened[0] === '/channels mirrin-channels'`, "the Channels page opened")
	waitFor(t, ctx, visible("#reachnext")+` && document.querySelector('#reachname').textContent === 'Telegram'`, "what to do there")
	if got := eval[string](t, ctx, pressed); got != "telegram" {
		t.Fatalf("chosen: %q", got)
	}
	if href := eval[string](t, ctx, `document.querySelector('#reachnext a').getAttribute('href')`); href != "/channels" {
		t.Fatalf("the Channels link goes to %q", href)
	}

	// Changing your mind is fine, and Done waits for nothing.
	run(t, ctx, chromedp.Click(`#reach .way[data-way=mac]`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#reachnext').hidden`, "nothing to pair")
	run(t, ctx, chromedp.Click(`#reachdone`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#reach').hidden && document.activeElement === document.querySelector('#hello a')`, "step 3 closed, Keep talking next")
	if n := eval[int](t, ctx, `__opened.length`); n != 1 {
		t.Fatalf("the Channels page opened %d times", n)
	}
}

// Finishing without choosing anything works: Done straight away closes the
// step, opens nothing, and a hello said again doesn't bring it back.
func TestWelcomeFinishesWithoutAChannel(t *testing.T) {
	ctx, d := welcomeStep3(t, []string{"telegram", "whatsapp"})
	waitFor(t, ctx, visible("#reach"), "step 3")
	run(t, ctx, chromedp.Click(`#reachdone`, chromedp.ByQuery))
	waitFor(t, ctx, `document.querySelector('#reach').hidden`, "step 3 closed")
	run(t, ctx, chromedp.Evaluate(`hello(); true`, nil))
	d.waitCalled("POST /welcome/hello", 2)
	waitFor(t, ctx, `document.querySelector('#reply').textContent === 'Good evening, Akshay.'`, "the second hello")
	if !eval[bool](t, ctx, `document.querySelector('#reach').hidden && __opened.length === 0`) {
		t.Fatal("step 3 came back, or something opened")
	}
	if n := d.count("GET /welcome/channels"); n != 1 {
		t.Fatalf("asked for the channels %d times", n)
	}
}

// A build without WhatsApp doesn't offer it, and a twin with no Channels
// page has nothing to pair, so there is no step 3.
func TestWelcomeReachAsBuilt(t *testing.T) {
	t.Run("no WhatsApp", func(t *testing.T) {
		ctx, _ := welcomeStep3(t, []string{"telegram"})
		waitFor(t, ctx, visible("#reach"), "step 3")
		if got := eval[string](t, ctx, `[...document.querySelectorAll('#reach .way')].filter(b => !b.hidden).map(b => b.dataset.way).join(',')`); got != "telegram,mac" {
			t.Fatalf("offered %q", got)
		}
	})
	for name, ways := range map[string][]string{"no Channels page": {}, "no answer": nil} {
		t.Run(name, func(t *testing.T) {
			ctx, d := welcomeStep3(t, ways)
			d.waitCalled("GET /welcome/channels", 1)
			// The page's own wait for the answer ends first, then this one.
			run(t, ctx, chromedp.Evaluate(`ways.then(w => { window.__ways = w }); true`, nil))
			waitFor(t, ctx, `window.__ways !== undefined`, "the answer")
			if !eval[bool](t, ctx, `document.querySelector('#reach').hidden`) {
				t.Fatal("step 3 with nothing to offer")
			}
		})
	}
}
