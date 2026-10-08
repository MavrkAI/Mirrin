package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// A sign-in window for sites that refuse a browser under automation.
// Google's sign-in says "This browser or app may not be secure" to a
// Chrome that something drives, however it is dressed, so the owner can't
// sign in on the presence screen's live view of the twin's browser. For
// those pages the twin opens ordinary Chrome on this computer, with its own
// profile and no remote control, and the owner signs in there. When they
// say done, the twin's next browser tool closes that window and starts its
// own browser on the same profile, signed in.

// refuseAutomation are the sites whose sign-in refuses a driven browser: a
// domain, with every host under it, or a prefix ending in "." for every
// country's version of a host. Google's services (Gmail, Drive) send a
// visitor who isn't signed in on to accounts.google.com, so a hand-over of
// any of their pages is a Google sign-in. Keep it short: everything else is
// handed over on the screen.
var refuseAutomation = []string{"accounts.google.", "google.com", "gmail.com"}

// refusesAutomation says whether raw is a page whose sign-in turns a driven
// browser away.
func refusesAutomation(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	for _, h := range refuseAutomation {
		if strings.HasSuffix(h, ".") {
			if strings.HasPrefix(host, h) {
				return true
			}
		} else if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// plainURL is the page to open in the sign-in window for this hand-over, or
// "" to hand it over as usual: a sign-in page that refuses a driven
// browser (raw, or the page open now), from a chat at this computer or
// asked for in a window.
func (s *Session) plainURL(chatKey, raw string, window bool) string {
	page := strings.TrimSpace(raw)
	if page == "" {
		page = s.CurrentURL()
	}
	if !refusesAutomation(page) {
		return ""
	}
	if !window && s.cfg.HandOver != "window" && !s.atThisComputer(chatKey) {
		return ""
	}
	return page
}

// windowWhere adds to a window hand-over's result, for a chat that isn't at
// this computer (WhatsApp, a call), that the window is only on this
// computer: the owner may be away from it, and the twin mustn't say the
// page is in front of them.
func (s *Session) windowWhere(chatKey string) func(string, error) (string, error) {
	return func(out string, err error) (string, error) {
		if err != nil || s.atThisComputer(chatKey) {
			return out, err
		}
		return out + " That window is on this computer, not where this chat is, so they may not be in front of it: tell them it's waiting on " + yourComputer() + " for when they're there, never that it's in front of them now.", nil
	}
}

// atThisComputer says whether the chat is at this computer (voice, the
// terminal, the screen), where a window can open in front of the owner.
func (s *Session) atThisComputer(chatKey string) bool {
	return s.ScreenURL != nil && s.ScreenURL(chatKey) != ""
}

// plainWindow is the ordinary Chrome the owner signs in with.
type plainWindow struct {
	cmd  *exec.Cmd
	done chan struct{} // closed when it has quit
}

// plainArgs are the switches for the sign-in window: the twin's profile and
// the same guard on what it may reach, and nothing that drives it or marks
// it as driven (no remote debugging, no automation switches).
func (s *Session) plainArgs(proxy, raw string) []string {
	flags := s.chromeFlags(!s.forceHeadless, proxy, "")
	delete(flags, "disable-blink-features") // nothing to hide in a Chrome nobody drives
	delete(flags, "user-agent")
	names := make([]string, 0, len(flags))
	for n := range flags {
		names = append(names, n)
	}
	sort.Strings(names)
	args := make([]string, 0, len(names)+1)
	for _, n := range names {
		switch v := flags[n].(type) {
		case bool:
			if v {
				args = append(args, "--"+n)
			}
		case string:
			args = append(args, "--"+n+"="+v)
		default:
			args = append(args, fmt.Sprintf("--%s=%v", n, v))
		}
	}
	return append(args, raw)
}

// signinPlain opens raw in ordinary Chrome on this computer, with the
// twin's profile, for the owner to sign in themselves.
func (s *Session) signinPlain(parent context.Context, raw string) (string, error) {
	if _, err := s.webURL(parent, raw); err != nil {
		return "", err
	}
	exe := findChrome()
	if exe == "" {
		return "", errors.New("Chrome isn't installed on this computer, so there's no window to sign in with")
	}
	proxy, err := s.guard.start()
	if err != nil {
		return "", fmt.Errorf("could not start the browser's safety check: %w", err)
	}
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	// One Chrome per profile: the twin's own steps aside (its logins are
	// on disk, and it starts again on the same profile afterwards).
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errClosed
	}
	s.stopLocked()
	w := s.plain
	s.mu.Unlock()
	if w != nil {
		select {
		case <-w.done:
			w = nil
		default:
		}
	}
	cmd := exec.Command(exe, s.plainArgs(proxy, raw)...)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("couldn't open Chrome: %w", err)
	}
	if w != nil {
		// Already open: Chrome passes the page to that window and quits.
		go func() { _ = cmd.Wait() }()
	} else {
		w = &plainWindow{cmd: cmd, done: make(chan struct{})}
		go func(w *plainWindow) { _ = w.cmd.Wait(); close(w.done) }(w)
	}
	s.mu.Lock()
	s.plain = w
	s.handedTo = time.Now()
	s.lastUse = time.Now()
	s.mu.Unlock()
	return "A Chrome window is open on this computer at " + raw + ", for the user to sign in themselves. It's ordinary Chrome with your browser's profile, so the site accepts it, and the login is kept for you. Tell them in one short message to sign in there and say when it's done. Then carry on with browse_page: that closes the window and you're signed in.", nil
}

// plainOpen says whether the sign-in window is still open.
func (s *Session) plainOpen() bool {
	s.mu.Lock()
	w := s.plain
	s.mu.Unlock()
	if w == nil {
		return false
	}
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}

// plainQuitWait is how long the sign-in window has to quit, saving its
// cookies, before it is killed.
var plainQuitWait = 10 * time.Second

// closePlain closes the sign-in window, if open, and waits for it to quit,
// so the twin's own Chrome can have the profile and finds the login there.
func (s *Session) closePlain() {
	s.mu.Lock()
	w := s.plain
	s.plain = nil
	s.mu.Unlock()
	if w == nil || w.cmd.Process == nil {
		return
	}
	select {
	case <-w.done:
		return
	default:
	}
	// Asked to quit, Chrome writes its cookies out first; Windows has no
	// such signal, and Chrome there saves them as it goes.
	if runtime.GOOS == "windows" || w.cmd.Process.Signal(os.Interrupt) != nil {
		_ = w.cmd.Process.Kill()
	}
	select {
	case <-w.done:
	case <-time.After(plainQuitWait):
		_ = w.cmd.Process.Kill()
		select {
		case <-w.done:
		case <-time.After(killWait):
		}
	}
}
