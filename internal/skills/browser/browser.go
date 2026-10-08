// Package browser is the twin's hands on the web: one real Chrome, kept open
// between calls, signed in to whatever the user has logged into, so sites with
// no API (banks, bookings, government, shops) are still usable. Pages come
// back as text, numbered elements and a screenshot the model can look at.
//
// Everything Chrome loads goes through a guard on this machine that keeps it
// on the public internet (see egress.go): a web page can't steer the twin
// into the router, a local admin page or a cloud metadata service.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/skills/web"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// inspectJS numbers the interactive elements and describes them.
const inspectJS = `(() => {
  const rows = []; let i = 0;
  for (const el of document.querySelectorAll('a[href], button, input, select, textarea, [role=button], [role=link], [role=tab], [role=menuitem], [contenteditable=true]')) {
    const r = el.getBoundingClientRect(); if (r.width === 0 || r.height === 0) continue;
    const st = getComputedStyle(el); if (st.visibility === 'hidden' || st.display === 'none') continue;
    i++; el.setAttribute('data-oh-ref', i);
    let label = (el.innerText || el.value || el.placeholder || el.getAttribute('aria-label') || el.title || el.alt || '').trim().replace(/\s+/g,' ').slice(0, 70);
    const tag = el.tagName.toLowerCase() + (el.type && el.type !== 'text' ? '[' + el.type + ']' : '');
    const extra = el.tagName === 'A' ? ' → ' + (el.getAttribute('href')||'').slice(0,80) : (el.checked ? ' (checked)' : '');
    rows.push('[' + i + '] ' + tag + ' ' + JSON.stringify(label) + extra + (r.top > innerHeight ? ' (below the fold)' : ''));
    if (i >= 150) break;
  }
  return rows.join('\n');
})()`

// labelJS returns the visible label of an element by ref or selector.
const labelJS = `((sel) => { const el = document.querySelector(sel); if (!el) return ''; return ((el.innerText || el.value || el.getAttribute('aria-label') || el.title || '') + ' ' + (el.id || '') + ' ' + (el.name || '')).trim().slice(0, 120); })`

// rePayment spots the clicks that move money.
var rePayment = regexp.MustCompile(`(?i)\b(pay|payment|buy|purchase|checkout|check out|place (the )?order|confirm (and pay|booking|order|purchase|payment)|book now|complete (order|purchase|booking)|subscribe|transfer|send money|donate|upgrade plan)\b`)

// textJS returns readable page text, main content first.
const textJS = `(() => {
  const main = document.querySelector('main, [role=main], article') || document.body;
  return (main.innerText || document.body.innerText || '').replace(/\n{3,}/g, '\n\n');
})()`

// Session owns the long-lived Chrome.
type Session struct {
	cfg     config.Browser
	dataDir string
	log     *slog.Logger
	guard   *guard

	launchMu sync.Mutex // one Chrome start at a time; held while starting, not s.mu

	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	headed   bool
	closed   bool
	lastUse  time.Time
	shotSeq  int
	handedTo time.Time // when browser_signin last handed the window to the user
	teach    *teaching
	ua       learnedUA            // what headless Chrome calls itself (ua.go)
	asked    map[string]askedPage // pages browser_act approvals were asked on (stale.go)
	pruned   time.Time            // last screenshot clean-up
	done     chan struct{}
	// forceHeadless keeps teach/sign-in windows hidden (tests).
	forceHeadless bool
	live          liveHub // the live view and take-over (live.go)
	// OnHandOver hears that a page was handed to the owner on the screen
	// (the daemon shows the orb and tells their phone), and says whether a
	// phone was told. ScreenURL is the screen's address for the chat that
	// asked, or "" when that chat isn't at this computer, where a local
	// address would be a dead link.
	OnHandOver func(url, ask string) (phoned bool)
	ScreenURL  func(chatKey string) string
	// ShowScreen brings the presence screen up at the page on this
	// computer for a chat here (a voice chat has nothing on screen to
	// click), and says whether it did.
	ShowScreen func(chatKey string) bool
	// OnActive hears that the twin started using its browser after a while
	// idle (a new run), so the screen can offer to watch it.
	OnActive func()
}

