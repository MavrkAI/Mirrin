package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// egress keeps fetch_url on the public internet. Every connection, including
// each redirect, is checked against the address it actually dials, so a
// hostname that resolves (or re-resolves) to this machine, the home network
// or a cloud metadata service is refused unless the owner allowed it.
type egress struct {
	hosts  map[string]bool // host names the owner allowed, lower case
	nets   []netip.Prefix  // addresses and ranges the owner allowed
	dialer *net.Dialer
	proxy  func(*http.Request) (*url.URL, error) // the proxy settings (HTTPS_PROXY and the like)
}

// newEgress parses allow: host names ("homeassistant.local"), addresses
// ("192.168.1.20") or ranges ("10.0.0.0/8").
func newEgress(allow []string) *egress {
	e := &egress{hosts: map[string]bool{}, dialer: &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}, proxy: http.ProxyFromEnvironment}
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if p, err := netip.ParsePrefix(a); err == nil {
			e.nets = append(e.nets, p.Masked())
		} else if ip, err := netip.ParseAddr(a); err == nil {
			e.nets = append(e.nets, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
		} else {
			e.hosts[a] = true
		}
	}
	return e
}

// client is an HTTP client whose every request, redirects included, goes
// through the egress check.
func (e *egress) client() *http.Client {
	direct := baseTransport()
	direct.Proxy = nil
	direct.DialContext = e.dial
	proxied := baseTransport()
	proxied.Proxy = func(r *http.Request) (*url.URL, error) {
		if u, ok := r.Context().Value(proxyKey{}).(*url.URL); ok {
			return u, nil
		}
		return nil, errors.New("no proxy chosen for this request") // never dial the target unchecked
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &egressTransport{e: e, direct: direct, proxied: proxied}}
}

// baseTransport is a copy of Go's default transport. When something has
// replaced http.DefaultTransport with its own RoundTripper (a test, or
// instrumentation), it starts from the same settings instead of panicking.
func baseTransport() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

type proxyKey struct{}

// egressTransport sends a request directly, checked as it dials, or through
// the proxy the settings name, checked before it goes: a proxy dials on our
// behalf, out of reach of the dial check.
type egressTransport struct {
	e               *egress
	direct, proxied *http.Transport
}

func (t *egressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := t.e.proxy(req)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return t.direct.RoundTrip(req)
	}
	if err := t.e.checkTarget(req.Context(), req.URL.Hostname()); err != nil {
		return nil, err
	}
	return t.proxied.RoundTrip(req.WithContext(context.WithValue(req.Context(), proxyKey{}, u)))
}

// checkTarget is the dial check for a request a proxy will make: every
// address the name has must be public or allowed. The proxy looks the name
// up again for itself, so this keeps out the plain cases (the router, the
// metadata service, localhost) rather than a name that changes in between.
func (e *egress) checkTarget(ctx context.Context, host string) error {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if e.hosts[host] {
		return nil
	}
	ips := []netip.Addr{}
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = append(ips, ip)
	} else if ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host); err != nil {
		return &refusedError{host: host, err: err}
	}
	for _, ip := range ips {
		if ip = ip.Unmap(); privateAddr(ip) && !e.allowed(ip) {
			return &refusedError{host: host, ip: ip}
		}
	}
	return nil
}

// refusedError is fetch_url declining to reach a private address, or one
// it couldn't check before handing it to a proxy.
type refusedError struct {
	host string
	ip   netip.Addr
	err  error // why where host points couldn't be checked
}

func (r *refusedError) Error() string {
	if r.err != nil {
		return fmt.Sprintf("I won't fetch %s through the proxy: I couldn't check where it points (%v). If you trust it, add it to skills.web.allow_hosts in the config.", r.host, r.err)
	}
	return fmt.Sprintf("I won't fetch %s: it points to %s, which is on this computer or a private network. If you trust it, add it to skills.web.allow_hosts in the config.", r.host, r.ip)
}

func (e *egress) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if e.hosts[strings.ToLower(strings.TrimSuffix(host, "."))] {
		return e.dialer.DialContext(ctx, network, addr)
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	var refused *refusedError
	var lastErr error
	for _, ip := range ips {
		ip = ip.Unmap()
		if privateAddr(ip) && !e.allowed(ip) {
			refused = &refusedError{host: host, ip: ip}
			continue
		}
		// Dial the checked address itself, so nothing can re-resolve the name.
		c, err := e.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	if refused != nil {
		return nil, refused
	}
	return nil, fmt.Errorf("no address found for %s", host)
}

func (e *egress) allowed(ip netip.Addr) bool {
	for _, p := range e.nets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Ranges that aren't the public internet, beyond what netip already knows.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, Tailscale
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, broadcast
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("fec0::/10"), // old site-local
}

// nat64 carries an IPv4 address in its last four bytes.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// privateAddr reports whether ip is this machine, the local network,
// link-local (where cloud metadata services live) or otherwise not a public
// internet address.
func privateAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if nat64.Contains(ip) {
		b := ip.As16()
		return privateAddr(netip.AddrFrom4([4]byte(b[12:16])))
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
