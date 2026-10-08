package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

func (e *env) offer(kind string) OfferResponse {
	e.t.Helper()
	w := e.do(onLoopback, req{method: "POST", path: "/devices/offers", body: `{"kind":"` + kind + `"}`, header: bearer(master)})
	if w.Code != 200 {
		e.t.Fatalf("offer: %d %s", w.Code, w.Body)
	}
	var o OfferResponse
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		e.t.Fatal(err)
	}
	return o
}

func claimBody(o devices.Offer, secret, name, kind string) string {
	b, _ := json.Marshal(ClaimRequest{Offer: o.ID, Secret: secret, Name: name, Kind: kind})
	return string(b)
}

func TestPhonePairsWithALink(t *testing.T) {
	e := newEnv(t)
	e.s.addr, e.s.remote = "100.64.0.1:7742", true
	o := e.offer("pwa")
	if len(o.Links) != 1 || o.Code != "" || o.Name != "Mirrin" {
		t.Fatalf("offer %+v", o)
	}
	link := o.Links[0]
	if !strings.HasPrefix(link, "http://100.64.0.1:7742/pair#v=2&o="+o.Offer.ID+"&s=") || strings.Contains(strings.SplitN(link, "#", 2)[0], o.Offer.Secret) {
		t.Fatalf("link %s: the secret must be in the fragment", link)
	}
	// The pair page itself is public and static.
	if w := e.do(onLegacy, req{path: "/pair"}); w.Code != 200 || !strings.Contains(w.Body.String(), "/pair/claim") {
		t.Fatalf("pair page: %d", w.Code)
	}
	w := e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(o.Offer, o.Offer.Secret, "Akshay's iPhone", "pwa"),
		header: map[string]string{"Origin": "http://100.64.0.1:7742", "Content-Type": "application/json"}})
	if w.Code != 200 {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	var resp ClaimResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Token != "" || !strings.HasPrefix(resp.Ticket, "it_") || resp.Device.Name != "Akshay's iPhone" || resp.Device.Has(devices.Admin) {
		t.Fatalf("claim response %s", w.Body)
	}
	c := cookieFrom(w, cookieDev)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie %+v", c)
	}
	// The phone now sees the screen, and approves.
	if w := e.do(onLegacy, req{path: "/ui", cookies: []*http.Cookie{c}}); w.Code != 200 {
		t.Fatalf("screen: %d", w.Code)
	}
	if w := e.do(onLegacy, req{method: "POST", path: "/approvals/4/approve", cookies: []*http.Cookie{c}, header: map[string]string{"Origin": "http://100.64.0.1:7742"}}); w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	ev := e.pairedEvents(1)
	if len(ev) != 1 || ev[0].How != HowClaimed || ev[0].Device.ID != resp.Device.ID || ev[0].IP != "100.64.0.9" || ev[0].Via != "tailscale" {
		t.Fatalf("announcement %+v", ev)
	}
	// The same link again is refused, plainly.
	w = e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(o.Offer, o.Offer.Secret, "x", "pwa")})
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), `"offer_used"`) {
		t.Fatalf("second claim: %d %s", w.Code, w.Body)
	}
	// A Home Screen app collects the cookie once with its ticket.
	w = e.do(onLegacy, req{method: "POST", path: "/pair/ticket", body: `{"t":"` + resp.Ticket + `"}`})
	if tc := cookieFrom(w, cookieDev); w.Code != 200 || tc == nil || tc.Value != c.Value {
		t.Fatalf("ticket: %d %v", w.Code, tc)
	}
	if w := e.do(onLegacy, req{method: "POST", path: "/pair/ticket", body: `{"t":"` + resp.Ticket + `"}`}); w.Code != http.StatusGone {
		t.Fatalf("ticket twice: %d", w.Code)
	}
}