// using marks one of the twin's browser tool calls for the screen (live.go
// use), telling OnActive when it starts a new run. Call what it returns when
// the call ends.
func (s *Session) using(key string) func() {
	done, fresh := s.live.use(key)
	if fresh && s.OnActive != nil {
		s.OnActive()
	}
	return done
}

// mockKeychain keeps Chrome away from the macOS keychain. Tests set it: their
// throwaway profiles hold nothing worth encrypting, and under a sandbox or a
// temporary HOME macOS would otherwise ask the user to create a keychain.
// The real profile must not use it, or its saved logins stop decrypting.
var mockKeychain bool

// NewSession prepares (but does not start) the browser. Screenshots past
// their keeping time are cleared now and as new ones are taken.
func NewSession(cfg config.Browser, dataDir string, log *slog.Logger) *Session {
	if log == nil {
		log = slog.Default()
	}
	if cfg.IdleMinutes <= 0 {
		cfg.IdleMinutes = 10
	}
	s := &Session{cfg: cfg, dataDir: dataDir, log: log, guard: newGuard(), done: make(chan struct{})}
	s.pruneShots(true)
	go s.reaper()
	return s
}

// AllowHosts lets the browser reach these hosts on the owner's own network
// (skills.web.allow_hosts: names, addresses or ranges, as for fetch_url).
// Everything else on this computer or the local network is refused.
func (s *Session) AllowHosts(hosts ...string) *Session {
	s.guard.setAllow(web.NewAllowlist(hosts))
	return s
}

// Close shuts the browser down.
func (s *Session) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	s.stopLocked()
	s.mu.Unlock()
	s.guard.close()
}

func (s *Session) stopLocked() {
	if s.cancel != nil {
		s.cancel()
		s.cancel, s.ctx = nil, nil
	}
	// A page that's gone can't be the owner's any more: a hold left behind
	// would keep the twin waiting for a hand-back that can't come.
	s.TakeOver(false)
}

func (s *Session) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		s.mu.Lock()
		idle := time.Duration(s.cfg.IdleMinutes) * time.Minute
		if s.headed && time.Since(s.handedTo) < 15*time.Minute || s.live.isHeld() {
			idle = 20 * time.Minute // give the user time to log in, or finish what they took over
		}
		if s.ctx != nil && time.Since(s.lastUse) > idle {
			s.log.Info("browser closed after idle")
			s.stopLocked()
		}
		s.mu.Unlock()
		s.pruneShots(false)
	}
}

// errClosed is a call after Close.
var errClosed = errors.New("the browser has been shut down")

// tab returns the live tab, starting Chrome if needed. headed forces a visible
// window (restarting a headless one; the profile, and so the logins, persist).
func (s *Session) tab(headed bool) (context.Context, error) {
	// While the owner drives (live view), the twin waits, before any lock.
	if err := s.waitHandBack(handBackWait); err != nil {
		return nil, err
	}
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errClosed
	}
	s.lastUse = time.Now()
	if s.ctx != nil && s.ctx.Err() == nil && (s.headed || !headed) {
		ctx := s.ctx
		s.mu.Unlock()
		return ctx, nil
	}
	s.stopLocked()
	want := headed || !s.cfg.Headless
	s.mu.Unlock()

	ctx, cancel, err := s.launch(want)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if s.closed {
		cancel()
		return nil, errClosed
	}
	s.ctx, s.cancel, s.headed = ctx, cancel, want
	s.lastUse = time.Now()
	return ctx, nil
}

