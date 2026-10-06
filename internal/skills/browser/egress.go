package browser

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/proxy"

	"github.com/MavrkAI/Mirrin/internal/skills/web"
)

// guard is a small forward proxy on 127.0.0.1 that Chrome sends all of its
// traffic through. Every page, redirect, image, script, WebSocket and worker
// is checked the way fetch_url is: the browser reaches the public internet,
// and on this computer or the local network only what skills.web.allow_hosts
// names. The guard dials the very address it checked, so a name can't point
// somewhere else between the check and the connection.
type guard struct {
	resolve  func(ctx context.Context, host string) ([]netip.Addr, error)
	upstream func(target *url.URL) (*url.URL, error) // the owner's own proxy for target, if any (set at start)
	dialer   *net.Dialer
	// dialAddr connects to one checked address (a variable for tests).
	dialAddr func(ctx context.Context, network, addr string) (net.Conn, error)
	// proxyTLS, if set, is the TLS setup for an https:// proxy (tests).
	proxyTLS *tls.Config
	// unsupported names a proxy setting of this computer the guard can't
	// follow (a PAC script, say), so the browser connects directly.
	unsupported string

	mu       sync.Mutex
	allow    web.Allowlist
	blocks   []block // recent refusals, newest last
	fails    []fail  // recent connections that didn't work, newest last
	canaries map[string]chan struct{}
	ln       net.Listener
	srv      *http.Server
	addr     string
}

// block is one refused connection.
type block struct {
	at  time.Time
	ref *refusedError
}

// fail is one connection that didn't work.
type fail struct {
	at  time.Time
	err *dialError
}

// maxBlocks is how many refusals the guard remembers.
const maxBlocks = 64

func newGuard() *guard {
	g := &guard{
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dialer: &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second},
	}
	g.dialAddr = g.dialer.DialContext
	return g
}

// unsupportedSetting names this computer's proxy setting the guard can't
// follow, "" when there is none.
func (g *guard) unsupportedSetting() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.unsupported
}

// proxyFor is the owner's own proxy for target, nil for none. The settings
// are read once, when first needed.
func (g *guard) proxyFor(target *url.URL) (*url.URL, error) {
	g.mu.Lock()
	if g.upstream == nil {
		g.upstream, g.unsupported = ownProxy()
	}
	up := g.upstream
	g.mu.Unlock()
	return up(target)
}

func (g *guard) setAllow(a web.Allowlist) {
	g.mu.Lock()
	g.allow = a
	g.mu.Unlock()
}

func (g *guard) allowlist() web.Allowlist {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.allow
}

// start listens on a loopback port (once) and returns the proxy address for
// Chrome's --proxy-server.
func (g *guard) start() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.srv != nil {
		return g.addr, nil
	}
	if g.upstream == nil {
		g.upstream, g.unsupported = ownProxy()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	direct := http.DefaultTransport.(*http.Transport).Clone()
	direct.Proxy = nil
	direct.DialContext = g.dial
	proxied := http.DefaultTransport.(*http.Transport).Clone()
	proxied.Proxy = func(r *http.Request) (*url.URL, error) {
		if u, ok := r.Context().Value(proxyKey{}).(*url.URL); ok {
			return u, nil
		}
		return nil, errors.New("no proxy chosen for this request") // never dial the target unchecked
	}
	rp := &httputil.ReverseProxy{
		// The request already names where it goes (Chrome sends a proxy the
		// full URL); nothing is rewritten and no X-Forwarded-For is added.
		// The query goes on exactly as Chrome sent it: the proxy would
		// otherwise drop parts it can't parse ("a=1;b=2").
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.Out.URL.RawQuery = pr.In.URL.RawQuery },
		Transport:     &guardTransport{g: g, direct: direct, proxied: proxied},
		FlushInterval: -1,
		ErrorHandler:  g.proxyError,
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	g.ln = ln
	g.addr = "http://" + ln.Addr().String()
	g.srv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case g.canary(w, r):
			case r.Method == http.MethodConnect:
				g.tunnel(w, r)
			case r.URL.IsAbs() && (r.URL.Scheme == "http" || r.URL.Scheme == "https"):
				rp.ServeHTTP(w, r)
			default:
				http.Error(w, "This is the Mirrin browser's safety check, not a web page.", http.StatusBadRequest)
			}
		}),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func(srv *http.Server) { _ = srv.Serve(ln) }(g.srv)
	return g.addr, nil
}

// close stops listening. Tunnels already open end with Chrome.
func (g *guard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.srv != nil {
		_ = g.srv.Close()
		g.srv, g.ln, g.addr = nil, nil, ""
	}
}

