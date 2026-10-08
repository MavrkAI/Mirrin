package api

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// Pairing v2. The owner mints a single-use offer on this computer
// (POST /devices/offers, which `mirrin pair` calls); the new device claims it
// with the offer's secret (POST /pair/claim) and gets a key of its own.
// Browsers get a link, https://<host>/pair#v=2&o=<offer>&s=<secret>&n=<name>,
// whose secret stays in the #fragment and so never reaches a server log.
// Terminals get a code, ab2.<base64url JSON>, carrying the addresses to try,
// the offer and, for TLS listeners, the certificate pins.

// How says how a device came to have its own key.
type How string

// The ways a device gets its own key.
const (
	HowClaimed    How = "claimed"     // a pairing link or code
	HowLegacyCode How = "legacy_code" // a terminal paired with an old-style code, moved onto its own key
)

// PairEvent is a device that just got its own key.
type PairEvent struct {
	Device devices.Device
	IP     string
	Via    string
	How    How
}

func (s *Server) announce(e PairEvent) {
	if s.onPaired != nil {
		s.onPaired(e)
	}
}

// Base is an address other devices reach the twin at, with the SPKI pins of
// its certificate when it is TLS.
type Base struct {
	URL  string   `json:"url"`
	Pins []string `json:"pins,omitempty"`
}

// PairBases lists the addresses a pairing code or link should carry: the
// remote (TLS) listeners, then the plain-HTTP api.remote address (for a
// wildcard, each Tailscale and LAN address this computer has).
func (s *Server) PairBases() []Base {
	out := s.remoteBases()
	if !s.remote {
		return out
	}
	host, port, err := net.SplitHostPort(s.addr)
	if err != nil || isLoopbackHost(host) {
		return out
	}
	if !isUnspecified(host) {
		return append(out, Base{URL: "http://" + net.JoinHostPort(host, port)})
	}
	for _, h := range candidateHosts() {
		out = append(out, Base{URL: "http://" + net.JoinHostPort(h, port)})
	}
	return out
}

// candidateHosts lists addresses another device might reach this computer
// on: Tailscale first, then the LAN, then the host name.
func candidateHosts() []string {
	var ts, lan []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip := ipn.IP.String()
			if tailnet.Contains(addrOf(&net.TCPAddr{IP: ipn.IP})) {
				ts = append(ts, ip)
			} else if ipn.IP.IsPrivate() {
				lan = append(lan, ip)
			}
		}
	}
	hosts := append(ts, lan...)
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	return hosts
}

// SPKIPin is a certificate's pin: base64url SHA-256 of its public key info.
func SPKIPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Code is what a terminal's pairing code carries.
type Code struct {
	URLs   []string `json:"u"`
	Offer  string   `json:"o"`
	Secret string   `json:"s"`
	Name   string   `json:"n,omitempty"`
	Pins   []string `json:"fp,omitempty"`
}

// CodePrefix starts every pairing code this version makes.
const CodePrefix = "ab2."

// EncodeCode makes an ab2. code.
func EncodeCode(c Code) string {
	b, _ := json.Marshal(c)
	return CodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCode reads an ab2. code.
func DecodeCode(s string) (Code, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), CodePrefix)
	if !ok {
		return Code{}, errors.New("not an ab2 code")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(rest, "="))
	if err != nil {
		return Code{}, errors.New("the code is cut short or mistyped")
	}
	var c Code
	if err := json.Unmarshal(b, &c); err != nil || c.Offer == "" || c.Secret == "" || len(c.URLs) == 0 {
		return Code{}, errors.New("the code is cut short or mistyped")
	}
	return c, nil
}