// launch starts Chrome behind the guard. Headless, it presents Chrome's own
// user agent without the "HeadlessChrome" that gives it away; the first start
// learns that from the browser itself, and one more start applies it.
func (s *Session) launch(headed bool) (context.Context, context.CancelFunc, error) {
	// Chrome hung on a locked keychain a moment ago and it is still locked:
	// say so now rather than wait for it to hang again.
	if le, at := lastLaunchFailure(); le != nil && errors.Is(le, errLaunchTimeout) && time.Since(at) < 10*time.Minute && keychainLocked(context.Background()) {
		return nil, nil, le
	}
	proxy, err := s.guard.start()
	if err != nil {
		return nil, nil, fmt.Errorf("could not start the browser's safety check: %w", err)
	}
	if what := s.guard.unsupportedSetting(); what != "" {
		s.log.Warn("this computer's proxy is set by a setting the browser can't follow yet; it connects directly", "setting", what)
	}
	ua := ""
	if !headed {
		ua = s.knownUA()
	}
	for attempt := 0; ; attempt++ {
		ctx, cancel, ver, err := s.startChromeRetrying(headed, proxy, ua) // launch.go
		if err != nil {
			return nil, nil, err
		}
		if headed || attempt == 2 {
			return ctx, cancel, nil
		}
		next := s.nextUA(ua, ver)
		if next == ua {
			return ctx, cancel, nil
		}
		cancel()
		ua = next
	}
}

// adjustFlags, when set, changes the switches of every start (tests).
var adjustFlags func(map[string]any)

// chromeFlags are the switches for one Chrome start. Headless, it may
// present ua; a visible window is Chrome as installed.
func (s *Session) chromeFlags(headed bool, proxy, ua string) map[string]any {
	f := map[string]any{
		"no-first-run":             true,
		"no-default-browser-check": true,
		"user-data-dir":            filepath.Join(s.dataDir, "chrome-profile"),
		"headless":                 !headed,
		"disable-blink-features":   "AutomationControlled",
		"enable-automation":        false,
		"disable-infobars":         true,
		"window-size":              "1280,900",
		"hide-scrollbars":          !headed,
		"mute-audio":               true,
		// Everything goes through the guard, this machine's own addresses
		// included (Chrome skips the proxy for those unless told not to), and
		// WebRTC may not send UDP around it.
		"proxy-server":                    proxy,
		"proxy-bypass-list":               "<-loopback>",
		"force-webrtc-ip-handling-policy": "disable_non_proxied_udp",
	}
	if ua != "" && !headed {
		f["user-agent"] = ua
	}
	if mockKeychain {
		f["use-mock-keychain"] = true
	}
	if adjustFlags != nil {
		adjustFlags(f)
	}
	return f
}

// startChrome runs one Chrome and waits, at most launchTimeout, for it to
// answer; a start that hangs ends in a plain error, not a stuck twin. Then
// it makes sure that Chrome really goes through the guard (canary.go).
func (s *Session) startChrome(headed bool, proxy, ua string) (context.Context, context.CancelFunc, chromeVersion, error) {
	opts := []chromedp.ExecAllocatorOption{
		chromedp.WSURLReadTimeout(launchTimeout + 5*time.Second), // ours ends it first
	}
	for name, value := range s.chromeFlags(headed, proxy, ua) {
		opts = append(opts, chromedp.Flag(name, value))
	}
	if p := findChrome(); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}
	actx, acancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, ccancel := chromedp.NewContext(actx, chromedp.WithLogf(func(string, ...any) {}), chromedp.WithErrorf(func(string, ...any) {}))
	cancel := func() { ccancel(); acancel() }
	var ver chromeVersion
	started := make(chan error, 1)
	go func() {
		started <- chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
			_, product, _, agent, _, err := cdpbrowser.GetVersion().Do(c)
			ver = chromeVersion{product: product, userAgent: agent}
			return err
		}))
	}()
	var err error
	select {
	case err = <-started:
	case <-time.After(launchTimeout):
		err = errLaunchTimeout
	}
	if err != nil {
		stopChrome(err, cancel) // launch.go
		le := explainLaunch(context.Background(), err)
		noteLaunch(le)
		s.log.Warn("browser did not start", "err", err)
		return nil, nil, chromeVersion{}, le
	}
	if err := s.checkGuardInUse(ctx); err != nil {
		cancel()
		le := s.otherProxyError(err)
		noteLaunch(le)
		s.log.Warn("browser stopped: it doesn't use the guard proxy", "err", err)
		return nil, nil, chromeVersion{}, le
	}
	noteLaunch(nil)
	return ctx, cancel, ver, nil
}

