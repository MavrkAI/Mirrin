package pagetest

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// settingsPage is one settings page and the JSON it loads.
type settingsPage struct {
	path  string
	reads []string       // the GET endpoints it loads from
	empty map[string]any // an empty answer for each read
	says  string         // what the empty page tells you
}

var settingsPages = []settingsPage{
	{
		path:  "/health",
		reads: []string{"GET /health.json"},
		empty: map[string]any{"GET /health.json": map[string]any{"results": []any{}}},
		says:  "No checks have run yet",
	},
	{
		path:  "/memory",
		reads: []string{"GET /memory/facts", "GET /memory/audit", "GET /memory/twin", "GET /memory/portrait"},
		empty: map[string]any{
			"GET /memory/facts": []any{}, "GET /memory/audit": []any{},
			"GET /memory/twin":     map[string]any{"name": "Mirrin", "persona": "mirrin", "character": "Calm and direct."},
			"GET /memory/portrait": map[string]any{"text": "", "updated_at": "0001-01-01T00:00:00Z"},
		},
		says: "Nothing yet",
	},
	{
		path:  "/channels",
		reads: []string{"GET /channels/list"},
		empty: map[string]any{"GET /channels/list": []any{}},
		says:  "Nothing connected yet",
	},
	{
		path:  "/accounts",
		reads: []string{"GET /accounts/list"},
		empty: map[string]any{"GET /accounts/list": []any{}},
		says:  "Nothing to sign in to",
	},
	{
		path:  "/protocols",
		reads: []string{"GET /protocols/installed"},
		empty: map[string]any{"GET /protocols/installed": map[string]any{"protocols": []any{}, "packs": []any{}}},
		says:  "No routines yet",
	},
}

// A settings page whose device isn't paired says so, with the next step,
// instead of going blank.
func TestSettingsPagesSayWhenNotPaired(t *testing.T) {
	for _, p := range settingsPages {
		t.Run(strings.TrimPrefix(p.path, "/"), func(t *testing.T) {
			d := newDaemon(t)
			hint := "Open it again from the menu bar on your Mac."
			for _, r := range p.reads {
				d.handle(r, notPairedH(hint))
			}
			ctx := tab(t)
			run(t, ctx, desktop(), chromedp.Navigate(d.url(p.path)))
			waitFor(t, ctx, visible("#gate"), "the not-paired notice")
			text := textOf(t, ctx, "#gate")
			if !strings.Contains(text, "isn't paired") || !strings.Contains(text, hint) {
				t.Fatalf("notice reads %q", text)
			}
			if !eval[bool](t, ctx, `document.querySelector('main').classList.contains('gated')`) {
				t.Fatal("the rest of the page is still showing")
			}
			// Try again checks in place: still refused, the card stays and says so
			// (a reload would land on the API's bare 401 text)
			run(t, ctx, chromedp.Evaluate(`window.__samePage = true; true`, nil), chromedp.Click("#gateRetry", chromedp.ByQuery))
			waitFor(t, ctx, `document.querySelector('#gateNote').textContent.includes('Still not paired') && `+visible("#gate"), "Try again says it is still not paired")
			// paired now: the same Try again brings the page back
			for _, r := range p.reads {
				d.handle(r, jsonH(p.empty[r]))
			}
			run(t, ctx, chromedp.Click("#gateRetry", chromedp.ByQuery))
			waitFor(t, ctx, fmt.Sprintf(`window.__samePage === true && !document.querySelector('main').classList.contains('gated') && document.querySelector('#gate').hidden && document.querySelector('main').innerText.includes(%s)`, jsString(p.says)), "the page came back")
			if title := eval[string](t, ctx, `document.title`); strings.Contains(title, "Not paired") {
				t.Fatalf("the tab still says %q", title)
			}
		})
	}
	t.Run("plain 401", func(t *testing.T) {
		d := newDaemon(t)
		for _, r := range settingsPages[1].reads {
			d.handle(r, textH(401, "unauthorized"))
		}
		ctx := tab(t)
		run(t, ctx, desktop(), chromedp.Navigate(d.url("/memory")))
		waitFor(t, ctx, visible("#gate"), "the not-paired notice")
		if text := textOf(t, ctx, "#gate"); !strings.Contains(text, "menu bar") {
			t.Fatalf("a plain 401 gives no next step: %q", text)
		}
	})
}

