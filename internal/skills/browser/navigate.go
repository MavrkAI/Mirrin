package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// reScheme spots an address that already names its scheme ("mailto:",
// "file:"), as opposed to a bare host with a port ("localhost:8080").
var reScheme = regexp.MustCompile(`(?i)^(about|blob|chrome|chrome-[a-z]+|data|devtools|file|filesystem|ftp|intent|javascript|mailto|sms|tel|view-source|ws|wss):`)

// webURL checks an address the model asked the browser to open: a web page
// (http or https) that isn't on this computer or the local network, unless
// the owner allowed it. A bare "example.com" gets https://. The guard checks
// every connection again, redirects included; this only gives the plain
// answer before anything loads.
func (s *Session) webURL(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "about:blank" {
		return raw, nil
	}
	if !strings.Contains(raw, "://") && !reScheme.MatchString(raw) {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" && (u.Scheme == "http" || u.Scheme == "https") {
		return "", fmt.Errorf("%q isn't a web address I can open", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("I only open web pages (http:// or https://) in the browser, not %s: addresses", strings.ToLower(u.Scheme))
	}
	if err := s.guard.check(ctx, u.Hostname(), false); err != nil {
		// A name that doesn't exist, unless the owner's proxy may know it
		// (an intranet name, say).
		var de *dialError
		if errors.As(err, &de) {
			if pu, perr := s.guard.proxyFor(u); perr == nil && pu != nil {
				return u.String(), nil
			}
		}
		return "", err
	}
	return u.String(), nil
}

// open loads raw in the tab (after webURL), then runs then. A page the guard
// stopped, the address itself or a redirect to somewhere private, comes back
// as a plain refusal, and a page that wouldn't load says why in words.
func (s *Session) open(ctx context.Context, raw string, then ...chromedp.Action) error {
	u, err := s.webURL(ctx, raw)
	if err != nil {
		return err
	}
	since := time.Now()
	err = chromedp.Run(ctx, append([]chromedp.Action{chromedp.Navigate(u)}, then...)...)
	if why := s.stoppedAt(ctx, u, since, err); why != nil {
		return why
	}
	if err != nil {
		return loadError(u, err)
	}
	return nil
}

// stoppedAt reports why the page at u didn't load, if the guard refused
// something on the way since t or couldn't connect to it.
func (s *Session) stoppedAt(ctx context.Context, u string, since time.Time, navErr error) error {
	refs := s.guard.blockedSince(since)
	fails := s.guard.failedSince(since)
	if len(refs) == 0 && len(fails) == 0 {
		return nil
	}
	var loc, shown string
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = chromedp.Run(lctx, chromedp.Location(&loc),
		chromedp.Evaluate(`(document.querySelector('meta[name="mirrin-guard"]') || {}).content || ''`, &shown))
	cancel()
	want, _ := url.Parse(u)
	from := ""
	if want != nil {
		from = want.Hostname()
	}
	// A page that didn't load at all; a wait that timed out on a loaded page
	// with an image refused doesn't count.
	failed := strings.HasPrefix(loc, "chrome-error:") || navErr != nil && strings.Contains(navErr.Error(), "net::ERR_")
	here := ""
	if l, err := url.Parse(loc); err == nil {
		here = l.Hostname()
	}
	for _, r := range refs {
		// The page itself went nowhere, or it is the guard's "blocked" page.
		if !failed && !strings.EqualFold(r.host, here) {
			continue
		}
		out := *r
		if !strings.EqualFold(r.host, from) {
			out.from = from
		}
		return &out
	}
	// The guard couldn't connect: the page failed (an https page's tunnel),
	// or what loaded is the guard's own "couldn't open" page. The site's
	// own host is the likeliest culprit; a later hop the next.
	if !failed && shown != "failed" {
		return nil
	}
	for _, host := range []string{here, from} {
		for _, f := range fails {
			if host != "" && strings.EqualFold(f.host, host) {
				return f
			}
		}
	}
	if failed && len(fails) > 0 && navErr != nil && strings.Contains(navErr.Error(), "ERR_TUNNEL_CONNECTION_FAILED") {
		return fails[0] // a redirect's host
	}
	return nil
}

// loadError puts a page that wouldn't load in words.
func loadError(u string, err error) error {
	host := u
	if p, perr := url.Parse(u); perr == nil && p.Host != "" {
		host = p.Host
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// Chrome may stop at the keychain prompt only when a page first
		// needs its saved logins.
		if keychainLocked(context.Background()) {
			return fmt.Errorf("%s didn't load: this Mac's login keychain is locked, so Chrome is waiting at a keychain password prompt, which nobody may be at the screen to answer. %s Then ask me again", host, unlockFix)
		}
		return fmt.Errorf("%s took too long to load, so I stopped waiting. Try again, or try later", host)
	case strings.Contains(msg, "ERR_NAME_NOT_RESOLVED"):
		return fmt.Errorf("I couldn't find %s. Check the address", host)
	case strings.Contains(msg, "ERR_INTERNET_DISCONNECTED"):
		return fmt.Errorf("this computer seems to be offline, so I couldn't open %s", host)
	case strings.Contains(msg, "ERR_CONNECTION_REFUSED"):
		return fmt.Errorf("%s refused the connection. The site may be down", host)
	case strings.Contains(msg, "TIMED_OUT"):
		return fmt.Errorf("%s didn't answer in time. The site may be down, or try again", host)
	case strings.Contains(msg, "ERR_CERT_") || strings.Contains(msg, "ERR_SSL_"):
		return fmt.Errorf("%s has a security certificate problem, so I didn't open it", host)
	case strings.Contains(msg, "ERR_TUNNEL_CONNECTION_FAILED") || strings.Contains(msg, "ERR_PROXY_CONNECTION_FAILED"):
		return fmt.Errorf("the browser couldn't connect to %s. The site may be down, or try again", host)
	}
	return fmt.Errorf("couldn't open %s: %w", u, err)
}