// refusedError is the browser declining to reach a private address, or one
// it couldn't check before handing it to the owner's proxy.
type refusedError struct {
	host string
	ip   netip.Addr
	err  error // why where host points couldn't be checked
	from string
}

func (r *refusedError) Error() string {
	allow := "If you trust it, add it to skills.web.allow_hosts in the config."
	if r.err != nil {
		return fmt.Sprintf("I won't open %s through your proxy: I couldn't check where it points (%v). %s", r.host, r.err, allow)
	}
	where := fmt.Sprintf("it points to %s, which is", r.ip)
	if r.ip.String() == r.host {
		where = "that address is"
	}
	if r.from != "" {
		return fmt.Sprintf("%s sent the browser on to %s, but %s on this computer or a private network, so I stopped there. %s", r.from, r.host, where, allow)
	}
	return fmt.Sprintf("I won't open %s: %s on this computer or a private network. %s", r.host, where, allow)
}

// check reports whether host may be opened. strict (for a host the owner's
// proxy will dial for us) refuses a name that can't be looked up or has any
// private address; otherwise a name is refused only when every address it has
// is private, as the dial then skips private ones. A name that doesn't exist
// is a *dialError; any other lookup failure is left for the dial to report.
func (g *guard) check(ctx context.Context, host string, strict bool) error {
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return nil
	}
	allow := g.allowlist()
	if allow.Host(host) {
		return nil
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = g.resolve(ctx, host); err != nil {
		if strict {
			return &refusedError{host: host, err: err}
		}
		if notFound(err) {
			return &dialError{host: host, err: err}
		}
		return nil
	}
	var refused *refusedError
	for _, ip := range ips {
		ip = ip.Unmap()
		if web.PrivateAddr(ip) && !allow.Addr(ip) {
			if strict {
				return &refusedError{host: host, ip: ip}
			}
			if refused == nil {
				refused = &refusedError{host: host, ip: ip}
			}
			continue
		}
		if !strict {
			return nil // a public address: the dial will use it
		}
	}
	if refused != nil {
		return refused
	}
	return nil
}

// dial connects to addr after checking where its name points, and dials the
// checked address itself. A connection that doesn't work is remembered, so
// a tool can say why in words.
func (g *guard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := g.dialChecked(ctx, network, addr)
	var de *dialError
	if errors.As(err, &de) && ctx.Err() == nil {
		g.noteFail(de)
	}
	return c, err
}

func (g *guard) dialChecked(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	allow := g.allowlist()
	if allow.Host(host) {
		c, err := g.dialAddr(ctx, network, addr)
		if err != nil {
			return nil, &dialError{host: host, err: err}
		}
		return c, nil
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = g.resolve(ctx, host); err != nil {
		return nil, &dialError{host: host, err: err}
	}
	var refused *refusedError
	var addrs []string
	for _, ip := range ips {
		ip = ip.Unmap()
		if web.PrivateAddr(ip) && !allow.Addr(ip) {
			refused = &refusedError{host: host, ip: ip}
			continue
		}
		// Dial the checked address itself, so nothing can re-resolve the name.
		addrs = append(addrs, net.JoinHostPort(ip.String(), port))
	}
	if len(addrs) > 0 {
		c, err := g.dialFirst(ctx, network, addrs)
		if err != nil {
			return nil, &dialError{host: host, err: err}
		}
		return c, nil
	}
	if refused != nil {
		g.note(refused)
		return nil, refused
	}
	return nil, &dialError{host: host, err: &net.DNSError{Err: "no address found", Name: host, IsNotFound: true}}
}

// dialError is a connection the guard was allowed to make that didn't work.
type dialError struct {
	host string
	err  error
}

func (d *dialError) Unwrap() error { return d.err }

// Error says what happened in the words a page that won't load gets.
func (d *dialError) Error() string {
	var ne net.Error
	switch {
	case notFound(d.err):
		return fmt.Sprintf("I couldn't find %s. Check the address", d.host)
	case errors.Is(d.err, syscall.ECONNREFUSED):
		return fmt.Sprintf("%s refused the connection. The site may be down", d.host)
	case errors.Is(d.err, context.DeadlineExceeded) || errors.As(d.err, &ne) && ne.Timeout():
		return fmt.Sprintf("%s didn't answer in time. The site may be down, or try again", d.host)
	}
	return fmt.Sprintf("the browser couldn't connect to %s. The site may be down, or try again", d.host)
}

// notFound reports a name that doesn't exist.
func notFound(err error) bool {
	var dns *net.DNSError
	return errors.As(err, &dns) && dns.IsNotFound
}

// fallbackDelay is how long one address gets before the next is tried
// alongside it, as Chrome itself would, so a dead IPv6 route doesn't stall
// every page.
const fallbackDelay = 300 * time.Millisecond

// dialFirst connects to the first of addrs that answers, starting them a
// little apart.
func (g *guard) dialFirst(ctx context.Context, network string, addrs []string) (net.Conn, error) {
	if len(addrs) == 1 {
		return g.dialAddr(ctx, network, addrs[0])
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		c   net.Conn
		err error
	}
	results := make(chan result, len(addrs))
	for i, a := range addrs {
		go func(wait time.Duration, a string) {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				results <- result{err: ctx.Err()}
				return
			}
			c, err := g.dialAddr(ctx, network, a)
			results <- result{c, err}
		}(time.Duration(i)*fallbackDelay, a)
	}
	var firstErr error
	for i := range addrs {
		r := <-results
		if r.err == nil {
			go func(left int) { // close any later winners
				for ; left > 0; left-- {
					if r := <-results; r.c != nil {
						r.c.Close()
					}
				}
			}(len(addrs) - i - 1)
			return r.c, nil
		}
		if firstErr == nil || errors.Is(firstErr, context.Canceled) {
			firstErr = r.err
		}
	}
	return nil, firstErr
}

