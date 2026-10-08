package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// Exposure says where a route or a listener answers.
type Exposure int

const (
	// LoopbackOnly answers on the loopback listener: this computer.
	LoopbackOnly Exposure = iota
	// Remote also answers on the listeners other devices reach.
	Remote
)

// listenerKind is what a connection arrived on. It, not the Host header or
// the source address, decides what a credential is worth.
type listenerKind int

const (
	kindLoopback listenerKind = iota // this computer
	kindRemote                       // a TLS listener other devices reach: device credentials only
	kindLegacy                       // plain-HTTP api.remote: device credentials, and the old shared key for view, chat and approve
	kindRefused                      // a connection that reached a loopback listener from elsewhere
)

type listener struct {
	kind                listenerKind
	via                 string   // loopback, lan, tailscale, relay r1…
	hosts               []string // kindRemote: the names it answers to
	admin, remotePolicy bool     // per-TLS-listener admin policy, when remotePolicy is set
}

type ctxKey int

const (
	listenerKey ctxKey = iota
	peerKey
)

// connContext tags each connection with its listener. The api.remote listener
// (classify) tells this computer's own connections, which arrive on a
// loopback address, from everyone else's. A loopback listener refuses a
// connection that didn't arrive on a loopback address.
func connContext(l listener, classify bool) func(context.Context, net.Conn) context.Context {
	return func(ctx context.Context, c net.Conn) context.Context {
		got := l
		local := addrOf(c.LocalAddr())
		switch {
		case classify && local.IsLoopback():
			got = listener{kind: kindLoopback, via: "loopback"}
		case classify:
			got = listener{kind: kindLegacy, via: viaFor(local)}
		case l.kind == kindLoopback && local.IsValid() && !local.IsLoopback():
			got = listener{kind: kindRefused}
		}
		return context.WithValue(ctx, listenerKey, got)
	}
}

func listenerFrom(ctx context.Context) listener {
	if l, ok := ctx.Value(listenerKey).(listener); ok {
		return l
	}
	return listener{kind: kindRefused} // a handler served some other way trusts nothing
}

func addrOf(a net.Addr) netip.Addr {
	if a == nil {
		return netip.Addr{}
	}
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.AddrPort().Addr().Unmap()
	}
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

var tailnet = netip.MustParsePrefix("100.64.0.0/10")

func viaFor(local netip.Addr) string {
	if tailnet.Contains(local) {
		return "tailscale"
	}
	return "lan"
}

// Peer is who is asking: a paired device, the master key, or nobody yet.
type Peer struct {
	Device   *devices.Device
	Master   bool // the master key (data/api.token)
	Loopback bool // arrived on the loopback listener
	ClientIP netip.Addr
	Via      string

	legacy bool   // arrived on the plain-HTTP api.remote listener
	cookie string // the device cookie it came with, if that's how
}

// Has reports whether the peer holds scope sc. The master key has every scope
// on the loopback listener; on the old api.remote listener it is what an
// old-style pairing code gave a terminal: view, chat and approve.
func (p Peer) Has(sc devices.Scope) bool {
	switch {
	case p.Master && p.Loopback:
		return true
	case p.Master && p.legacy:
		return sc != devices.Admin
	case p.Device != nil:
		return p.Device.Has(sc)
	}
	return false
}

// Lacks reports whether a request came through the API with a credential that
// doesn't hold sc. What arrives on the twin's own channels carries no peer,
// so it lacks nothing.
func (p Peer) Lacks(sc devices.Scope) bool {
	return (p.Device != nil || p.Master) && !p.Has(sc)
}

// WithPeer notes on ctx the peer a request comes from, as the API's own
// checks do before a handler runs: for code that hands a request on in
// process, and for tests of what a scope allows.
func WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey, p)
}

// PeerFrom returns the peer a route's handler is serving.
func PeerFrom(ctx context.Context) Peer {
	p, _ := ctx.Value(peerKey).(Peer)
	return p
}

func basePeer(r *http.Request, l listener) Peer {
	p := Peer{Loopback: l.kind == kindLoopback, Via: l.via, legacy: l.kind == kindLegacy}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		p.ClientIP = ap.Addr().Unmap()
	}
	return p
}

