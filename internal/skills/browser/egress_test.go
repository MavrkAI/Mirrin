package browser

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/skills/web"
)

// counter counts requests per path.
type counter struct {
	mu   sync.Mutex
	hits map[string]int
}

func (c *counter) hit(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hits == nil {
		c.hits = map[string]int{}
	}
	c.hits[path]++
}

func (c *counter) n(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits[path]
}

// testGuard is a started guard with no proxy of the owner's own and a
// resolver that knows a few made-up names, so nothing leaves this machine.
func testGuard(t *testing.T, allow ...string) (*guard, *http.Client) {
	t.Helper()
	g := newGuard()
	g.upstream = func(*url.URL) (*url.URL, error) { return nil, nil }
	g.resolve = fakeResolve
	g.setAllow(web.NewAllowlist(allow))
	addr, err := g.start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.close)
	pu, _ := url.Parse(addr)
	tr := &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(tr.CloseIdleConnections)
	return g, &http.Client{Transport: tr}
}

func TestGuardKeepsTheBrowserOffTheLocalNetwork(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.hit(r.URL.Path)
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "http://127.0.0.1:"+r.URL.Port()+"/admin", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "admin panel")
	})
	plain := httptest.NewServer(h)
	defer plain.Close()
	secure := httptest.NewTLSServer(h)
	defer secure.Close()
	port := plain.URL[strings.LastIndex(plain.URL, ":")+1:]

	for _, tc := range []struct {
		name  string
		allow []string
		url   string
		ok    bool
	}{
		{"loopback address", nil, plain.URL + "/admin", false},
		{"name that resolves to loopback", nil, "http://localhost:" + port + "/admin", false},
		{"https to loopback (CONNECT)", nil, secure.URL + "/admin", false},
		{"allowed address", []string{"127.0.0.1"}, plain.URL + "/admin", true},
		{"allowed range, https", []string{"127.0.0.0/8"}, secure.URL + "/admin", true},
		{"allowed name", []string{"localhost"}, "http://localhost:" + port + "/admin", true},
		// The name is allowed, but the redirect goes to an address that isn't.
		{"redirect re-checked", []string{"localhost"}, "http://localhost:" + port + "/hop", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, client := testGuard(t, tc.allow...)
			before := hits.n("/admin")
			resp, err := client.Get(tc.url)
			body := ""
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				body = string(b)
			}
			if tc.ok {
				if err != nil || body != "admin panel" {
					t.Fatalf("want the page, got %q, %v", body, err)
				}
				return
			}
			if hits.n("/admin") != before {
				t.Fatal("the private page was reached")
			}
			refs := g.blockedSince(testStart)
			if len(refs) == 0 || !strings.Contains(refs[len(refs)-1].Error(), "allow_hosts") {
				t.Fatalf("no refusal recorded: %v", refs)
			}
			if err == nil && (resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Blocked by Mirrin") || !strings.Contains(body, "allow_hosts")) {
				t.Fatalf("plain http should get the blocked page, got %d %q", resp.StatusCode, body)
			}
		})
	}
}

func TestGuardChecksWhereANamePoints(t *testing.T) {
	g, client := testGuard(t)
	ctx := context.Background()
	// The metadata service by name: refused without a single packet sent.
	if _, err := client.Get("http://metadata.test/latest/meta-data/"); err != nil {
		t.Fatal(err)
	}
	if refs := g.blockedSince(testStart); len(refs) != 1 || refs[0].host != "metadata.test" || refs[0].ip.String() != "169.254.169.254" {
		t.Fatalf("metadata by name: %v", refs)
	}
	var ref *refusedError
	if err := g.check(ctx, "metadata.test", false); !errors.As(err, &ref) {
		t.Fatalf("pre-check let the metadata service through: %v", err)
	}
	if err := g.check(ctx, "169.254.169.254", false); !errors.As(err, &ref) || !strings.Contains(err.Error(), "that address is on this computer or a private network") {
		t.Fatalf("metadata address: %v", err)
	}
	// A name with a public address may be opened (only that address is
	// dialed), unless the owner's proxy will dial it for us.
	if err := g.check(ctx, "mixed.test", false); err != nil {
		t.Fatalf("mixed name refused: %v", err)
	}
	if err := g.check(ctx, "mixed.test", true); !errors.As(err, &ref) {
		t.Fatalf("mixed name through a proxy: %v", err)
	}
	// Something that isn't a proxy request gets a plain answer.
	resp, err := http.Get(g.addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("direct request: %d", resp.StatusCode)
	}
}