// note remembers a refusal, so a tool can say why a page didn't load.
func (g *guard) note(r *refusedError) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocks = append(g.blocks, block{at: time.Now(), ref: r})
	if len(g.blocks) > maxBlocks {
		g.blocks = append([]block(nil), g.blocks[len(g.blocks)-maxBlocks:]...)
	}
}

// noteFail remembers a connection that didn't work.
func (g *guard) noteFail(d *dialError) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fails = append(g.fails, fail{at: time.Now(), err: d})
	if len(g.fails) > maxBlocks {
		g.fails = append([]fail(nil), g.fails[len(g.fails)-maxBlocks:]...)
	}
}

// failedSince lists the connections that didn't work since t, newest first.
func (g *guard) failedSince(t time.Time) []*dialError {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*dialError
	for i := len(g.fails) - 1; i >= 0; i-- {
		if !g.fails[i].at.Before(t) {
			out = append(out, g.fails[i].err)
		}
	}
	return out
}

// blockedSince lists the hosts refused since t, oldest first, once each.
func (g *guard) blockedSince(t time.Time) []*refusedError {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*refusedError
	seen := map[string]bool{}
	for _, b := range g.blocks {
		if b.at.Before(t) || seen[b.ref.host] {
			continue
		}
		seen[b.ref.host] = true
		out = append(out, b.ref)
	}
	return out
}

type proxyKey struct{}

// guardTransport sends a plain-HTTP request directly, checked as it dials,
// or through the owner's proxy, checked before it goes: a proxy dials on our
// behalf, out of reach of the dial check.
type guardTransport struct {
	g               *guard
	direct, proxied *http.Transport
}

func (t *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := t.g.proxyFor(req.URL)
	if err == nil {
		err = usableProxy(u)
	}
	if err != nil {
		return nil, err
	}
	if u == nil {
		return t.direct.RoundTrip(req)
	}
	if err := t.g.check(req.Context(), req.URL.Hostname(), true); err != nil {
		var ref *refusedError
		if errors.As(err, &ref) {
			t.g.note(ref)
		}
		return nil, err
	}
	return t.proxied.RoundTrip(req.WithContext(context.WithValue(req.Context(), proxyKey{}, u)))
}

// guardPage is what a plain-HTTP page the guard refused, or couldn't reach,
// shows: to the model, and to the owner in a visible window. The meta tag
// lets a tool tell it from the site's own page.
const guardPage = `<!doctype html><html><head><meta charset="utf-8"><meta name="mirrin-guard" content="%s"><title>%s</title></head>
<body style="font-family:system-ui,sans-serif;max-width:36em;margin:4em auto;line-height:1.5">
<h1>%s</h1><p>%s</p></body></html>`

func (g *guard) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return // Chrome gave up on it
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var ref *refusedError
	if errors.As(err, &ref) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, guardPage, "blocked", "Blocked by Mirrin", "Blocked by Mirrin", html.EscapeString(ref.Error()))
		return
	}
	var de *dialError
	msg := fmt.Sprintf("the browser couldn't connect to %s. The site may be down, or try again", r.URL.Hostname())
	if errors.As(err, &de) {
		msg = de.Error()
	} else if strings.HasPrefix(err.Error(), "your proxy") {
		msg = err.Error()
	}
	w.WriteHeader(http.StatusBadGateway)
	title := "Couldn't open " + r.URL.Hostname()
	_, _ = fmt.Fprintf(w, guardPage, "failed", html.EscapeString(title), html.EscapeString(title), html.EscapeString(upperFirst(msg)+"."))
}