func TestClaimOverTLSSetsAHostCookie(t *testing.T) {
	e := newEnv(t)
	o := e.offer("pwa")
	w := e.do(onRemote, req{method: "POST", path: "/pair/claim", body: claimBody(o.Offer, o.Offer.Secret, "Phone", ""),
		header: map[string]string{"Origin": "https://twin.example.ts.net", "Sec-Fetch-Site": "same-origin"}})
	if w.Code != 200 {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	c := cookieFrom(w, cookieSecure)
	if c == nil || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode || c.MaxAge != cookieMaxAge {
		t.Fatalf("__Host- cookie %+v", c)
	}
	if w := e.do(onRemote, req{path: "/screen", cookies: []*http.Cookie{c}}); w.Code != 200 {
		t.Fatalf("screen over TLS: %d", w.Code)
	}
}

func TestClaimRefusals(t *testing.T) {
	e := newEnv(t)
	o := e.offer("pwa")
	// Unknown offers look like any unknown route.
	w := e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: `{"o":"of_aaaaaaaaaa","s":"x"}`})
	unknown := e.do(onLegacy, req{method: "POST", path: "/no/such/route"})
	if w.Code != 404 || w.Body.String() != unknown.Body.String() {
		t.Fatalf("unknown offer: %d %q vs %q", w.Code, w.Body, unknown.Body)
	}
	// Five wrong secrets burn it; the right one after that is refused.
	for i := 0; i < 5; i++ {
		w = e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(o.Offer, "guess", "", ""), ip: "100.64.1." + string(rune('1'+i))})
		if w.Code != http.StatusForbidden {
			t.Fatalf("wrong secret %d: %d %s", i+1, w.Code, w.Body)
		}
	}
	w = e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(o.Offer, o.Offer.Secret, "", "")})
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "too many wrong tries") {
		t.Fatalf("burned offer: %d %s", w.Code, w.Body)
	}
	// Ten claims a minute per address; the eleventh waits.
	for i := 0; i < devices.ClaimsPerMinute; i++ {
		e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: `{"o":"of_aaaaaaaaaa","s":"x"}`, ip: "203.0.113.50"})
	}
	fresh := e.offer("pwa")
	w = e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(fresh.Offer, fresh.Offer.Secret, "", ""), ip: "203.0.113.50"})
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("11th claim: %d %v", w.Code, w.Header())
	}
	// No global lockout: the owner, elsewhere, pairs with that same offer.
	if w := e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(fresh.Offer, fresh.Offer.Secret, "", "")}); w.Code != 200 {
		t.Fatalf("owner's claim after someone else was limited: %d %s", w.Code, w.Body)
	}
	// A cross-site page can't claim on a visitor's behalf.
	other := e.offer("pwa")
	w = e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(other.Offer, other.Offer.Secret, "", ""), header: map[string]string{"Origin": "https://evil.example"}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site claim: %d", w.Code)
	}
}

func TestTerminalPairsWithACode(t *testing.T) {
	e := newEnv(t)
	e.s.addr, e.s.remote = "100.64.0.1:7742", true
	o := e.offer("cli")
	if o.Code == "" || len(o.Links) != 0 {
		t.Fatalf("offer %+v", o)
	}
	c, err := DecodeCode(o.Code)
	if err != nil || c.Offer != o.Offer.ID || c.Secret != o.Offer.Secret || len(c.URLs) != 1 || c.URLs[0] != "http://100.64.0.1:7742" || c.Name != "Mirrin" {
		t.Fatalf("code %+v %v", c, err)
	}
	if strings.Contains(o.Code, master) {
		t.Fatal("the code carries the master key")
	}
	w := e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(o.Offer, o.Offer.Secret, "Laptop", "cli")})
	var resp ClaimResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != 200 || !devices.LooksLikeToken(resp.Token) || cookieFrom(w, cookieDev) != nil {
		t.Fatalf("cli claim: %d %s", w.Code, w.Body)
	}
	if w := e.do(onLegacy, req{method: "POST", path: "/message", body: `{"text":"hi"}`, header: bearer(resp.Token)}); w.Code != 200 {
		t.Fatalf("terminal chat: %d", w.Code)
	}
	// A browser can't spend a terminal's code, and vice versa.
	cli := e.offer("cli")
	if w := e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(cli.Offer, cli.Offer.Secret, "", "pwa")}); w.Code != 400 {
		t.Fatalf("browser claiming a cli offer: %d", w.Code)
	}
}