// A failing load shows the daemon's own words and a Try again that works;
// an empty one says what to do.
func TestSettingsPagesShowErrorsAndEmptyStates(t *testing.T) {
	for _, p := range settingsPages {
		t.Run(strings.TrimPrefix(p.path, "/"), func(t *testing.T) {
			d := newDaemon(t)
			for _, r := range p.reads {
				d.handle(r, textH(500, "database is locked"))
			}
			ctx := tab(t)
			run(t, ctx, desktop(), chromedp.Navigate(d.url(p.path)))
			waitFor(t, ctx, `[...document.querySelectorAll('[role=alert]')].some(e => e.offsetHeight > 0 && e.innerText.includes('database is locked') && e.querySelector('button'))`, "an error with a Try again button")
			for _, r := range p.reads {
				d.handle(r, jsonH(p.empty[r]))
			}
			run(t, ctx, chromedp.Evaluate(`[...document.querySelectorAll('[role=alert] button')].find(b => b.offsetHeight > 0).click(); true`, nil))
			waitFor(t, ctx, fmt.Sprintf(`document.querySelector('main').innerText.includes(%s) && ![...document.querySelectorAll('[role=alert]')].some(e => e.offsetHeight > 0 && e.innerText.includes('database is locked'))`, jsString(p.says)), "the empty state after Try again")
		})
	}
}

// A button the device may not use says what the API said and how to get it,
// without the backticks the API puts around commands.
func TestSettingsPagesSayWhatTheAPIRefused(t *testing.T) {
	d := newDaemon(t)
	for _, r := range settingsPages[1].reads {
		d.handle(r, jsonH(settingsPages[1].empty[r]))
	}
	d.handle("POST /memory/portrait", refusedH(http.StatusForbidden, "not_allowed",
		"This device was paired to see the screen only, so it can't do that.",
		"To give it more, pair it again: on the computer Mirrin runs on, run `mirrin pair --screen --scopes view,chat,approve`."))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/memory")))
	waitFor(t, ctx, `document.querySelector('main').innerText.includes('Nothing yet')`, "the memory page")
	run(t, ctx, chromedp.Click("#refresh", chromedp.ByQuery))
	d.waitCalled("POST /memory/portrait", 1)
	waitFor(t, ctx, `document.querySelector('#portraitMsg').textContent.includes('see the screen only')`, "the refusal")
	text := textOf(t, ctx, "#portraitMsg")
	if !strings.Contains(text, "mirrin pair --screen --scopes view,chat,approve") || strings.Contains(text, "`") {
		t.Fatalf("the refusal reads %q", text)
	}
	if eval[bool](t, ctx, visible("#gate")) {
		t.Fatal("a refused button is not an unpaired device")
	}
}

// Run now says whether the protocol started; it used to say nothing while the
// API turned it away.
func TestProtocolsRunNowTellsTheTruth(t *testing.T) {
	installed := map[string]any{"protocols": []any{map[string]any{"name": "morning-brief", "description": "Weather and the day ahead", "enabled": true, "schedule": "0 7 * * *"}}, "packs": []any{}}
	for _, c := range []struct {
		name string
		run  http.HandlerFunc
		want string
		not  string
	}{
		{"refused", textH(401, "unauthorized"), "run morning-brief", "Started"},
		// What the API really answers now (devices): a device without the
		// scope gets 403 not_allowed, in words, with the next step.
		{"not allowed", refusedH(403, "not_allowed", "This device can't run protocols.", "To give it more, pair it again: run `mirrin pair --screen --scopes view,chat,approve,admin`."), "pair it again", "Started"},
		{"started", jsonH(map[string]string{"ok": "morning-brief"}), "Started", "Couldn't"},
		{"unknown", textH(400, `no protocol named "morning-brief"`), `no protocol named "morning-brief"`, "Started"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDaemon(t)
			d.handle("GET /protocols/installed", jsonH(installed))
			d.handle("POST /protocols/run", c.run)
			ctx := tab(t)
			run(t, ctx, desktop(), chromedp.Navigate(d.url("/protocols")))
			waitFor(t, ctx, `!!document.querySelector('#installed button')`, "the protocol list")
			run(t, ctx, chromedp.Click("#installed button", chromedp.ByQuery))
			d.waitCalled("POST /protocols/run", 1)
			waitFor(t, ctx, fmt.Sprintf(`document.querySelector('#installed').innerText.includes(%s)`, jsString(c.want)), "an honest answer")
			if strings.Contains(textOf(t, ctx, "#installed"), c.not) {
				t.Fatalf("the page says %q: %q", c.not, textOf(t, ctx, "#installed"))
			}
			if (c.name == "refused" || c.name == "not allowed") && eval[bool](t, ctx, visible("#gate")) {
				t.Fatal("a refused button is not an unpaired device")
			}
		})
	}
}

