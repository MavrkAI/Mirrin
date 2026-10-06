package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Chrome ranks proxy settings: a company policy first, then an extension,
// and only then the --proxy-server it was started with. So every start
// checks that Chrome really talks to the guard: it opens an address only the
// guard answers (a name under .invalid, which never resolves), and if the
// guard never hears of it, that Chrome is stopped.

// canaryDomain is the name only the guard answers for.
const canaryDomain = "mirrin-guard.invalid"

// canaryWait is how long a start waits for the guard to hear from Chrome.
var canaryWait = 3 * time.Second

// canaryAnswering, when set, runs after the guard has heard the canary and
// before it answers (tests).
var canaryAnswering func()

// errOtherProxy is Chrome using a proxy other than the guard.
var errOtherProxy = errors.New("chrome is not using the guard proxy")

// expectCanary makes a fresh name under canaryDomain and returns it with a
// channel closed once Chrome asks the guard for it.
func (g *guard) expectCanary() (host string, seen <-chan struct{}, done func()) {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	nonce := hex.EncodeToString(b)
	ch := make(chan struct{})
	g.mu.Lock()
	if g.canaries == nil {
		g.canaries = map[string]chan struct{}{}
	}
	g.canaries[nonce] = ch
	g.mu.Unlock()
	return nonce + "." + canaryDomain, ch, func() {
		g.mu.Lock()
		delete(g.canaries, nonce)
		g.mu.Unlock()
	}
}

// canary answers a request for the canary name, noting a name it is waiting
// for, and reports whether it did. Nothing about it leaves this machine.
func (g *guard) canary(w http.ResponseWriter, r *http.Request) bool {
	host := r.Host
	if r.Method != http.MethodConnect && r.URL.IsAbs() {
		host = r.URL.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host != canaryDomain && !strings.HasSuffix(host, "."+canaryDomain) {
		return false
	}
	nonce, _, _ := strings.Cut(host, ".")
	g.mu.Lock()
	if ch, ok := g.canaries[nonce]; ok {
		close(ch)
		delete(g.canaries, nonce)
	}
	g.mu.Unlock()
	if canaryAnswering != nil {
		canaryAnswering()
	}
	if r.Method == http.MethodConnect {
		// No tunnel: Chrome falls back to plain http, which is answered below.
		http.Error(w, "Mirrin's browser check", http.StatusNotFound)
		return true
	}
	w.WriteHeader(http.StatusNoContent) // the tab stays where it is
	return true
}

// checkGuardInUse opens the canary address in a just-started Chrome and
// waits for the guard to hear of it.
func (s *Session) checkGuardInUse(ctx context.Context) error {
	host, seen, done := s.guard.expectCanary()
	defer done()
	cctx, cancel := context.WithTimeout(ctx, canaryWait)
	navigated := make(chan struct{})
	go func() {
		defer close(navigated)
		_ = chromedp.Run(cctx, chromedp.ActionFunc(func(c context.Context) error {
			_, _, _, _, err := page.Navigate("http://" + host + "/").Do(c)
			return err
		}))
	}()
	defer func() { cancel(); <-navigated }()
	select {
	case <-seen:
		// Let Chrome take the guard's answer before the tab is handed out:
		// an answer that lands while the next page is starting to load
		// cancels that page (net::ERR_ABORTED).
		<-navigated
		return nil
	case <-cctx.Done():
		select {
		case <-seen:
			return nil
		default:
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errOtherProxy
	}
}

// otherProxyError is a Chrome that uses another proxy, in words.
func (s *Session) otherProxyError(err error) *launchError {
	return &launchError{
		what: "Chrome is set to use another proxy (a company policy or an extension), so I can't keep it off your local network, and I've stopped it.",
		fix: fmt.Sprintf("If you added a proxy or VPN extension in my browser window, remove it; the sure way is to delete the folder %s, which starts my browser afresh (you'll sign in to sites again). On a work computer, ask whoever manages it about Chrome's proxy policy. Then ask me again.",
			filepath.Join(s.dataDir, "chrome-profile")),
		err: err,
	}
}