func TestDevicesFileHoldsNoKeys(t *testing.T) {
	e := newEnv(t)
	o := e.offer("cli")
	w := e.do(onLegacy, req{method: "POST", path: "/pair/claim", body: claimBody(o.Offer, o.Offer.Secret, "Laptop", "cli")})
	var resp ClaimResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	page := cookieFrom(e.do(onLoopback, req{path: "/ui?token=" + master}), cookieDev)
	b, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{resp.Token, page.Value, o.Offer.Secret, master} {
		if secret == "" || strings.Contains(string(b), secret) {
			t.Fatalf("devices.json holds %q", secret)
		}
	}
	// Revoking is instant.
	if w := e.do(onLoopback, req{method: "POST", path: "/devices/" + resp.Device.ID + "/revoke", header: bearer(master)}); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := e.do(onLegacy, req{path: "/status", header: bearer(resp.Token)}); w.Code != 401 {
		t.Fatalf("revoked terminal: %d", w.Code)
	}
}

func TestOldStyleTerminalMovesOntoItsOwnKey(t *testing.T) {
	e := newEnv(t)
	// The master key over the old listener works, for what old codes gave,
	// with a notice saying what to do next.
	w := e.do(onLegacy, req{path: "/status", header: bearer(master)})
	if w.Code != 200 || w.Header().Get("Mirrin-Notice") == "" {
		t.Fatalf("old-style code: %d %v", w.Code, w.Header())
	}
	if w := e.do(onLegacy, req{path: "/devices", header: bearer(master)}); w.Code != 404 {
		t.Fatalf("old-style code reaching settings: %d", w.Code)
	}
	w = e.do(onLegacy, req{method: "POST", path: "/pair/upgrade", body: `{"name":"Akshay's laptop"}`, header: bearer(master)})
	var resp ClaimResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != 200 || !devices.LooksLikeToken(resp.Token) || resp.Device.Kind != devices.KindCLI || resp.Device.Name != "Akshay's laptop" {
		t.Fatalf("upgrade: %d %s", w.Code, w.Body)
	}
	if ev := e.pairedEvents(1); len(ev) != 1 || ev[0].How != HowLegacyCode {
		t.Fatalf("announcement %+v", ev)
	}
	if w := e.do(onLegacy, req{path: "/status", header: bearer(resp.Token)}); w.Code != 200 {
		t.Fatalf("new key: %d", w.Code)
	}
	// Not needed, and not done, on this computer.
	if w := e.do(onLoopback, req{method: "POST", path: "/pair/upgrade", header: bearer(master)}); w.Code != 400 {
		t.Fatalf("upgrade on loopback: %d", w.Code)
	}
}

func TestCodes(t *testing.T) {
	c := Code{URLs: []string{"https://twin.example.ts.net", "http://100.64.0.1:7742"}, Offer: "of_abcdefghij", Secret: "c2VjcmV0", Name: "Mirrin", Pins: []string{"pin1", "pin2"}}
	s := EncodeCode(c)
	if !strings.HasPrefix(s, "ab2.") {
		t.Fatal(s)
	}
	got, err := DecodeCode(s)
	if err != nil || got.Offer != c.Offer || len(got.URLs) != 2 || len(got.Pins) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"ab2.", "ab2.!!!", "ab2." + base64.RawURLEncoding.EncodeToString([]byte(`{"u":[]}`)), s[:len(s)-10]} {
		if _, err := DecodeCode(bad); err == nil {
			t.Errorf("decoded %q", bad)
		}
	}
	legacy := base64.RawURLEncoding.EncodeToString([]byte("100.64.0.1:7742|" + master + "|Mirrin"))
	addr, tok, name, ok := DecodeLegacyCode(legacy)
	if !ok || addr != "100.64.0.1:7742" || tok != master || name != "Mirrin" {
		t.Fatalf("legacy %q %q %q %v", addr, tok, name, ok)
	}
	if _, _, _, ok := DecodeLegacyCode(s); ok {
		t.Fatal("an ab2 code read as a legacy one")
	}
	if _, _, _, ok := DecodeLegacyCode(base64.RawURLEncoding.EncodeToString([]byte("nonsense"))); ok {
		t.Fatal("nonsense read as a legacy code")
	}
}