// upperFirst starts a sentence with a capital.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// tunnel serves CONNECT, which Chrome uses for https and WebSockets.
func (g *guard) tunnel(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "bad CONNECT target", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	up, err := g.dialVia(ctx, host, port)
	cancel()
	if err != nil {
		status := http.StatusBadGateway
		var ref *refusedError
		if errors.As(err, &ref) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "can't tunnel", http.StatusInternalServerError)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		up.Close()
		return
	}
	var from io.Reader = client
	if n := rw.Reader.Buffered(); n > 0 { // anything Chrome sent before our answer
		from = io.MultiReader(rw.Reader, client)
	}
	go splice(client, from, up)
}

// splice copies both ways until either side closes, then closes both.
func splice(client net.Conn, from io.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, from); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, up); done <- struct{}{} }()
	<-done
	client.Close()
	up.Close()
	<-done
}

// dialVia opens a raw connection to host:port for a tunnel: directly, checked
// as it dials, or through the owner's proxy after the target is checked.
func (g *guard) dialVia(ctx context.Context, host, port string) (net.Conn, error) {
	target := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}
	if port == "80" {
		target.Scheme = "http" // a ws:// WebSocket
	}
	pu, err := g.proxyFor(target)
	if err == nil {
		err = usableProxy(pu)
	}
	if err != nil {
		return nil, err
	}
	if pu == nil {
		return g.dial(ctx, "tcp", target.Host)
	}
	if err := g.check(ctx, host, true); err != nil {
		var ref *refusedError
		if errors.As(err, &ref) {
			g.note(ref)
		}
		return nil, err
	}
	return g.connectVia(ctx, pu, target.Host)
}

// usableProxy refuses, in words, a proxy address the guard can't speak to.
func usableProxy(pu *url.URL) error {
	if pu == nil {
		return nil
	}
	switch pu.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	}
	return fmt.Errorf("your proxy setting (%s) is a %s:// proxy, which my browser can't use. Set HTTPS_PROXY to an http://, https:// or socks5:// proxy instead", pu.Redacted(), pu.Scheme)
}

// connectVia asks the owner's proxy for a tunnel to hostport: an HTTP proxy
// (over TLS for an https:// one) with CONNECT, or a SOCKS5 proxy.
func (g *guard) connectVia(ctx context.Context, pu *url.URL, hostport string) (net.Conn, error) {
	if pu.Scheme == "socks5" || pu.Scheme == "socks5h" {
		return g.socksVia(ctx, pu, hostport)
	}
	paddr := pu.Host
	if pu.Port() == "" {
		paddr = net.JoinHostPort(pu.Hostname(), map[bool]string{true: "443", false: "80"}[pu.Scheme == "https"])
	}
	c, err := g.dialer.DialContext(ctx, "tcp", paddr)
	if err != nil {
		return nil, fmt.Errorf("your proxy %s didn't answer: %w", pu.Host, err)
	}
	if d, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(d)
	}
	if pu.Scheme == "https" {
		conf := &tls.Config{}
		if g.proxyTLS != nil {
			conf = g.proxyTLS.Clone()
		}
		conf.ServerName = pu.Hostname()
		tc := tls.Client(c, conf)
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, fmt.Errorf("your proxy %s didn't answer securely: %w", pu.Host, err)
		}
		c = tc
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: hostport}, Host: hostport, Header: http.Header{}}
	if pu.User != nil {
		pw, _ := pu.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(pu.User.Username()+":"+pw)))
	}
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, fmt.Errorf("your proxy %s wouldn't connect to %s: %s", pu.Host, hostport, resp.Status)
	}
	_ = c.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufConn{Conn: c, r: br}, nil
	}
	return c, nil
}

// socksVia opens a tunnel to hostport through a SOCKS5 proxy. The target
// was checked already, so the proxy may look its name up itself.
func (g *guard) socksVia(ctx context.Context, pu *url.URL, hostport string) (net.Conn, error) {
	d, err := proxy.FromURL(pu, g.dialer)
	if err != nil {
		return nil, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("your proxy %s can't be used", pu.Host)
	}
	c, err := cd.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return nil, fmt.Errorf("your proxy %s wouldn't connect to %s: %w", pu.Host, hostport, err)
	}
	return c, nil
}

// bufConn is a connection with bytes already read into a buffer.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// ownProxy is the proxy the owner's settings name for a target: the usual
// environment variables, or on a Mac the proxy set in System Settings. Chrome
// would have used it; now the guard does, after its check.
func ownProxy() (func(*url.URL) (*url.URL, error), string) {
	sys, unsupported := systemProxy()
	return func(u *url.URL) (*url.URL, error) {
		p, err := http.ProxyFromEnvironment(&http.Request{URL: u})
		if p != nil || err != nil {
			return p, err
		}
		if sys != nil {
			return sys(u), nil
		}
		return nil, nil
	}, unsupported
}