// Behind the owner's own proxy the target is checked before the request goes,
// since the proxy dials for us; the proxy itself may be on the local network.
func TestGuardGoesThroughTheOwnersProxy(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.RequestURI)
		mu.Unlock()
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, "via proxy")
	}))
	defer upstream.Close()
	pu, _ := url.Parse(upstream.URL)
	g, client := testGuard(t, "10.0.0.0/8")
	g.upstream = func(*url.URL) (*url.URL, error) { return pu, nil }

	resp, err := client.Get("http://93.184.215.14/page")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "via proxy" {
		t.Fatalf("public page through the proxy: %q", b)
	}
	for _, private := range []string{"http://169.254.169.254/latest/", "http://192.168.1.1/admin", "https://192.168.1.1/admin"} {
		if resp, err := client.Get(private); err == nil {
			resp.Body.Close()
		}
	}
	// An allowed range goes through too.
	if resp, err := client.Get("http://10.1.2.3/"); err == nil {
		resp.Body.Close()
	}
	// A tunnel is asked of the proxy with the target it was checked for.
	c, err := net.Dial("tcp", strings.TrimPrefix(g.addr, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT 93.184.215.14:443 HTTP/1.1\r\nHost: 93.184.215.14:443\r\n\r\n")
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if !strings.Contains(line, "200") {
		t.Fatalf("tunnel through the proxy: %q", line)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"GET http://93.184.215.14/page", "GET http://10.1.2.3/", "CONNECT 93.184.215.14:443"}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the proxy saw:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
}

// An address that doesn't answer (a dead IPv6 route, say) doesn't stall the
// page: the next is tried alongside it.
func TestGuardDialsTheFirstAddressThatAnswers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := closed.Addr().String()
	closed.Close()
	g := newGuard()
	// The first address never answers; nothing is sent to it for real.
	const silent = "192.0.2.1:9"
	real := g.dialAddr
	g.dialAddr = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == silent {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return real(ctx, network, addr)
	}
	start := time.Now()
	c, err := g.dialFirst(context.Background(), "tcp", []string{silent, dead, ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %s", took)
	}
	if _, err := g.dialFirst(context.Background(), "tcp", []string{dead}); err == nil {
		t.Fatal("a closed port answered")
	}
}

// A plain-HTTP query reaches the site exactly as Chrome sent it, parts Go
// can't parse included.
func TestGuardKeepsTheQueryAsSent(t *testing.T) {
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.URL.RawQuery
		mu.Unlock()
	}))
	defer srv.Close()
	_, client := testGuard(t, "127.0.0.1")
	resp, err := client.Get(srv.URL + "/p?a=1;b=2&c=3")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if got != "a=1;b=2&c=3" {
		t.Fatalf("the site saw %q", got)
	}
}