// DecodeLegacyCode reads an old-style code (base64url of address|token|name,
// with no prefix). They carry the master key and work only over plain-HTTP
// api.remote.
func DecodeLegacyCode(s string) (addr, token, name string, ok bool) {
	s = strings.TrimSpace(s)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return "", "", "", false
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) < 2 || parts[1] == "" {
		return "", "", "", false
	}
	if _, _, err := net.SplitHostPort(parts[0]); err != nil {
		return "", "", "", false
	}
	if len(parts) == 3 {
		name = parts[2]
	}
	return parts[0], parts[1], name, true
}

// PairLink is the link a browser opens to claim an offer.
func PairLink(base string, o devices.Offer, name string) string {
	v := "v=2&o=" + url.QueryEscape(o.ID) + "&s=" + url.QueryEscape(o.Secret)
	if name != "" {
		v += "&n=" + url.QueryEscape(name)
	}
	if o.Kind == devices.KindKiosk {
		v += "&k=kiosk"
	}
	return strings.TrimRight(base, "/") + "/pair#" + v
}

// OfferResponse is what POST /devices/offers returns: the offer, and ready
// links (browsers) or a code (terminals) for each address.
type OfferResponse struct {
	Offer devices.Offer `json:"offer"`
	Name  string        `json:"name"`
	Bases []Base        `json:"bases"`
	Links []string      `json:"links,omitempty"`
	Code  string        `json:"code,omitempty"`
}