func ipString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

// Authz wraps a route's handler with what it needs.
type Authz interface {
	// Require needs a credential holding scope.
	Require(scope devices.Scope, h http.HandlerFunc) http.HandlerFunc
	// Public needs nothing (the handler checks its own proof, if any).
	Public(h http.HandlerFunc) http.HandlerFunc
	// Local needs the loopback listener and an admin credential.
	Local(h http.HandlerFunc) http.HandlerFunc
}

type authz struct {
	s   *Server
	exp Exposure
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// reachable reports whether a route with this exposure answers on l, before
// knowing who asks. Settings on other devices need AllowAdminRemote (and an
// admin device, checked once the peer is known).
func (a authz) reachable(l listener) bool {
	return a.exp == Remote || l.kind == kindLoopback || a.s.remoteAdmin(l)
}

func (a authz) Require(scope devices.Scope, h http.HandlerFunc) http.HandlerFunc {
	return a.require([]devices.Scope{scope}, false, h)
}

func (a authz) Local(h http.HandlerFunc) http.HandlerFunc {
	return a.require([]devices.Scope{devices.Admin}, true, h)
}

// require needs a credential holding at least one of scopes.
func (a authz) require(scopes []devices.Scope, loopbackOnly bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := listenerFrom(r.Context())
		if loopbackOnly && l.kind != kindLoopback || !a.reachable(l) {
			a.s.fail(w, r, http.StatusNotFound, a.s.notHere())
			return
		}
		// A browser attaches cookies to requests other sites make, so a
		// change carried by a cookie must come from the twin's own page.
		if !safeMethod(r.Method) && !sameOrigin(r, r.Header.Get("Authorization") == "" && carriesCookie(r)) {
			a.s.fail(w, r, http.StatusForbidden, a.s.crossSite())
			return
		}
		p, ok := a.s.authenticate(w, r, l)
		if !ok {
			a.s.unauthorized(w, r, p, l, loopbackOnly || a.exp == LoopbackOnly)
			return
		}
		if a.exp == LoopbackOnly && l.kind != kindLoopback && (p.Device == nil || !p.Device.Has(devices.Admin)) {
			a.s.fail(w, r, http.StatusNotFound, a.s.notHere())
			return
		}
		if !slices.ContainsFunc(scopes, p.Has) {
			a.s.fail(w, r, http.StatusForbidden, a.s.notAllowed(p))
			return
		}
		ctx := context.WithValue(r.Context(), peerKey, p)
		if p.Device != nil {
			var done func()
			ctx, done = a.s.live.track(ctx, p.Device.ID)
			defer done()
		}
		if p.Master && p.legacy {
			w.Header().Set(noticeHeader, "old-style pairing code; run `mirrin pair` on the home computer and connect again")
		}
		h(w, r.WithContext(ctx))
	}
}

func (a authz) Public(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := listenerFrom(r.Context())
		if !a.reachable(l) || a.exp == LoopbackOnly && l.kind != kindLoopback {
			a.s.fail(w, r, http.StatusNotFound, a.s.notHere())
			return
		}
		if !safeMethod(r.Method) && !sameOrigin(r, false) {
			a.s.fail(w, r, http.StatusForbidden, a.s.crossSite())
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), peerKey, basePeer(r, l))))
	}
}

// noticeHeader tells a client using an old-style pairing code what to do
// next.
const noticeHeader = "Mirrin-Notice"

// sameOrigin checks a state-changing request came from the twin's own pages:
// Sec-Fetch-Site, when sent, is same-origin (or none), and Origin, when sent,
// is this origin. Origin must be there when a cookie is the credential
// (required): browsers always send it on such requests.
func sameOrigin(r *http.Request, required bool) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return !required
	}
	scheme := "http://"
	if r.TLS != nil {
		scheme = "https://"
	}
	return strings.EqualFold(origin, scheme+r.Host)
}