// The guard answers the start-up check itself, by plain http or a tunnel,
// and nothing else hears of it.
func TestGuardAnswersTheCanary(t *testing.T) {
	g, client := testGuard(t)
	host, seen, done := g.expectCanary()
	defer done()
	resp, err := client.Get("http://" + host + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("canary: %d", resp.StatusCode)
	}
	select {
	case <-seen:
	default:
		t.Fatal("the guard didn't notice the canary")
	}
	host2, seen2, done2 := g.expectCanary()
	defer done2()
	c, err := net.Dial("tcp", strings.TrimPrefix(g.addr, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", host2, host2)
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if strings.Contains(line, "200") {
		t.Fatalf("a tunnel was opened for the canary: %q", line)
	}
	select {
	case <-seen2:
	default:
		t.Fatal("the guard didn't notice the canary tunnel")
	}
	if refs := g.blockedSince(testStart); len(refs) != 0 {
		t.Fatalf("the canary counted as a refusal: %v", refs)
	}
}

// A site that can't be reached says why in words: on the page the browser
// shows for plain http, and in what the tools are told.
func TestGuardSaysWhyAConnectionFailed(t *testing.T) {
	g, client := testGuard(t, "127.0.0.1")
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := closed.Addr().String()
	closed.Close()
	for u, want := range map[string]string{
		"http://nosuch.test/":  "I couldn't find nosuch.test. Check the address",
		"http://" + dead + "/": "127.0.0.1 refused the connection. The site may be down",
	} {
		resp, err := client.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body := html.UnescapeString(string(b))
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, want) || !strings.Contains(body, `name="mirrin-guard" content="failed"`) || strings.Contains(body, "dial tcp") {
			t.Errorf("%s: %d %q", u, resp.StatusCode, body)
		}
	}
	fails := g.failedSince(testStart)
	hosts := map[string]bool{}
	for _, f := range fails {
		hosts[f.host] = true
	}
	if len(fails) != 2 || !hosts["127.0.0.1"] || !hosts["nosuch.test"] {
		t.Fatalf("failures noted: %v", fails)
	}
	var ne net.Error = timeoutErr{}
	for err, want := range map[error]string{
		&net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}: "I couldn't find example.org",
		context.DeadlineExceeded:             "didn't answer in time",
		&net.OpError{Op: "dial", Err: ne}:    "didn't answer in time",
		errors.New("network is unreachable"): "couldn't connect to example.org",
	} {
		if got := (&dialError{host: "example.org", err: err}).Error(); !strings.Contains(got, want) {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// The owner's proxy may be an https:// one (spoken to over TLS) or SOCKS5;
// one the guard can't speak to is refused in words.
func TestGuardSpeaksToEveryKindOfProxy(t *testing.T) {
	// An https:// proxy: TLS first, then CONNECT.
	var mu sync.Mutex
	var seen []string
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Host)
		mu.Unlock()
		if r.Method != http.MethodConnect {
			_, _ = io.WriteString(w, "via secure proxy")
			return
		}
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\n\r\nhello over tls")
		_ = rw.Flush()
		c.Close()
	}))
	defer secure.Close()
	socks, socksSaw := socksServer(t)

	tunnel := func(g *guard) string {
		t.Helper()
		c, err := net.Dial("tcp", strings.TrimPrefix(g.addr, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(c, "CONNECT 93.184.215.14:443 HTTP/1.1\r\nHost: 93.184.215.14:443\r\n\r\n")
		b, _ := io.ReadAll(c)
		return string(b)
	}

	g, client := testGuard(t)
	g.proxyTLS = secure.Client().Transport.(*http.Transport).TLSClientConfig
	su, _ := url.Parse(secure.URL)
	g.upstream = func(*url.URL) (*url.URL, error) { return su, nil }
	if got := tunnel(g); !strings.Contains(got, "200 Connection Established") || !strings.Contains(got, "hello over tls") {
		t.Fatalf("tunnel through an https proxy: %q", got)
	}

	g.upstream = func(*url.URL) (*url.URL, error) { return &url.URL{Scheme: "socks5", Host: socks}, nil }
	if got := tunnel(g); !strings.Contains(got, "200 Connection Established") || !strings.Contains(got, "hello over socks") {
		t.Fatalf("tunnel through a SOCKS5 proxy: %q", got)
	}
	if got := socksSaw(); got != "93.184.215.14:443" {
		t.Fatalf("the SOCKS proxy was asked for %q", got)
	}

	g.upstream = func(*url.URL) (*url.URL, error) { return &url.URL{Scheme: "socks4", Host: socks}, nil }
	if got := tunnel(g); strings.Contains(got, "200") {
		t.Fatalf("a socks4 proxy was used: %q", got)
	}
	resp, err := client.Get("http://93.184.215.14/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "socks4:// proxy, which my browser can&#39;t use") {
		t.Fatalf("unusable proxy: %q", b)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(seen, ",") != "CONNECT 93.184.215.14:443" {
		t.Fatalf("the https proxy saw %v", seen)
	}
}

// socksServer is a minimal SOCKS5 proxy (no password) on this machine that
// answers every tunnel with "hello over socks" and reports what it was asked
// for.
func socksServer(t *testing.T) (addr string, asked func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var target string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 262)
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				if _, err := io.ReadFull(c, buf[:buf[1]]); err != nil {
					return
				}
				_, _ = c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, buf[:4]); err != nil {
					return
				}
				var host string
				switch buf[3] {
				case 1:
					_, _ = io.ReadFull(c, buf[:4])
					host = net.IP(buf[:4]).String()
				case 3:
					_, _ = io.ReadFull(c, buf[:1])
					n := int(buf[0])
					_, _ = io.ReadFull(c, buf[:n])
					host = string(buf[:n])
				default:
					return
				}
				_, _ = io.ReadFull(c, buf[:2])
				mu.Lock()
				target = net.JoinHostPort(host, fmt.Sprint(int(buf[0])<<8|int(buf[1])))
				mu.Unlock()
				_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				_, _ = c.Write([]byte("hello over socks"))
			}(c)
		}
	}()
	return ln.Addr().String(), func() string { mu.Lock(); defer mu.Unlock(); return target }
}