// Every text field on every page is at least 16px on a phone, so iOS doesn't
// zoom the page when one is tapped.
func TestNoTextFieldZoomsAPhone(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	d.handle("GET /health.json", jsonH(map[string]any{"results": []any{}}))
	for k, v := range settingsPages[1].empty {
		d.handle(k, jsonH(v))
	}
	d.handle("GET /channels/list", jsonH([]any{map[string]any{"name": "telegram", "label": "Telegram", "blurb": "Chat from anywhere.", "enabled": false,
		"fields": []any{map[string]any{"key": "token", "label": "Bot token", "secret": true, "required": true}}}}))
	d.handle("GET /accounts/list", jsonH([]any{map[string]any{"name": "google", "label": "Google", "blurb": "Calendar, mail and files.", "connected": false, "has_client": false}}))
	d.handle("GET /protocols/installed", jsonH(map[string]any{"protocols": []any{}, "packs": []any{}}))
	ctx := tab(t)
	for path := range pageFiles {
		if path == "/health" {
			continue // no text fields
		}
		run(t, ctx, phone(), chromedp.Navigate(d.url(path)))
		waitFor(t, ctx, `document.readyState === 'complete' && document.querySelectorAll('input, textarea, select').length > 0`, path+" has its fields")
		small := eval[[]string](t, ctx, `[...document.querySelectorAll('input:not([type=checkbox]):not([type=radio]), textarea, select')]
			.map(e => [e.name || e.id || e.tagName, parseFloat(getComputedStyle(e).fontSize)]).filter(([, px]) => px < 16).map(([n, px]) => n + ' ' + px + 'px')`)
		if len(small) > 0 {
			t.Errorf("%s: fields under 16px: %v", path, small)
		}
	}
}

// A channel card opens from the keyboard and says whether it is open.
func TestChannelCardsOpenFromTheKeyboard(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /channels/list", jsonH([]any{map[string]any{"name": "telegram", "label": "Telegram", "blurb": "Chat from anywhere.", "enabled": false,
		"fields": []any{map[string]any{"key": "token", "label": "Bot token", "secret": true, "required": true}}}}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/channels")))
	waitFor(t, ctx, `!!document.querySelector('.card .head')`, "the channel card")
	if tag := eval[string](t, ctx, `document.querySelector('.card .head').tagName`); tag != "BUTTON" {
		t.Fatalf("the card header is a %s, which the keyboard can't reach", tag)
	}
	run(t, ctx, chromedp.Focus(".card .head", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	waitFor(t, ctx, `document.querySelector('.card').classList.contains('open') && document.querySelector('.card .head').getAttribute('aria-expanded') === 'true'`, "the card opened")
}

// A token being typed survives the page's own refresh.
func TestChannelsKeepWhatYouTyped(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /channels/list", jsonH([]any{map[string]any{"name": "telegram", "label": "Telegram", "blurb": "Chat from anywhere.", "enabled": false,
		"fields": []any{map[string]any{"key": "token", "label": "Bot token", "secret": true, "required": true}}}}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/channels")))
	waitFor(t, ctx, `!!document.querySelector('.card .head')`, "the channel card")
	run(t, ctx, chromedp.Click(".card .head", chromedp.ByQuery), chromedp.SendKeys(".card input[name=token]", "123:abc", chromedp.ByQuery))
	// something changed, so the refresh redraws the card
	d.handle("GET /channels/list", jsonH([]any{map[string]any{"name": "telegram", "label": "Telegram", "blurb": "Chat from anywhere.", "enabled": false,
		"notice": "Telegram was slow to answer a moment ago.",
		"fields": []any{map[string]any{"key": "token", "label": "Bot token", "secret": true, "required": true}}}}))
	run(t, ctx, chromedp.Evaluate(`window.__card = document.querySelector('.card'); document.activeElement.blur(); loadAll(); true`, nil))
	waitFor(t, ctx, `document.querySelector('.card') !== window.__card && document.querySelector('.card input[name=token]').value === '123:abc'`, "the typed token after a refresh")
}