// carriesCookie reports whether r brings any cookie that could authenticate
// it (so it must prove it came from the twin's own page).
func carriesCookie(r *http.Request) bool {
	for _, c := range r.Cookies() {
		if c.Name == cookieDev || c.Name == cookieSecure {
			return true
		}
	}
	return false
}

// Cookies. Browsers get a cookie holding their own device token: __Host-mirrin
// over TLS, mirrin_dev over plain HTTP (a __Host- cookie needs Secure).
const (
	cookieDev    = "mirrin_dev"
	cookieSecure = "__Host-mirrin"
	cookieMaxAge = 180 * 24 * 3600 // sliding: refreshed whenever a page loads
)

func deviceCookieName(r *http.Request) string {
	if r.TLS != nil {
		return cookieSecure
	}
	return cookieDev
}

// secureCookie is the device token in r's __Host- cookie, or "".
func secureCookie(r *http.Request) string {
	if c, err := r.Cookie(cookieSecure); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) setDeviceCookie(w http.ResponseWriter, r *http.Request, tok string) {
	c := &http.Cookie{Name: deviceCookieName(r), Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: cookieMaxAge}
	if r.TLS != nil {
		c.Secure = true
	}
	http.SetCookie(w, c)
}

// isMaster reports whether tok is the master key, comparing in constant time
// (hashes first, so not even the length leaks). On the loopback listener a key
// replaced since this run began (ResetMasterKey) still counts: the menu's
// links carry it until the twin restarts, and only this computer reaches
// that listener.
func (s *Server) isMaster(tok string, l listener) bool {
	if tok == "" {
		return false
	}
	got := sha256.Sum256([]byte(tok))
	same := func(key string) bool {
		want := sha256.Sum256([]byte(key))
		return key != "" && subtle.ConstantTimeCompare(got[:], want[:]) == 1
	}
	s.kmu.RLock()
	defer s.kmu.RUnlock()
	match := same(s.token)
	if l.kind == kindLoopback {
		for _, old := range s.retired {
			match = same(old) || match
		}
	}
	return match
}

// masterKey is the current master key.
func (s *Server) masterKey() string {
	s.kmu.RLock()
	defer s.kmu.RUnlock()
	return s.token
}

// authenticate works out who is asking, from the Authorization header, else
// the device cookie.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, l listener) (Peer, bool) {
	p := basePeer(r, l)
	if l.kind == kindRefused {
		return p, false
	}
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			return p, false
		}
		return s.fromToken(p, strings.TrimSpace(tok), l)
	}
	if c, err := r.Cookie(deviceCookieName(r)); err == nil && c.Value != "" {
		if got, ok := s.fromDevice(p, c.Value, l); ok {
			got.cookie = c.Value
			return got, true
		}
	}
	return p, false
}

func (s *Server) fromToken(p Peer, tok string, l listener) (Peer, bool) {
	if s.isMaster(tok, l) {
		switch l.kind {
		case kindLoopback:
			p.Master = true
			return p, true
		case kindLegacy:
			p.Master = true
			s.logLegacy(p)
			return p, true
		}
		return p, false // never on a listener other devices reach over TLS
	}
	return s.fromDevice(p, tok, l)
}

func (s *Server) fromDevice(p Peer, tok string, l listener) (Peer, bool) {
	store := s.Devices()
	d, ok := store.Authenticate(tok)
	if !ok || d.Local() && l.kind != kindLoopback {
		return p, false
	}
	store.Touch(d.ID, ipString(p.ClientIP))
	p.Device = &d
	return p, true
}

// logLegacy notes, at most every ten minutes, that something still uses the
// master key over plain HTTP: an old-style pairing code, or a program given
// the key (a hosted twin's gateway).
func (s *Server) logLegacy(p Peer) {
	now := time.Now().Unix()
	last := s.legacyLog.Load()
	if now-last < 600 || !s.legacyLog.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("the master key was used over plain HTTP from another computer; give that client a key of its own with `mirrin pair` (it can view, talk and approve with the master key there, but not change settings)", "from", ipString(p.ClientIP))
}