func TestParseMacProxySettings(t *testing.T) {
	p := parseScutil(`<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
    1 : 169.254/16
    2 : intranet.example.com
  }
  ExcludeSimpleHostnames : 1
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 3128
  HTTPProxy : proxy.example.com
  HTTPSEnable : 1
  HTTPSPort : 3129
  HTTPSProxy : secure-proxy.example.com
}`)
	for raw, want := range map[string]string{
		"https://example.org/":         "http://secure-proxy.example.com:3129",
		"http://example.org/":          "http://proxy.example.com:3128",
		"http://printer.local/":        "",
		"http://intranet.example.com/": "",
		"http://nas/":                  "", // a simple host name
	} {
		u, _ := url.Parse(raw)
		got := ""
		if p := p.forURL(u); p != nil {
			got = p.String()
		}
		if got != want {
			t.Errorf("%s: proxy %q, want %q", raw, got, want)
		}
	}
	if p := parseScutil("<dictionary> {\n  ProxyAutoConfigEnable : 1\n  ProxyAutoConfigURLString : http://wpad/wpad.dat\n}"); p == nil || p.unsupported != pacSetting || p.http != "" || p.https != "" {
		t.Fatalf("PAC only: %+v", p)
	}
	if p := parseScutil("<dictionary> {\n  ProxyAutoDiscoveryEnable : 1\n}"); p == nil || p.unsupported != wpadSetting {
		t.Fatalf("WPAD: %+v", p)
	}
	if parseScutil("") != nil {
		t.Fatal("no settings should mean no proxy")
	}
	// Only a web proxy: https pages go direct, as on the Mac itself; SOCKS
	// takes what the others don't.
	only := parseScutil("<dictionary> {\n  HTTPEnable : 1\n  HTTPPort : 3128\n  HTTPProxy : proxy.corp\n}")
	socks := parseScutil("<dictionary> {\n  HTTPEnable : 1\n  HTTPPort : 3128\n  HTTPProxy : proxy.corp\n  SOCKSEnable : 1\n  SOCKSPort : 1080\n  SOCKSProxy : socks.corp\n}")
	for _, c := range []struct {
		p         *sysProxy
		raw, want string
	}{
		{only, "https://example.com/", ""},
		{only, "http://example.com/", "http://proxy.corp:3128"},
		{socks, "https://example.com/", "socks5://socks.corp:1080"},
		{socks, "http://example.com/", "http://proxy.corp:3128"},
	} {
		u, _ := url.Parse(c.raw)
		got := ""
		if p := c.p.forURL(u); p != nil {
			got = p.String()
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestParseWindowsProxy(t *testing.T) {
	pick := func(p *sysProxy, raw string) string {
		if p == nil {
			return "<none>"
		}
		u, _ := url.Parse(raw)
		if pu := p.forURL(u); pu != nil {
			return pu.String()
		}
		return ""
	}
	one := parseWindowsProxy(true, "proxy.corp:8080", "<local>;*.corp.example;10.*", "")
	for raw, want := range map[string]string{
		"https://example.com/":         "http://proxy.corp:8080",
		"http://example.com/":          "http://proxy.corp:8080",
		"http://intranet/":             "",
		"https://wiki.corp.example/":   "",
		"http://10.1.2.3/":             "",
		"https://corp.example.evil.io": "http://proxy.corp:8080",
	} {
		if got := pick(one, raw); got != want {
			t.Errorf("one proxy, %s: %q, want %q", raw, got, want)
		}
	}
	per := parseWindowsProxy(true, "http=web.corp:80;https=secure.corp:443", "", "")
	if got := pick(per, "https://example.com/"); got != "http://secure.corp:443" {
		t.Errorf("per scheme: %q", got)
	}
	if got := pick(per, "http://example.com/"); got != "http://web.corp:80" {
		t.Errorf("per scheme: %q", got)
	}
	if p := parseWindowsProxy(false, "proxy.corp:8080", "", ""); p != nil {
		t.Errorf("turned off: %+v", p)
	}
	if p := parseWindowsProxy(false, "", "", "http://wpad.corp/proxy.pac"); p == nil || p.unsupported != pacSetting {
		t.Errorf("PAC: %+v", p)
	}
	if p := parseWindowsProxy(true, "socks=socks.corp:1080", "", ""); p == nil || p.unsupported != socks4Setting {
		t.Errorf("SOCKS4: %+v", p)
	}
	if got := pick(parseWindowsProxy(true, "socks=socks5://socks.corp:1080", "", ""), "https://example.com/"); got != "socks5://socks.corp:1080" {
		t.Errorf("SOCKS5: %q", got)
	}
}

func TestParseLinuxDesktopProxy(t *testing.T) {
	gnome := parseGsettings(`org.gnome.system.proxy autoconfig-url ''
org.gnome.system.proxy ignore-hosts ['localhost', '127.0.0.0/8', '*.corp.example']
org.gnome.system.proxy mode 'manual'
org.gnome.system.proxy use-same-proxy false
org.gnome.system.proxy.http host 'proxy.corp'
org.gnome.system.proxy.http port 3128
org.gnome.system.proxy.https host ''
org.gnome.system.proxy.https port 0
org.gnome.system.proxy.socks host ''
org.gnome.system.proxy.socks port 0
`)
	if gnome == nil || gnome.http != "proxy.corp:3128" || gnome.https != "" || !gnome.bypass("wiki.corp.example") || !gnome.bypass("127.0.0.9") || gnome.bypass("example.com") {
		t.Fatalf("GNOME manual: %+v", gnome)
	}
	if p := parseGsettings("org.gnome.system.proxy mode 'auto'\norg.gnome.system.proxy autoconfig-url 'http://wpad/wpad.dat'\n"); p == nil || p.unsupported != pacSetting {
		t.Fatalf("GNOME PAC: %+v", p)
	}
	if p := parseGsettings("org.gnome.system.proxy mode 'auto'\norg.gnome.system.proxy autoconfig-url ''\n"); p == nil || p.unsupported != wpadSetting {
		t.Fatalf("GNOME WPAD: %+v", p)
	}
	if parseGsettings("org.gnome.system.proxy mode 'none'\n") != nil || parseGsettings("") != nil {
		t.Fatal("GNOME with no proxy")
	}
	kde := parseKioslaverc("[General]\nx=1\n[Proxy Settings]\nNoProxyFor=localhost,.corp.example\nProxyType=1\nhttpProxy=http://proxy.corp 3128\nhttpsProxy=http://proxy.corp:3129\n")
	if kde == nil || kde.http != "proxy.corp:3128" || kde.https != "proxy.corp:3129" || !kde.bypass("wiki.corp.example") {
		t.Fatalf("KDE manual: %+v", kde)
	}
	if p := parseKioslaverc("[Proxy Settings]\nProxyType=2\n"); p == nil || p.unsupported != pacSetting {
		t.Fatalf("KDE PAC: %+v", p)
	}
	if parseKioslaverc("[Proxy Settings]\nProxyType=0\n") != nil {
		t.Fatal("KDE with no proxy")
	}
}

// fakeResolve knows a few made-up names and localhost; nothing else exists,
// so no test looks a name up for real.
func fakeResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	switch host {
	case "metadata.test":
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	case "mixed.test":
		return []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("93.184.215.14")}, nil
	case "localhost":
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// testStart is before anything: blockedSince(testStart) is every refusal of
// that (per-test) guard.
var testStart time.Time