// Compose builds the links and code for bases.
func (o *OfferResponse) Compose(bases []Base) {
	o.Bases, o.Links, o.Code = bases, nil, ""
	if len(bases) == 0 {
		return
	}
	if o.Offer.Kind == devices.KindCLI {
		c := Code{Offer: o.Offer.ID, Secret: o.Offer.Secret, Name: o.Name}
		for _, b := range bases {
			c.URLs = append(c.URLs, b.URL)
			for _, p := range b.Pins {
				if !contains(c.Pins, p) {
					c.Pins = append(c.Pins, p)
				}
			}
		}
		o.Code = EncodeCode(c)
		return
	}
	for _, b := range bases {
		o.Links = append(o.Links, PairLink(b.URL, o.Offer, o.Name))
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ClaimRequest is the body of POST /pair/claim.
type ClaimRequest struct {
	Offer  string `json:"o"`
	Secret string `json:"s"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

// ClaimResponse is what a successful claim returns: the device, and its token
// (terminals, kiosks) or an install ticket (phone browsers, which get the
// token only as a cookie).
type ClaimResponse struct {
	Device devices.Device `json:"device"`
	Token  string         `json:"token,omitempty"`
	Ticket string         `json:"ticket,omitempty"`
}

func (s *Server) pairRoutes(mux *http.ServeMux, local, remote authz) {
	mux.HandleFunc("GET /pair", remote.Public(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		if listenerFrom(r.Context()).kind != kindRemote {
			h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		}
		_, _ = w.Write([]byte(pairHTML))
	}))
	mux.HandleFunc("POST /pair/claim", remote.Public(s.claim))
	mux.HandleFunc("POST /pair/ticket", remote.Public(s.redeemTicket))
	mux.HandleFunc("POST /pair/seen", remote.Public(s.pairSeen)) // pages_add.go
	mux.HandleFunc("POST /pair/upgrade", remote.Require(devices.View, s.upgradeLegacy))
	mux.HandleFunc("GET /devices", local.Local(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"devices": s.Devices().List(), "notifications": s.notifying()})
	}))
	mux.HandleFunc("POST /devices/offers", local.Local(s.newOffer))
	mux.HandleFunc("POST /devices/{id}/revoke", local.Local(func(w http.ResponseWriter, r *http.Request) {
		res, err := s.RevokeDevice(r.PathValue("id"))
		if errors.Is(err, devices.ErrNotFound) {
			s.fail(w, r, http.StatusNotFound, apiError{Error: "no_device", Message: "There's no paired device with that id.", Fix: "Open Devices from the menu bar to see them."})
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, res)
	}))
	mux.HandleFunc("POST /devices/{id}/rename", local.Local(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		d, err := s.Devices().Rename(r.PathValue("id"), req.Name)
		switch {
		case errors.Is(err, devices.ErrNotFound):
			s.fail(w, r, http.StatusNotFound, apiError{Error: "no_device", Message: "There's no paired device with that id.", Fix: "Open Devices from the menu bar to see them."})
		case err != nil:
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_name", Message: err.Error() + "."})
		default:
			writeJSON(w, map[string]any{"device": d})
		}
	}))
}

func (s *Server) newOffer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind   string          `json:"kind"`
		Scopes []devices.Scope `json:"scopes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Kind == "" {
		req.Kind = devices.KindPWA
	}
	o, err := s.Devices().NewOffer(req.Kind, req.Scopes, 0)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_offer", Message: err.Error() + "."})
		return
	}
	resp := OfferResponse{Offer: o, Name: s.name}
	resp.Compose(s.PairBases())
	writeJSON(w, resp)
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		s.fail(w, r, http.StatusBadRequest, apiError{Error: "bad_request", Message: "That pairing request didn't make sense.", Fix: "Open the pairing link again."})
		return
	}
	p := PeerFrom(r.Context())
	ip := ipString(p.ClientIP)
	store := s.Devices()
	d, tok, err := store.Claim(req.Offer, req.Secret, req.Name, req.Kind, p.Via, ip)
	again := "Make a new one on the computer " + s.twinName() + " runs on: run `mirrin pair`."
	switch {
	case errors.Is(err, devices.ErrOfferUnknown):
		http.NotFound(w, r) // the same as any unknown route
		return
	case errors.Is(err, devices.ErrOfferUsed):
		s.fail(w, r, http.StatusGone, apiError{Error: "offer_used", Message: "This pairing link was already used.", Fix: again})
		return
	case errors.Is(err, devices.ErrOfferExpired):
		s.fail(w, r, http.StatusGone, apiError{Error: "offer_expired", Message: "This pairing link has expired (they last 10 minutes).", Fix: again})
		return
	case errors.Is(err, devices.ErrOfferRevoked):
		s.fail(w, r, http.StatusGone, apiError{Error: "offer_withdrawn", Message: "This pairing code was replaced by a newer one, or its page was closed.", Fix: again})
		return
	case errors.Is(err, devices.ErrOfferBurned):
		s.fail(w, r, http.StatusGone, apiError{Error: "offer_burned", Message: "This pairing link had too many wrong tries, so it no longer works.", Fix: again})
		return
	case errors.Is(err, devices.ErrBadSecret):
		s.fail(w, r, http.StatusForbidden, apiError{Error: "wrong_secret", Message: "This pairing link is incomplete or was changed.", Fix: "Copy the whole link, or scan the QR code again."})
		return
	case errors.Is(err, devices.ErrRateLimited):
		w.Header().Set("Retry-After", strconv.Itoa(int(store.RetryAfter(ip).Seconds()+0.999)))
		s.fail(w, r, http.StatusTooManyRequests, apiError{Error: "slow_down", Message: "Too many pairing attempts from this address.", Fix: "Wait a minute, then try again."})
		return
	case errors.Is(err, devices.ErrWrongKind):
		s.fail(w, r, http.StatusBadRequest, apiError{Error: "wrong_kind", Message: "This pairing is for a different kind of device.",
			Fix: "For a phone, tablet or screen, use Add your phone… in the menu bar; for another computer's terminal, type mirrin pair in Terminal here and mirrin connect on the other computer."})
		return
	case err != nil:
		http.Error(w, "couldn't save the new device: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := ClaimResponse{Device: d}
	switch d.Kind {
	case devices.KindCLI:
		resp.Token = tok
	case devices.KindKiosk:
		resp.Token = tok
		s.setDeviceCookie(w, r, tok)
	default:
		resp.Ticket = store.NewTicket(d.ID, tok)
		s.setDeviceCookie(w, r, tok)
	}
	go s.announce(PairEvent{Device: d, IP: ip, Via: p.Via, How: HowClaimed})
	s.addClaimed(req.Offer, d) // pages_add.go
	writeJSON(w, resp)
}

// redeemTicket gives a Home Screen web app (whose storage is separate from
// the browser's) the cookie its pairing set in the browser.
func (s *Server) redeemTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		T string `json:"t"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	d, tok, err := s.Devices().RedeemTicket(req.T)
	if err != nil {
		s.fail(w, r, http.StatusGone, apiError{Error: "ticket_used", Message: "This app's pairing ticket was used or has expired.",
			Fix: "Pair again: on the computer " + s.twinName() + " runs on, choose Add your phone… from the menu bar."})
		return
	}
	s.setDeviceCookie(w, r, tok)
	s.addProgress(d.ID, stepInstalled) // pages_add.go
	writeJSON(w, map[string]any{"device": d})
}

// upgradeLegacy moves a terminal that still uses an old-style code (the
// master key, over plain-HTTP api.remote) onto a key of its own.
func (s *Server) upgradeLegacy(w http.ResponseWriter, r *http.Request) {
	p := PeerFrom(r.Context())
	if !p.Master || !p.legacy {
		s.fail(w, r, http.StatusBadRequest, apiError{Error: "not_needed", Message: "Only a terminal paired with an old-style code needs this."})
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	ip := ipString(p.ClientIP)
	if req.Name = devices.CleanName(req.Name); req.Name == "" {
		req.Name = "Computer at " + ip
	}
	d, tok, err := s.Devices().AddShared(req.Name, devices.KindCLI, p.Via, ip)
	if err != nil {
		http.Error(w, "couldn't save the new device: "+err.Error(), http.StatusInternalServerError)
		return
	}
	go s.announce(PairEvent{Device: d, IP: ip, Via: p.Via, How: HowLegacyCode})
	writeJSON(w, ClaimResponse{Device: d, Token: tok})
}

// DescribeScopes says in words what a device may do.
func DescribeScopes(scopes []devices.Scope) string {
	var words []string
	for _, sc := range scopes {
		words = append(words, scopeWords(sc))
	}
	switch len(words) {
	case 0:
		return "nothing"
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// Expiry says when an offer stops working, in the owner's time.
func Expiry(o devices.Offer) string {
	return fmt.Sprintf("%s (in %d minutes)", o.Expires.Local().Format("15:04"), int(time.Until(o.Expires).Round(time.Minute).Minutes()))
}

// pairHTML is the page a pairing link opens. The secret is read from the
// #fragment, which browsers never send, and wiped from the address bar
// before anything else happens.
const pairHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="referrer" content="no-referrer">
<title>Pair this device</title>
<style>
:root{--bg:#07080b;--card:#111318;--line:#1c2029;--fg:#eceef3;--muted:#9aa1b1;--accent:#7aa2ff;--ok:#3ddc84;--fail:#ff6b6b}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:17px/1.5 -apple-system,system-ui,"Inter",sans-serif;padding:max(16px,env(safe-area-inset-top)) 16px}
main{width:100%;max-width:26rem;background:var(--card);border:1px solid var(--line);border-radius:1rem;padding:1.6rem}
h1{font-size:1.35rem;margin:0 0 .5rem}
p{color:var(--muted);margin:.4rem 0 1rem}
label{display:block;font-size:.9rem;color:var(--muted);margin-bottom:.3rem}
input{width:100%;font:inherit;font-size:max(16px,1rem);padding:.7rem .8rem;border-radius:.6rem;border:1px solid var(--line);background:#0c0e13;color:var(--fg)}
button{margin-top:1rem;width:100%;font:inherit;font-weight:600;border:0;border-radius:.7rem;padding:.8rem;background:var(--accent);color:#07080b;cursor:pointer}
button:disabled{opacity:.5}
.msg{margin-top:1rem;min-height:1.5em}.msg.err{color:var(--fail)}.msg.ok{color:var(--ok)}
code{background:#0c0e13;padding:.1rem .35rem;border-radius:.3rem;color:var(--fg)}
</style>
</head>
<body>
<main>
<h1 id="title">Pair this device</h1>
<p id="lead">Checking the link…</p>
<form id="f" hidden>
<label for="name">Name this device</label>
<input id="name" maxlength="60" autocomplete="off">
<button id="go" type="submit">Pair this device</button>
</form>
<div id="msg" class="msg" role="status" aria-live="polite"></div>
</main>
<script>
(function(){
  const $ = s => document.getElementById(s);
  const h = new URLSearchParams(location.hash.slice(1));
  history.replaceState(null, '', location.pathname); // the secret leaves the address bar at once
  const o = h.get('o'), s = h.get('s'), twin = h.get('n') || 'your twin', kind = h.get('k') === 'kiosk' ? 'kiosk' : 'pwa';
  document.title = 'Pair with ' + twin;
  $('title').textContent = 'Pair with ' + twin;
  if (h.get('v') !== '2' || !o || !s) {
    $('lead').innerHTML = 'This link is incomplete. On the computer ' + esc(twin) + ' runs on, choose Add your phone… from the menu bar icon (or type <code>mirrin pair --screen</code> in Terminal), then scan the code it shows.';
    return;
  }
  $('lead').textContent = kind === 'kiosk'
    ? 'This screen will show what ' + twin + ' is doing. Only pair screens you own.'
    : 'This device will see ' + twin + '\'s screen, talk to it and approve what it asks. Only pair your own devices.';
  $('name').value = guess();
  $('f').hidden = false;
  // The computer's "Add your phone" page lights "Scanned" (it learns nothing else).
  fetch('/pair/seen', {method:'POST', headers:{'Content-Type':'application/json'}, credentials:'omit', body: JSON.stringify({o, s})}).catch(() => {});
  $('f').onsubmit = async ev => {
    ev.preventDefault();
    $('go').disabled = true;
    say('Pairing…', '');
    try {
      const r = await fetch('/pair/claim', {method:'POST', headers:{'Content-Type':'application/json'}, credentials:'same-origin',
        body: JSON.stringify({o, s, name: $('name').value.trim(), kind})});
      if (r.ok) {
        say('Paired. Opening ' + twin + '…', 'ok');
        setTimeout(() => location.replace('/ui'), 600);
        return;
      }
      let e = {};
      try { e = await r.json(); } catch (_) {}
      if (r.status === 404) e = {message: 'This pairing link isn\'t known here. ' + twin + ' may have restarted since it was made.', fix: 'Make a new one: on the computer ' + twin + ' runs on, choose Add your phone… from the menu bar icon.'};
      say((e.message || 'Pairing failed.') + (e.fix ? ' ' + e.fix : ''), 'err');
      if (r.status === 429 || r.status === 403) $('go').disabled = false;
    } catch (_) {
      say('Couldn\'t reach ' + twin + '. Check this device is on the same network (or Tailscale), then try again.', 'err');
      $('go').disabled = false;
    }
  };
  function say(t, cls) { const m = $('msg'); m.textContent = t; m.className = 'msg ' + cls; }
  function esc(t) { return String(t).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
  function guess() {
    const u = navigator.userAgent;
    const dev = /iPhone/.test(u) ? 'iPhone' : /iPad/.test(u) ? 'iPad' : /Android/.test(u) ? 'Android phone' : /CrOS/.test(u) ? 'Chromebook' : /Macintosh/.test(u) ? 'Mac' : /Windows/.test(u) ? 'Windows PC' : /Linux/.test(u) ? 'Linux computer' : '';
    return kind === 'kiosk' ? 'Wall screen' : (dev || 'My phone');
  }
})();
</script>
</body>
</html>
`