// browserName guesses a friendly name for a browser from its User-Agent.
func browserName(r *http.Request) string {
	ua := r.UserAgent()
	device := ""
	switch {
	case strings.Contains(ua, "iPhone"):
		device = "iPhone"
	case strings.Contains(ua, "iPad"):
		device = "iPad"
	case strings.Contains(ua, "Android"):
		device = "Android"
	case strings.Contains(ua, "CrOS"):
		device = "Chromebook"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		device = "Mac"
	case strings.Contains(ua, "Windows"):
		device = "Windows"
	case strings.Contains(ua, "Linux"):
		device = "Linux"
	}
	browser := ""
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"), strings.Contains(ua, "CriOS"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	switch {
	case browser != "" && device != "":
		return browser + " on " + device
	case device != "":
		return device
	case browser != "":
		return browser
	}
	return "A browser"
}

// liveSet holds what each device has open (event streams, running requests),
// so revoking it cuts them off at once instead of when they next reconnect.
type liveSet struct {
	mu sync.Mutex
	m  map[string]map[*context.CancelCauseFunc]struct{}
}

func (ls *liveSet) track(ctx context.Context, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	ls.mu.Lock()
	if ls.m == nil {
		ls.m = map[string]map[*context.CancelCauseFunc]struct{}{}
	}
	if ls.m[id] == nil {
		ls.m[id] = map[*context.CancelCauseFunc]struct{}{}
	}
	ls.m[id][&cancel] = struct{}{}
	ls.mu.Unlock()
	return ctx, func() {
		ls.mu.Lock()
		delete(ls.m[id], &cancel)
		if len(ls.m[id]) == 0 {
			delete(ls.m, id)
		}
		ls.mu.Unlock()
		cancel(nil)
	}
}

func (ls *liveSet) cut(id string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for c := range ls.m[id] {
		(*c)(errRevoked)
	}
}

func (s *Server) onDeviceChange(c devices.Change) {
	if c.Kind == devices.Revoked {
		s.live.cut(c.Device.ID)
	}
	s.addProgressChange(c) // pages_add.go: "Add your phone" lights its steps
}