// The refresh doesn't take keyboard focus away, and a button's answer lands in
// the card on the page even when the card was redrawn while it waited.
func TestChannelsKeepFocusAndAnswersOnRefresh(t *testing.T) {
	tg := func(extra map[string]any) http.HandlerFunc {
		k := map[string]any{"name": "telegram", "label": "Telegram", "blurb": "Chat from anywhere.", "enabled": true, "running": true, "owner_chat": "",
			"fields": []any{map[string]any{"key": "token", "label": "Bot token", "secret": true, "required": true}}}
		for key, v := range extra {
			k[key] = v
		}
		return jsonH([]any{k})
	}
	d := newDaemon(t)
	d.handle("GET /channels/list", tg(nil))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/channels")))
	waitFor(t, ctx, `!!document.querySelector('.card .head')`, "the channel card")
	run(t, ctx, chromedp.Focus(".card .head", chromedp.ByQuery))
	d.handle("GET /channels/list", tg(map[string]any{"owner_chat": "telegram:1"}))
	run(t, ctx, chromedp.Evaluate(`window.__card = document.querySelector('.card'); loadAll(); true`, nil))
	waitFor(t, ctx, `document.querySelector('.card') !== window.__card`, "the redraw")
	if !eval[bool](t, ctx, `document.activeElement === document.querySelector('.card[data-name=telegram] .head')`) {
		t.Fatalf("focus went to %s after the refresh", eval[string](t, ctx, `document.activeElement.tagName + '.' + document.activeElement.className`))
	}
	// a slow Test: the card is redrawn while it waits, and the answer still shows
	d.handle("POST /channels/test", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		jsonH(map[string]string{"ok": "sent"})(w, r)
	})
	run(t, ctx, chromedp.Click(".card .head", chromedp.ByQuery), chromedp.Click(".card .test", chromedp.ByQuery))
	d.waitCalled("POST /channels/test", 1)
	d.handle("GET /channels/list", tg(map[string]any{"owner_chat": "telegram:1", "notice": "Replies may be slow for a minute."}))
	run(t, ctx, chromedp.Evaluate(`window.__card = document.querySelector('.card'); loadAll(); true`, nil))
	waitFor(t, ctx, `document.querySelector('.card') !== window.__card`, "the redraw while Test waits")
	waitFor(t, ctx, `document.querySelector('.card .msg').textContent.includes('Sent. Check your phone.')`, "the Test answer in the redrawn card")
	if !eval[bool](t, ctx, `document.activeElement === document.querySelector('.card[data-name=telegram] .test')`) {
		t.Fatalf("focus went to %s after the redraw", eval[string](t, ctx, `document.activeElement.tagName + '.' + document.activeElement.className`))
	}
}