// snapshot captures the state the model needs after any action. Anything the
// guard refused since the call began is listed, so a half-loaded page makes
// sense.
func (s *Session) snapshot(ctx context.Context, textLimit int, since time.Time) (string, error) {
	var title, loc, text, elems string
	var shot []byte
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := chromedp.Run(tctx,
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Title(&title),
		chromedp.Location(&loc),
		chromedp.Evaluate(textJS, &text),
		chromedp.Evaluate(inspectJS, &elems),
		chromedp.CaptureScreenshot(&shot),
	)
	if err != nil {
		return "", err
	}
	path := s.saveShot("browser", shot)
	text = strings.TrimSpace(text)
	if len(text) > textLimit {
		text = text[:textLimit] + "\n…[truncated]"
	}
	if len(elems) > 7000 {
		elems = elems[:7000] + "\n…[more below]"
	}
	image := ""
	if path != "" {
		image = "[[image:" + path + "]]\n"
	}
	return fmt.Sprintf("page: %s\nurl: %s\n%s%s\nELEMENTS (use the [n] as \"ref\" in browser_act):\n%s\n\nTEXT:\n%s", title, loc, s.blockedNote(since), image, elems, text), nil
}

// blockedNote lists what the guard kept out since t.
func (s *Session) blockedNote(since time.Time) string {
	refs := s.guard.blockedSince(since)
	if len(refs) == 0 {
		return ""
	}
	hosts := make([]string, 0, len(refs))
	for _, r := range refs {
		hosts = append(hosts, r.host)
	}
	return "blocked: " + strings.Join(hosts, ", ") + " (on this computer or a private network, so not loaded; the owner can allow a host with skills.web.allow_hosts)\n"
}

// saveShot writes a screenshot the owner alone can read, and returns its path
// ("" if it couldn't be written).
func (s *Session) saveShot(kind string, png []byte) string {
	s.mu.Lock()
	s.shotSeq++
	seq := s.shotSeq
	s.mu.Unlock()
	path := filepath.Join(s.dataDir, fmt.Sprintf("%s-%d-%d.png", kind, time.Now().Unix(), seq))
	if err := os.WriteFile(path, png, 0o600); err != nil {
		s.log.Warn("save screenshot", "err", err)
		return ""
	}
	s.pruneShots(false)
	return path
}

func target(ref int, selector string) (string, error) {
	if ref > 0 {
		return fmt.Sprintf(`[data-oh-ref="%d"]`, ref), nil
	}
	if selector == "" {
		return "", fmt.Errorf("a step needs a ref (from ELEMENTS) or a selector")
	}
	return selector, nil
}

// Tools returns the browser tools bound to one shared session.
func Tools(cfg config.Browser, dataDir string) []tools.Tool {
	return NewSession(cfg, dataDir, nil).Tools()
}

// actStep is one browser_act step.
type actStep struct {
	Type      string `json:"type"`
	Ref       int    `json:"ref"`
	Selector  string `json:"selector"`
	Text      string `json:"text"`
	Value     string `json:"value"`
	Key       string `json:"key"`
	Direction string `json:"direction"`
	Enter     bool   `json:"enter"`
	Ms        int    `json:"ms"`
}

// actInput is browser_act's input.
type actInput struct {
	URL   string
	Steps string
}

func parseSteps(raw string) ([]actStep, error) {
	var steps []actStep
	if err := json.Unmarshal([]byte(raw), &steps); err != nil {
		return nil, fmt.Errorf("steps must be a JSON array: %w", err)
	}
	return steps, nil
}