// gate is the check every request passes before its route: the listener's
// Host names, no proxied requests on the loopback listener, a rate limit for
// other devices, and the security headers.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := listenerFrom(r.Context())
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		switch l.kind {
		case kindRefused:
			s.fail(w, r, http.StatusMisdirectedRequest, s.wrongAddress())
			return
		case kindLoopback:
			// Signed webhooks (Twilio through Tailscale Funnel or ngrok) and
			// /healthz may arrive through a tunnel, which keeps the public
			// name in Host. They prove themselves, or reveal nothing, so
			// neither check below applies to them.
			if s.proxyExempt(r.URL.Path) {
				break
			}
			// DNS rebinding: a site whose name now points at 127.0.0.1
			// still sends its own name.
			if !loopbackHostHeader(r.Host) {
				s.fail(w, r, http.StatusMisdirectedRequest, s.wrongAddress())
				return
			}
			// A proxy or tunnel on this computer would make every request
			// look local.
			if proxied(r) {
				s.fail(w, r, http.StatusMisdirectedRequest, s.proxiedErr())
				return
			}
		case kindRemote:
			if !l.answers(r.Host) {
				s.fail(w, r, http.StatusMisdirectedRequest, s.wrongAddress())
				return
			}
			h.Set("Strict-Transport-Security", "max-age=31536000")
			h.Set("Content-Security-Policy", remoteCSP)
		}
		if l.kind != kindLoopback {
			if ok, wait := s.limiter.allow(r.RemoteAddr); !ok {
				h.Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
				s.fail(w, r, http.StatusTooManyRequests, s.slowDown())
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// remoteCSP is the policy on listeners other devices reach. The pages keep
// their script and style inline, hence 'unsafe-inline'; everything is still
// same-origin, and nothing can frame them.
const remoteCSP = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func splitHost(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

// loopbackHostHeader: 127.0.0.1, localhost or [::1], on any port (an SSH
// tunnel may use another).
func loopbackHostHeader(host string) bool {
	switch strings.ToLower(splitHost(host)) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func (l listener) answers(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(splitHost(host), "."))
	for _, want := range l.hosts {
		if h == strings.ToLower(want) {
			return true
		}
	}
	return false
}

func proxied(r *http.Request) bool {
	for k := range r.Header {
		ck := http.CanonicalHeaderKey(k)
		if ck == "Forwarded" || ck == "Via" || ck == "X-Real-Ip" || strings.HasPrefix(ck, "X-Forwarded-") || strings.HasPrefix(ck, "Tailscale-") {
			return true
		}
	}
	return false
}

// proxyExempt reports whether path is a route that may come through a proxy
// or tunnel: /healthz, and the webhooks mounted with WithPublic, matched the
// way the mux matches them (a pattern ending in / covers what's under it).
func (s *Server) proxyExempt(path string) bool {
	if path == "/healthz" {
		return true
	}
	for pattern := range s.public {
		if path == pattern || strings.HasSuffix(pattern, "/") && strings.HasPrefix(path, pattern) {
			return true
		}
	}
	return false
}

// apiError is every refusal's body: a code for programs, a sentence for
// people, and what to do about it.
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
	// PairURL is where this computer pairs a device (notPaired on the
	// loopback listener): the "Add your phone" page.
	PairURL string `json:"pair_url,omitempty"`
}

func (s *Server) twinName() string {
	if s.name != "" {
		return s.name
	}
	return "your twin"
}

// notPaired is the answer to a device the twin doesn't know. local is set
// for what opens only on this computer (settings and Health): a screen
// paired with --screen couldn't open those anyway, so the menu item that
// opens the page is the way back.
func (s *Server) notPaired(l listener, local bool) apiError {
	fix := "On the computer " + s.twinName() + " runs on, choose Add your phone… from the menu bar icon (or type `mirrin pair --screen` in Terminal), then open the link it shows on this device."
	switch {
	case l.kind == kindLoopback && local:
		fix = "Open it again from " + s.twinName() + "'s menu (Health, Channels…, Accounts…, Your twin… or Routines…)."
	case l.kind == kindLoopback:
		fix = "Open this page from " + s.twinName() + "'s menu (Open screen…), or pair this device with Add your phone… in the same menu."
	}
	e := apiError{Error: "not_paired", Message: "This device isn't paired with " + s.twinName() + ".", Fix: fix}
	if l.kind == kindLoopback && s.pages != nil {
		e.PairURL = pairPagePath
	}
	return e
}

// pairPagePath is the "Add your phone" page, where this computer pairs a
// device (pages.go).
const pairPagePath = "/devices/add"

// unauthorized answers a request authenticate turned down.
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request, p Peer, l listener, local bool) {
	s.fail(w, r, http.StatusUnauthorized, s.notPaired(l, local))
}

func (s *Server) notAllowed(p Peer) apiError {
	what := "can't do that"
	cmd := "mirrin pair"
	if p.Device != nil {
		what = "was paired to " + DescribeScopes(p.Device.Scopes) + " only, so it can't do that"
		switch p.Device.Kind {
		case devices.KindCLI:
			cmd = "mirrin pair --scopes view,chat,approve"
		case devices.KindKiosk:
			cmd = "mirrin pair --screen"
		default:
			cmd = "mirrin pair --screen --scopes view,chat,approve"
		}
	}
	return apiError{Error: "not_allowed", Message: "This device " + what + ".",
		Fix: "To give it more, pair it again: on the computer " + s.twinName() + " runs on, type `" + cmd + "` in Terminal."}
}

// scopeWords says what a scope lets a device do.
func scopeWords(sc devices.Scope) string {
	switch sc {
	case devices.View:
		return "see the screen"
	case devices.Chat:
		return "talk"
	case devices.Approve:
		return "approve requests"
	case devices.Admin:
		return "change settings"
	}
	return string(sc)
}

func (s *Server) notHere() apiError {
	return apiError{Error: "not_here", Message: "This page only opens on the computer " + s.twinName() + " runs on.",
		Fix: "Open it there, from " + s.twinName() + "'s menu."}
}

func (s *Server) crossSite() apiError {
	return apiError{Error: "cross_site", Message: "That request came from another website, so I ignored it.",
		Fix: "Use " + s.twinName() + "'s own pages to make changes."}
}

func (s *Server) wrongAddress() apiError {
	return apiError{Error: "wrong_address", Message: "This address isn't one " + s.twinName() + " answers on.",
		Fix: "On this computer, open " + s.twinName() + " from its menu bar icon. To use a phone or another device, pair it first: menu bar → Add your phone…"}
}

func (s *Server) proxiedErr() apiError {
	return apiError{Error: "proxied", Message: s.twinName() + "'s local address doesn't take requests passed on by a proxy or tunnel: they would look as if they came from this computer.",
		Fix: "To use " + s.twinName() + " from another device, pair it first: on this computer, menu bar → Add your phone…"}
}

func (s *Server) slowDown() apiError {
	return apiError{Error: "slow_down", Message: "Too many requests from this address.", Fix: "Wait a minute, then try again."}
}

// wantsPage reports whether a person's browser is navigating (so a refusal is
// shown as a page, not JSON).
func wantsPage(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(r.Header.Get("Accept"), "text/html")
}

// fail refuses a request with a body a page can show.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, e apiError) {
	w.Header().Set("Cache-Control", "no-store")
	if wantsPage(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		w.WriteHeader(status)
		_ = errorPage.Execute(w, struct {
			Name string
			apiError
		}{s.twinName(), e})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

// codeSpans shows `a command` as <code>, escaping everything first.
func codeSpans(text string) template.HTML {
	parts := strings.Split(template.HTMLEscapeString(text), "`")
	var b strings.Builder
	for i, part := range parts {
		switch {
		case i%2 == 1 && i < len(parts)-1:
			b.WriteString("<code>" + part + "</code>")
		case i%2 == 1:
			b.WriteString("`" + part) // an unpaired backtick stays as it was
		default:
			b.WriteString(part)
		}
	}
	return template.HTML(b.String())
}

var errorPage = template.Must(template.New("e").Funcs(template.FuncMap{"code": codeSpans}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}}</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#07080b;color:#eceef3;font:17px/1.5 -apple-system,system-ui,sans-serif}
main{max-width:30rem;padding:2rem}h1{font-size:1.3rem;margin:0 0 .6rem}p{color:#b4bac7;margin:.4rem 0}code{background:#171a23;padding:.1rem .35rem;border-radius:.3rem;color:#eceef3}</style>
</head><body><main><h1>{{code .Message}}</h1>{{if .Fix}}<p>{{code .Fix}}</p>{{end}}{{if .PairURL}}<p><a href="{{.PairURL}}" style="color:#7aa2ff">Pair this device</a></p>{{end}}</main></body></html>
`))

// ipLimiter is a token bucket per client address, for listeners other devices
// reach.
type ipLimiter struct {
	mu    sync.Mutex
	rate  float64 // tokens a second
	burst float64
	b     map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newIPLimiter(rate, burst float64) *ipLimiter {
	return &ipLimiter{rate: rate, burst: burst, b: map[string]*bucket{}}
}

func (l *ipLimiter) allow(remoteAddr string) (bool, time.Duration) {
	ip := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		ip = h
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.b) > 10000 {
		for k, v := range l.b {
			if now.Sub(v.at) > time.Minute {
				delete(l.b, k)
			}
		}
	}
	bk := l.b[ip]
	if bk == nil {
		bk = &bucket{tokens: l.burst, at: now}
		l.b[ip] = bk
	}
	bk.tokens = min(l.burst, bk.tokens+now.Sub(bk.at).Seconds()*l.rate)
	bk.at = now
	if bk.tokens < 1 {
		return false, time.Duration((1 - bk.tokens) / l.rate * float64(time.Second))
	}
	bk.tokens--
	return true, 0
}

// isLoopbackHost reports whether an api.listen host is loopback.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

func isUnspecified(host string) bool {
	if host == "" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsUnspecified()
}

// LoopbackAddr is the loopback address for an api.listen value: itself when
// it is loopback, else 127.0.0.1 on the same port (the api.remote listener
// serves this computer there too).
func LoopbackAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || isLoopbackHost(host) {
		return listen
	}
	return net.JoinHostPort("127.0.0.1", port)
}
