package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type RemoteOptions struct {
	Hostnames  []string
	Via        string
	AllowAdmin bool
	Limits     Limits
	hostnames  func() []string
}

func remoteRoute(p string) bool {
	switch p {
	case "/usage", "/ui", "/screen", "/screen/shot", "/events", "/status", "/message", "/message/stream", "/pair", "/pair/claim", "/pair/ticket", "/pair/seen", "/start", "/manifest.webmanifest", "/sw.js", "/offline", "/healthz", "/devices/self/revoke", "/characters.js":
		return true
	}
	for _, prefix := range []string{"/approvals/", "/approve/", "/push/", "/stepup/", "/pwa/", "/icons/", "/phone/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
func (s *Server) remoteHandler(o RemoteOptions) http.Handler {
	s.once.Do(s.initHandlers)
	anonymous, authenticated := o.Limits.buckets()
	next := s.routeHandler
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("Content-Security-Policy", remoteCSP)
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		hosts := o.Hostnames
		if o.hostnames != nil {
			hosts = o.hostnames()
		}
		l := listener{kind: kindRemote, via: o.Via, hosts: hosts, admin: o.AllowAdmin, remotePolicy: true}
		r = r.WithContext(context.WithValue(r.Context(), listenerKey, l))
		if !l.answers(r.Host) {
			s.fail(w, r, 421, s.wrongAddress())
			return
		}
		tok, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !bearer {
			tok = ""
		}
		tok = strings.TrimSpace(tok)
		if r.Header.Get("Authorization") == "" {
			tok = secureCookie(r)
		}
		d, valid := s.Devices().Authenticate(tok)
		bucket := anonymous
		if valid {
			bucket = authenticated
		}
		if ok, wait := bucket.allow(r.RemoteAddr); !ok {
			h.Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
			s.fail(w, r, 429, s.slowDown())
			return
		}
		if r.URL.Query().Has("token") || (r.Header.Get("Authorization") != "" && !valid) {
			s.fail(w, r, 401, s.notPaired(l, false))
			return
		}
		if !remoteRoute(r.URL.Path) && !(o.AllowAdmin && valid && d.Has(devices.Admin)) {
			s.fail(w, r, 404, s.notHere())
			return
		}
		if !safeMethod(r.Method) && !s.proxyExempt(r.URL.Path) && !remoteOriginOK(r) {
			s.fail(w, r, 403, s.crossSite())
			return
		}
		var held bool
		if r, held = s.alarmHold(w, r, d, valid); held { // alarm.go: certificate alarm (423, Clear-Site-Data)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeRemote terminates TLS itself, including HTTP/2. Configure mounts and
// other configuration before starting listeners; admin policy is per listener.
func (s *Server) ServeRemote(ctx context.Context, ln net.Listener, src tlsmgr.Source, o RemoteOptions) error {
	defer ln.Close()
	if len(o.Hostnames) == 0 {
		o.Hostnames = src.Hostnames()
		o.hostnames = src.Hostnames
	}
	if len(o.Hostnames) == 0 {
		return errors.New("no HTTPS hostname is available; check your certificate")
	}
	srv := &http.Server{Handler: s.remoteHandler(o), TLSConfig: tlsmgr.TLSConfig(src), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }, ConnContext: connContext(listener{kind: kindRemote, via: o.Via, hosts: o.Hostnames, admin: o.AllowAdmin, remotePolicy: true}, false)}
	base := Base{URL: "https://" + o.Hostnames[0], Pins: src.SPKIs()}
	if _, port, e := net.SplitHostPort(ln.Addr().String()); e == nil && port != "443" {
		base.URL = "https://" + net.JoinHostPort(o.Hostnames[0], port)
	}
	s.lmu.Lock()
	s.reached = append(s.reached, base)
	if s.remoteIdentities == nil {
		s.remoteIdentities = make(map[string]func() Base)
	}
	s.remoteIdentities[base.URL] = func() Base {
		b := base
		b.Pins = src.SPKIs()
		if o.hostnames != nil {
			if hosts := o.hostnames(); len(hosts) > 0 {
				_, port, _ := net.SplitHostPort(ln.Addr().String())
				b.URL = "https://" + hosts[0]
				if port != "443" {
					b.URL = "https://" + net.JoinHostPort(hosts[0], port)
				}
			}
		}
		return b
	}
	s.remoteLeaf(base.URL, o.Via, src) // pages_trust.go: the Trust page shows it
	s.lmu.Unlock()
	defer func() {
		s.lmu.Lock()
		defer s.lmu.Unlock()
		delete(s.remoteIdentities, base.URL)
		delete(s.remoteLeaves, base.URL)
		for i, b := range s.reached {
			if b.URL == base.URL {
				s.reached = append(s.reached[:i], s.reached[i+1:]...)
				break
			}
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stop()
	err := srv.ServeTLS(ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) initHandlers() {
	s.routeHandler = s.pwaHandler(s.routes())
	s.handler = s.gate(s.routeHandler)
}

// Requests with cookies or browser metadata must prove their origin. Native
// bearer requests and credential-less pairing claims have no ambient authority.
func remoteOriginOK(r *http.Request) bool {
	browser := carriesCookie(r) || r.Header.Get("Origin") != ""
	for k := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Sec-Fetch-") {
			browser = true
		}
	}
	return !browser || r.Header.Get("Origin") == "https://"+r.Host && r.Header.Get("Sec-Fetch-Site") == "same-origin"
}
func (s *Server) remoteAdmin(l listener) bool {
	if l.kind != kindRemote {
		return false
	}
	if l.remotePolicy {
		return l.admin
	}
	return s.adminRemote
}
func (s *Server) remoteBases() []Base {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	out := append([]Base(nil), s.reached...)
	for i, b := range out {
		if source := s.remoteIdentities[b.URL]; source != nil {
			out[i] = source()
		}
		out[i].Pins = append([]string(nil), out[i].Pins...)
	}
	return out
}

// Tailscale's public certificates rotate keys. A stale pin may fall back only
// to a trusted CA chain for the exact requested *.ts.net hostname. Other
// pinned origins keep their original pin-only trust model.
func verifyTailscaleRenewal(cs tls.ConnectionState, roots *x509.CertPool) error {
	if len(cs.PeerCertificates) > 0 && strings.HasSuffix(strings.ToLower(cs.ServerName), ".ts.net") {
		intermediates := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{DNSName: cs.ServerName, Roots: roots, Intermediates: intermediates}); err == nil {
			return nil
		}
	}
	return errors.New("the twin's certificate isn't the one it was paired with; if you changed how it's reached, pair again with `mirrin pair`")
}