// A terminal paired with a TLS listener trusts exactly the pinned keys.
func TestClientPinsTheTwinsKey(t *testing.T) {
	e := newEnv(t)
	_, tok, _ := e.store.Add("Laptop", devices.KindCLI, nil, "", "")
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.s.ServeHTTPAs(w, r, Remote, "127.0.0.1")
	})
	ts.StartTLS()
	defer ts.Close()
	pin := SPKIPin(ts.Certificate())
	if c := Dial(Target{Address: ts.URL, Token: tok, Pins: []string{"wrong", pin}}); c == nil {
		t.Fatal("the pinned twin wasn't reached")
	}
	if c := Dial(Target{Address: ts.URL, Token: tok, Pins: []string{"c29tZXRoaW5nIGVsc2U"}}); c != nil {
		t.Fatal("a twin with another key was trusted")
	}
	c := newClient(Target{Address: ts.URL, Token: tok, Pins: []string{"c29tZXRoaW5nIGVsc2U"}})
	if _, err := c.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "pair again") {
		t.Fatalf("pin mismatch error: %v", err)
	}
}

func TestClientShowsTheTwinsWords(t *testing.T) {
	e := newEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.s.Serve(ctx, ln, LoopbackOnly, "loopback") }()
	c := newClient(Target{Address: ln.Addr().String(), Token: "abt1_0000000000000000_" + strings.Repeat("A", 43)})
	_, err = c.Status(context.Background())
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 401 || ae.Code != "not_paired" || !strings.Contains(err.Error(), "isn't paired") {
		t.Fatalf("error %v", err)
	}
	// Pairing against it end to end.
	o := e.offer("cli")
	resp, err := Claim(context.Background(), "http://"+ln.Addr().String(), nil, o.Offer.ID, o.Offer.Secret, "Laptop", "cli")
	if err != nil || !devices.LooksLikeToken(resp.Token) {
		t.Fatalf("claim %+v %v", resp, err)
	}
	if _, err := Claim(context.Background(), "http://"+ln.Addr().String(), nil, o.Offer.ID, o.Offer.Secret, "Laptop", "cli"); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := Claim(context.Background(), "http://"+ln.Addr().String(), nil, "of_zzzzzzzzzz", "x", "Laptop", "cli"); err == nil || !strings.Contains(err.Error(), "restarted") {
		t.Fatalf("unknown offer: %v", err)
	}
}

// A terminal on an old-style code swaps the master key for its own over the
// old plain-HTTP listener, and uses the new key from then on.
func TestClientUpgradesAnOldStyleCode(t *testing.T) {
	e := newEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := listener{kind: kindLegacy, via: "tailscale"}
		e.s.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerKey, l)))
	}))
	defer ts.Close()
	c := ConnectWithToken(strings.TrimPrefix(ts.URL, "http://"), master)
	if c == nil {
		t.Fatal("an old-style code no longer connects")
	}
	resp, err := c.UpgradeLegacy(context.Background(), "Laptop (terminal)")
	if err != nil || c.Token() != resp.Token || !devices.LooksLikeToken(resp.Token) {
		t.Fatalf("upgrade: %+v %v", resp, err)
	}
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("with the new key: %v", err)
	}
}

func TestConnectUsesTheLoopbackAddress(t *testing.T) {
	e := newEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.s.Serve(ctx, ln, LoopbackOnly, "loopback") }()
	dir := t.TempDir()
	if err := os.WriteFile(TokenPath(dir), []byte(master+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	// api.listen 0.0.0.0 (or a Tailscale address): the terminal still comes
	// in on loopback, where the master key works.
	c := Connect(net.JoinHostPort("0.0.0.0", port), dir)
	if c == nil || c.Address() != "127.0.0.1:"+port {
		t.Fatalf("client %+v", c)
	}
}
