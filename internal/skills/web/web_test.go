package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

func TestPrivateAddresses(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1":          true,
		"10.1.2.3":           true,
		"172.16.0.1":         true,
		"192.168.1.1":        true,
		"169.254.169.254":    true, // cloud metadata
		"100.100.1.2":        true, // Tailscale
		"0.0.0.0":            true,
		"::1":                true,
		"fe80::1":            true,
		"fd00:ec2::254":      true, // AWS metadata over IPv6
		"::ffff:127.0.0.1":   true,
		"64:ff9b::a9fe:a9fe": true, // metadata through NAT64
		"8.8.8.8":            false,
		"1.1.1.1":            false,
		"2606:4700::1111":    false,
	} {
		if got := privateAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("privateAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func fetch(t *testing.T, allow []string, u string) (string, error) {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"url": u})
	return Tools(allow...)[0].Run(context.Background(), tools.Call{Input: in})
}

func TestFetchURLStaysOffTheLocalNetwork(t *testing.T) {
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "http://127.0.0.1:"+r.Host[strings.LastIndex(r.Host, ":")+1:]+"/admin", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("admin panel"))
	}))
	defer secret.Close()
	port := secret.URL[strings.LastIndex(secret.URL, ":")+1:]

	for _, tc := range []struct {
		name  string
		allow []string
		url   string
		ok    bool
	}{
		{"loopback address", nil, secret.URL + "/admin", false},
		{"name that resolves to loopback", nil, "http://localhost:" + port + "/admin", false},
		{"metadata service", nil, "http://169.254.169.254/latest/meta-data/", false},
		{"allowed address", []string{"127.0.0.1"}, secret.URL + "/admin", true},
		{"allowed range", []string{"127.0.0.0/8"}, secret.URL + "/admin", true},
		{"allowed name", []string{"localhost"}, "http://localhost:" + port + "/admin", true},
		// The name is allowed, but the redirect goes to an address that isn't.
		{"redirect re-checked", []string{"localhost"}, "http://localhost:" + port + "/hop", false},
	} {
		out, err := fetch(t, tc.allow, tc.url)
		if tc.ok && (err != nil || !strings.Contains(out, "admin panel")) {
			t.Errorf("%s: want the page, got %q, %v", tc.name, out, err)
		}
		if !tc.ok {
			if err == nil || strings.Contains(out, "admin panel") {
				t.Errorf("%s: should be refused, got %q", tc.name, out)
			} else if !strings.Contains(err.Error(), "allow_hosts") {
				t.Errorf("%s: refusal should say how to allow it: %v", tc.name, err)
			}
		}
	}
}

// Behind a proxy the target is checked before the request goes, since the
// proxy dials for us; the proxy itself may be on the local network.
func TestFetchThroughAProxyIsCheckedToo(t *testing.T) {
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.String()) // a proxy gets the full URL
		_, _ = w.Write([]byte("via proxy"))
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	for _, tc := range []struct {
		name  string
		allow []string
		url   string
		ok    bool
	}{
		{"public address", nil, "http://93.184.215.14/page", true},
		{"metadata service", nil, "http://169.254.169.254/latest/meta-data/", false},
		{"home network", nil, "http://192.168.1.1/admin", false},
		{"allowed range", []string{"10.0.0.0/8"}, "http://10.1.2.3/", true},
		{"a name that can't be checked", nil, "http://no-such-host.invalid/", false},
		{"allowed name", []string{"no-such-host.invalid"}, "http://no-such-host.invalid/", true},
	} {
		seen = nil
		e := newEgress(tc.allow)
		e.proxy = http.ProxyURL(pu)
		resp, err := e.client().Get(tc.url)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
				continue
			}
			resp.Body.Close()
			if len(seen) != 1 || seen[0] != tc.url {
				t.Errorf("%s: proxy saw %q", tc.name, seen)
			}
			continue
		}
		if err == nil {
			resp.Body.Close()
		}
		var refused *refusedError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "allow_hosts") || len(seen) != 0 {
			t.Errorf("%s: should be refused before reaching the proxy: %v (proxy saw %q)", tc.name, err, seen)
		}
	}
}

type wrappedTransport struct{ http.RoundTripper }

// Replacing http.DefaultTransport with a wrapper used to panic when the
// fetch tool was built, taking the daemon down at start.
func TestFetchToolBuildsWhenDefaultTransportIsWrapped(t *testing.T) {
	saved := http.DefaultTransport
	http.DefaultTransport = wrappedTransport{saved}
	t.Cleanup(func() { http.DefaultTransport = saved })
	c := newEgress(nil).client()
	et, ok := c.Transport.(*egressTransport)
	if !ok || et.direct.DialContext == nil || et.direct.Proxy != nil {
		t.Fatalf("the checked transport wasn't set up: %#v", c.Transport)
	}
}