// Nothing on a phone scrolls sideways, even with long problems and errors.
func TestNothingScrollsSidewaysOnAPhone(t *testing.T) {
	longErr := "Telegram said: 401 Unauthorized (the bot token in ~/.mirrin/config.yaml was revoked; create a new one with @BotFather and paste it here)"
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
		m["health"] = "2 problems"
		m["problems"] = []any{
			"Thinking: can't reach Ollama at http://127.0.0.1:11434/api/chat (connection refused)",
			"Google: token at /Users/someone/.mirrin/secrets/google-oauth-token.json expired on 2026-09-01",
		}
	})))
	d.handle("GET /health.json", jsonH(map[string]any{"results": []any{map[string]any{"label": "Thinking", "state": "fail",
		"detail": "Can't reach http://127.0.0.1:11434/api/chat/completions/with/a/really/long/path/that/never/breaks",
		"fix":    "Start Ollama, or set llm.base_url in /Users/someone/.mirrin/config.yaml to a server that answers."}}}))
	d.handle("GET /channels/list", jsonH([]any{
		map[string]any{"name": "telegram", "label": "Telegram", "blurb": "Chat from anywhere.", "enabled": true, "error": longErr},
		map[string]any{"name": "whatsapp", "label": "WhatsApp", "blurb": "Chat from your phone.", "enabled": true, "running": true},
	}))
	ctx := tab(t)
	for _, c := range []struct{ path, ready string }{
		{"/ui", loaded + ` && document.querySelector('#lights').textContent.includes('Ollama')`},
		{"/health", `document.querySelector('#rows').textContent.includes('Thinking')`},
		{"/channels", `document.querySelectorAll('.card').length === 2`},
	} {
		run(t, ctx, phone(), chromedp.Navigate(d.url(c.path)))
		waitFor(t, ctx, c.ready, c.path+" loaded")
		// against the phone's width, not innerWidth: a mobile browser zooms out to fit what overflows
		if w := eval[int](t, ctx, `document.documentElement.scrollWidth`); w > 390 {
			t.Errorf("%s is %dpx wide on a 390px phone", c.path, w)
		}
	}
}

// With no checks run yet, Health doesn't claim all is well.
func TestHealthDoesNotSayAllGoodBeforeChecking(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /health.json", jsonH(map[string]any{"results": []any{}}))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/health")))
	waitFor(t, ctx, `document.querySelector('#rows').textContent.includes('No checks have run yet')`, "the empty state")
	if s := textOf(t, ctx, "#summary"); strings.Contains(s, "All good") {
		t.Fatalf("summary says %q with nothing checked", s)
	}
}

// A channel the daemon is reconnecting says so, in amber, instead of looking
// connected or broken.
func TestChannelsSayWhenReconnecting(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /channels/list", jsonH([]any{map[string]any{"name": "telegram", "label": "Telegram", "blurb": "Chat from anywhere.",
		"enabled": true, "running": false, "state": "reconnecting", "since": "2026-09-27T10:02:00Z"}}))
	ctx := tab(t)
	run(t, ctx, utc(), desktop(), chromedp.Navigate(d.url("/channels")))
	waitFor(t, ctx, `!!document.querySelector('.card .status')`, "the channel card")
	if s := textOf(t, ctx, ".card .status"); !strings.Contains(s, "reconnecting") || !strings.Contains(s, "10:02") {
		t.Fatalf("status reads %q", s)
	}
}

// A button pressed from the keyboard keeps focus after its request, although
// it is disabled while it waits (which drops focus to the page in Chrome).
func TestBusyButtonsKeepKeyboardFocus(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /health.json", jsonH(map[string]any{"results": []any{}}))
	d.handle("POST /health/run", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		jsonH(map[string]any{"results": []any{map[string]any{"label": "Thinking", "state": "ok", "detail": "Ollama answered."}}})(w, r)
	})
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/health")))
	waitFor(t, ctx, `document.querySelector('#rows').textContent.includes('No checks have run yet')`, "the page")
	run(t, ctx, chromedp.Focus("#run", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	waitFor(t, ctx, `document.querySelector('#runMsg').textContent.includes('Checked just now')`, "the check finished")
	waitFor(t, ctx, `document.activeElement === document.querySelector('#run')`, "focus back on Check again now")
}

// The test browser is started without Chrome's background calls to Google
// (sync, component updates, safe browsing), so tests reach only the fake daemon.
func TestChromeStaysOffTheNetwork(t *testing.T) {
	ctx := tab(t)
	run(t, ctx, chromedp.Navigate("chrome://version"))
	waitFor(t, ctx, `!!document.querySelector('#command_line')`, "chrome://version")
	cmd := textOf(t, ctx, "#command_line")
	for _, flag := range []string{"--disable-background-networking", "--disable-component-update", "--disable-sync", "--safebrowsing-disable-auto-update", "--disable-domain-reliability", "--use-mock-keychain", "--user-data-dir="} {
		if !strings.Contains(cmd, flag) {
			t.Errorf("Chrome runs without %s: %s", flag, cmd)
		}
	}
}