// Tools returns browse_page, browser_act, browser_inspect, screenshot_page and browser_signin.
func (s *Session) Tools() []tools.Tool {
	act := tools.New("browser_act",
		"Do things on the current page (or a URL): click, type, select, press keys, scroll, wait, submit. Target elements by \"ref\" from the ELEMENTS list or by CSS selector. Returns the new page text, elements and screenshot. Keep each call to one screen's worth of steps and look at the result before the next. Put the final submit of anything irreversible (payment, sending, deleting) in its own call after showing the user the screenshot.",
		tools.Schema(map[string]tools.Prop{
			"url":   {Type: "string", Description: "Optional URL to open first"},
			"steps": {Type: "string", Description: "JSON array of steps: {\"type\":\"click\",\"ref\":12} | {\"type\":\"type\",\"ref\":3,\"text\":\"...\"} (replaces whatever the field holds, and checks it; add \"enter\":true to press Enter after; never type into a field again to fix it, this already replaces) | {\"type\":\"select\",\"ref\":5,\"value\":\"...\"} | {\"type\":\"press\",\"key\":\"Enter|Tab|Escape|ArrowDown|Backspace\"} (a combination like \"Meta+a\" or \"Shift+Tab\" presses the keys together) | {\"type\":\"scroll\",\"direction\":\"down|up\"} | {\"type\":\"wait\",\"selector\":\"css\"} | {\"type\":\"sleep\",\"ms\":1000} | {\"type\":\"submit\",\"ref\":9}. \"selector\" may replace \"ref\" in any step.", Required: true},
		}), tools.RiskWrite, s.runAct).WithRiskFor(s.paymentRisk)
	return append(s.teachTools(),
		tools.New("browse_page",
			"Open a URL in the twin's real browser (stays signed in to the user's sites) or read the current page. Returns the page text, numbered elements and a screenshot you can see. Use for anything JavaScript-heavy, anything behind a login, and as the first step of any task on a website. Addresses on this computer or the local network are refused unless the user allowed them.",
			tools.Schema(map[string]tools.Prop{
				"url":      {Type: "string", Description: "Absolute URL, or empty to look at the page as it is now"},
				"wait_for": {Type: "string", Description: "Optional CSS selector to wait for before reading"},
			}), tools.RiskRead,
			func(parent context.Context, call tools.Call) (string, error) {
				defer s.using(call.ChatKey)()
				var in struct {
					URL     string
					WaitFor string `json:"wait_for"`
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				since := time.Now()
				ctx, err := s.tab(false)
				if err != nil {
					return "", err
				}
				if in.URL != "" {
					tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
					defer cancel()
					var wait chromedp.Action = chromedp.Sleep(1200 * time.Millisecond)
					if in.WaitFor != "" {
						wait = chromedp.WaitVisible(in.WaitFor, chromedp.ByQuery)
					}
					if err := s.open(tctx, in.URL, wait); err != nil {
						return "", err
					}
				}
				return s.snapshot(ctx, 12000, since)
			}),
		checked{Func: act, check: s.checkAct, summary: s.actSummary}, // summary.go
		tools.New("browser_inspect", "List the interactive elements on the current page (or a URL) with their [n] refs, without the full text.",
			tools.Schema(map[string]tools.Prop{"url": {Type: "string", Description: "URL to open (empty for the current page)"}}), tools.RiskRead,
			func(parent context.Context, call tools.Call) (string, error) {
				defer s.using(call.ChatKey)()
				var in struct{ URL string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				ctx, err := s.tab(false)
				if err != nil {
					return "", err
				}
				tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				if in.URL != "" {
					if err := s.open(tctx, in.URL, chromedp.Sleep(1000*time.Millisecond)); err != nil {
						return "", err
					}
				}
				var out string
				if err := chromedp.Run(tctx, chromedp.Evaluate(inspectJS, &out)); err != nil {
					return "", err
				}
				return out, nil
			}),
		tools.New("browser_take_back",
			"Take the browser back from the user once they've told you they're finished with it (they took it over on the screen, or you handed a page to them). Only when they've said so: while they're still using it, leave it with them.",
			tools.Schema(map[string]tools.Prop{}), tools.RiskRead,
			func(context.Context, tools.Call) (string, error) {
				if !s.live.isHeld() {
					return "The browser is already yours.", nil
				}
				s.TakeOver(false)
				return "The browser is yours again: carry on with browse_page (no url reads the page as the user left it).", nil
			}),
		tools.New("screenshot_page", "Screenshot the current page (or a URL). Returns the image so you can see it, and its path.",
			tools.Schema(map[string]tools.Prop{
				"url":  {Type: "string", Description: "URL to open first (empty for the current page)"},
				"full": {Type: "boolean", Description: "Whole page rather than the visible part"},
			}), tools.RiskRead,
			func(parent context.Context, call tools.Call) (string, error) {
				defer s.using(call.ChatKey)()
				var in struct {
					URL  string
					Full bool
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				ctx, err := s.tab(false)
				if err != nil {
					return "", err
				}
				tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				if in.URL != "" {
					if err := s.open(tctx, in.URL, chromedp.Sleep(1500*time.Millisecond)); err != nil {
						return "", err
					}
				}
				var buf []byte
				shot := chromedp.CaptureScreenshot(&buf)
				if in.Full {
					shot = chromedp.FullScreenshot(&buf, 85)
				}
				if err := chromedp.Run(tctx, shot); err != nil {
					return "", err
				}
				path := s.saveShot("screenshot", buf)
				if path == "" {
					return "", fmt.Errorf("couldn't save the screenshot in %s", s.dataDir)
				}
				return "[[image:" + path + "]]", nil
			}),
		tools.New("browser_signin",
			"Hand the page to the user when they must do something themselves: log in, enter a code, fill in card details, solve a CAPTCHA or pick between options you've shown them. By default it stays in your own browser and the user does it on the presence screen (\"In the browser\", already under their control) or by clicking the orb. Say in ONE short message what they need to do there and that they should tell you when it's done; don't ask them to relay details you could read yourself afterwards. When they say done, carry on with browse_page / browser_act: that takes the page back. With no URL it hands over the page that is open now, as it is. Set window only if the page can't be used on the screen (a passkey or system prompt, or it keeps failing there): that opens a separate Chrome window instead.",
			tools.Schema(map[string]tools.Prop{
				"url":    {Type: "string", Description: "The site's login or start page; leave it empty to hand over the page that is open now"},
				"ask":    {Type: "string", Description: "What the user needs to do there, in a few words, shown beside the page (\"Tap Search, and solve the check if one appears\")"},
				"window": {Type: "boolean", Description: "Open a separate Chrome window instead of the screen; only when the screen can't do it"},
			}), tools.RiskRead, s.runSignin),
	)
}

// runSignin is browser_signin.
func (s *Session) runSignin(parent context.Context, call tools.Call) (string, error) {
	defer s.using(call.ChatKey)()
	var in struct {
		URL    string
		Ask    string
		Window bool
	}
	if err := tools.Decode(call, &in); err != nil {
		return "", err
	}
	if in.Window || s.cfg.HandOver == "window" {
		return s.signinWindow(parent, in.URL)
	}
	return s.signinScreen(parent, call.ChatKey, in.URL, strings.TrimSpace(in.Ask))
}

// signinScreen hands the page over inside the app: it stays in the twin's
// own browser and the owner drives it from the presence screen's live view.
func (s *Session) signinScreen(parent context.Context, chatKey, raw, ask string) (string, error) {
	if strings.TrimSpace(raw) != "" {
		if _, err := s.webURL(parent, raw); err != nil {
			return "", err
		}
		ctx, err := s.tab(false)
		if err != nil {
			return "", err
		}
		tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err := s.open(tctx, raw); err != nil {
			return "", err
		}
	} else {
		s.mu.Lock()
		open := s.ctx != nil && s.ctx.Err() == nil
		s.mu.Unlock()
		here := s.CurrentURL()
		if !open || here == "" || here == "about:blank" || strings.HasPrefix(here, "chrome-error:") {
			return "", errors.New("there's no page open in the browser to hand over. Give browser_signin the address to open")
		}
	}
	s.mu.Lock()
	s.handedTo = time.Now()
	s.mu.Unlock()
	s.handTo(ask)
	return s.handedOver(chatKey, s.CurrentURL(), ask), nil
}

// handedOver tells the daemon a page is the owner's now, and the twin where
// it is for the chat that asked: at this computer, the screen's address; in
// a messaging chat, the screen on the owner's computer, with no link that
// would only open there, and their phone when it was told.
func (s *Session) handedOver(chatKey, here, ask string) string {
	phoned := false
	if s.OnHandOver != nil {
		phoned = s.OnHandOver(here, ask)
	}
	screen := ""
	if s.ScreenURL != nil {
		screen = s.ScreenURL(chatKey)
	}
	const next = " When they say done, carry on with browse_page / browser_act."
	if screen != "" && s.ShowScreen != nil && s.ShowScreen(chatKey) {
		return "The page " + here + " is open in front of the user now, on the presence screen (" + screen + "), under their control. Tell them in one short message what to do there and to say when it's done." + next
	}
	if screen != "" {
		return "The page " + here + " is on the presence screen (" + screen + "), under the user's control, and the orb is showing. Tell them in one short message what to do there and to say when it's done; they can also click the orb to get there." + next
	}
	say := "It's on the presence screen on " + yourComputer()
	if phoned {
		say += ", and I've sent a notification to your phone"
	}
	return "The page " + here + " is on the presence screen on this computer, under the user's control. A link to the screen only opens there, so give none: tell them in one short message what's needed and where it is, like \"" + say + ".\"" + next
}

// yourComputer is the computer the twin runs on, as the owner would say it.
func yourComputer() string {
	if runtime.GOOS == "darwin" {
		return "your Mac"
	}
	return "the computer I run on"
}

// signinWindow is the fallback: a separate, visible Chrome window.
func (s *Session) signinWindow(parent context.Context, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return s.handOver()
	}
	if _, err := s.webURL(parent, raw); err != nil {
		return "", err
	}
	ctx, err := s.tab(!s.forceHeadless)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.handedTo = time.Now()
	s.mu.Unlock()
	tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := s.open(tctx, raw); err != nil {
		return "", err
	}
	return "A separate Chrome window is open at " + raw + " for the user (it's its own Chrome, with its own Dock icon). Tell them what to do there and to say when it's done; the window stays open for 20 minutes and their login is kept for next time.", nil
}

// handOver gives the user the page that is open now. In a visible window it
// stays exactly as it is; a hidden one has to be reopened in a window, which
// loads the page afresh.
func (s *Session) handOver() (string, error) {
	s.mu.Lock()
	open := s.ctx != nil && s.ctx.Err() == nil
	visible := open && (s.headed || s.forceHeadless)
	s.mu.Unlock()
	if !open {
		return "", errors.New("there's no page open in the browser to hand over. Give browser_signin the address to open")
	}
	here := s.CurrentURL()
	if !visible {
		if here == "" || here == "about:blank" || strings.HasPrefix(here, "chrome-error:") {
			return "", errors.New("there's no page open in the browser to hand over. Give browser_signin the address to open")
		}
		ctx, err := s.tab(true)
		if err != nil {
			return "", err
		}
		tctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err := s.open(tctx, here); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	s.handedTo = time.Now()
	s.mu.Unlock()
	if !visible {
		return "The page was in a hidden window, so it has been opened afresh in a visible one at " + here + " (anything already typed there is gone). Ask the user to do what's needed there and tell you when they're done.", nil
	}
	return "The browser window showing " + here + " is the user's now, exactly as it is. Ask them to fill in what's needed there and tell you when they're done; then carry on with browser_act.", nil
}

// runAct is browser_act. An approved call first makes sure the page is still
// the one the owner was asked about (stale.go).
func (s *Session) runAct(parent context.Context, call tools.Call) (string, error) {
	defer s.using(call.ChatKey)()
	var in actInput
	if err := tools.Decode(call, &in); err != nil {
		return "", err
	}
	steps, err := parseSteps(in.Steps)
	if err != nil {
		return "", err
	}
	if in.URL != "" {
		if _, err := s.webURL(parent, in.URL); err != nil {
			return "", err
		}
	}
	if err := s.checkStillAsked(call); err != nil {
		return "", err
	}
	since := time.Now()
	ctx, err := s.tab(false)
	if err != nil {
		return "", err
	}
	tctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if in.URL != "" {
		if err := s.open(tctx, in.URL, chromedp.Sleep(1200*time.Millisecond), chromedp.Evaluate(inspectJS, new(string))); err != nil {
			return "", err
		}
	}
	for i, st := range steps {
		var acts []chromedp.Action
		sel, terr := target(st.Ref, st.Selector)
		switch st.Type {
		case "click":
			if terr != nil {
				return "", terr
			}
			acts = []chromedp.Action{chromedp.WaitVisible(sel, chromedp.ByQuery), chromedp.ScrollIntoView(sel, chromedp.ByQuery), chromedp.Click(sel, chromedp.ByQuery), chromedp.Sleep(700 * time.Millisecond)}
		case "type":
			if terr != nil {
				return "", terr
			}
			acts = []chromedp.Action{typeInto(sel, st.Text)} // fill.go: replaces what's there, and checks it
			if st.Enter {
				acts = append(acts, chromedp.KeyEvent("\r"), chromedp.Sleep(1200*time.Millisecond))
			}
		case "select":
			if terr != nil {
				return "", terr
			}
			acts = []chromedp.Action{chromedp.SetValue(sel, st.Value, chromedp.ByQuery)}
		case "press":
			press, err := pressKey(st.Key) // fill.go: a key or a combination, never typed out
			if err != nil {
				return "", err
			}
			acts = []chromedp.Action{press, chromedp.Sleep(600 * time.Millisecond)}
		case "scroll":
			dy := 700
			if st.Direction == "up" {
				dy = -700
			}
			acts = []chromedp.Action{chromedp.ActionFunc(func(c context.Context) error {
				return input.DispatchMouseEvent(input.MouseWheel, 640, 450).WithDeltaY(float64(dy)).Do(c)
			}), chromedp.Sleep(500 * time.Millisecond)}
		case "wait":
			acts = []chromedp.Action{chromedp.WaitVisible(st.Selector, chromedp.ByQuery)}
		case "sleep":
			acts = []chromedp.Action{chromedp.Sleep(time.Duration(st.Ms) * time.Millisecond)}
		case "submit":
			if terr != nil {
				return "", terr
			}
			acts = []chromedp.Action{chromedp.Submit(sel, chromedp.ByQuery), chromedp.Sleep(1500 * time.Millisecond)}
		default:
			return "", fmt.Errorf("unknown step type %q", st.Type)
		}
		// Each step gets a short while of its own: an element that isn't
		// there fails in seconds, not after the whole call's time (a
		// silence the owner hears when talking to the twin).
		limit := stepWait
		if st.Type == "sleep" {
			limit += time.Duration(st.Ms) * time.Millisecond
		}
		sctx, scancel := context.WithTimeout(tctx, limit)
		err := chromedp.Run(sctx, acts...)
		scancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && tctx.Err() == nil {
				err = fmt.Errorf("nothing matching was visible on the page within %s", stepWait)
			}
			snap, _ := s.snapshot(ctx, 4000, since)
			return "", fmt.Errorf("step %d (%s) failed: %v\n%s", i+1, st.Type, err, snap)
		}
		// Re-number after each step so later refs are fresh.
		_ = chromedp.Run(tctx, chromedp.Evaluate(inspectJS, new(string)))
	}
	return s.snapshot(ctx, 8000, since)
}

// stepWait is how long one browser_act step waits for its element.
var stepWait = 12 * time.Second

// checked is a tool that can refuse a call before the owner is asked
// (tools.Checker); risk, spec and run come from the Func.
type checked struct {
	*tools.Func
	check   func(ctx context.Context, call tools.Call) error
	summary func(call tools.Call) string // the approval text (summary.go)
}

// Check implements tools.Checker.
func (c checked) Check(ctx context.Context, call tools.Call) error { return c.check(ctx, call) }

// CurrentURL reports where the browser is (for status), or "".
func (s *Session) CurrentURL() string {
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	if ctx == nil {
		return ""
	}
	var loc string
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := chromedp.Run(tctx, chromedp.Location(&loc)); err != nil {
		return ""
	}
	return loc
}

// paymentRisk raises browser_act to dangerous when a click or submit lands on
// something that looks like paying, buying or transferring money, so it
// always goes through approval with a screenshot, whatever the policy says.
func (s *Session) paymentRisk(ctx context.Context, call tools.Call) tools.Risk {
	var in actInput
	if tools.Decode(call, &in) != nil || in.Steps == "" {
		return tools.RiskWrite
	}
	steps, err := parseSteps(in.Steps)
	if err != nil {
		return tools.RiskWrite
	}
	if rePayment.MatchString(in.Steps) {
		return tools.RiskDangerous
	}
	s.mu.Lock()
	tab := s.ctx
	s.mu.Unlock()
	if stepsCommit(tab, in.URL, steps) { // commit.go: Send, Submit order, Book, Pay
		return tools.RiskDangerous
	}
	return tools.RiskWrite
}
