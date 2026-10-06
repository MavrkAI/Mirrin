package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// The relay terminates TLS for exactly one name, its control name, which
// serves the tunnel endpoint, the status endpoint and health. Every other
// name is spliced, still encrypted, to the daemon that holds it. There is
// no code path that loads or serves a certificate for any other name.

// errNotControlName is GetCertificate's answer for every name but the
// control name.
var errNotControlName = errors.New("relay: no certificate for that name; tenant TLS ends on the daemon")

// certSource returns the control name's certificate source: the test or
// caller's override, the configured files, or autocert. autocert answers
// TLS-ALPN-01 for the control name on this node only, and its host policy
// refuses every other name.
func certSource(cfg *Config, override func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	if override != nil {
		return override, nil
	}
	if cfg.CertFile != "" {
		c, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("control certificate: %w", err)
		}
		return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &c, nil }, nil
	}
	return newAutocert(cfg).GetCertificate, nil
}

func newAutocert(cfg *Config) *autocert.Manager {
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(cfg.ControlHostname),
		Cache:      autocert.DirCache(filepath.Join(cfg.StateDir, "autocert")),
		Email:      cfg.ACMEEmail,
		Client:     &acme.Client{DirectoryURL: cfg.ACMEDirectory},
	}
}

// getCertificate is the only GetCertificate in the relay. It answers for
// the control name and refuses everything else, including tenant names and
// acme-tls/1 hellos for them, which are spliced to the daemon and never
// reach it. Trouble with the control name's own certificate is logged:
// the HTTP server's handshake errors are not.
func (s *Server) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if lowerASCII(hello.ServerName) != s.cfg.ControlHostname {
		return nil, errNotControlName
	}
	c, err := s.certs(hello)
	if !slices.Contains(hello.SupportedProtos, acme.ALPNProto) { // not a TLS-ALPN-01 probe
		s.watchCert(c, err)
	}
	return c, err
}

// controlTLS serves the control name: TLS 1.3, HTTP/1.1 (the tunnel is a
// WebSocket upgrade), and acme-tls/1 so autocert can answer TLS-ALPN-01.
func (s *Server) controlTLS() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		NextProtos:     []string{"http/1.1", acme.ALPNProto},
		GetCertificate: s.getCertificate,
	}
}

// controlServer is the HTTP server behind the control name.
func (s *Server) controlServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+wire.Path, s.serveTunnel)
	mux.HandleFunc("GET /v1/status/{handle}", s.serveStatus)
	mux.HandleFunc("GET /healthz", serveHealth)
	mux.HandleFunc("GET /v1/version", s.serveVersion)
	return &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// An acme-tls/1 connection has done its job once the handshake
		// is over. A non-nil map also keeps HTTP/2 off.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){
			acme.ALPNProto: func(*http.Server, *tls.Conn, http.Handler) {},
		},
		ErrorLog: log.New(discardWriter{}, "", 0),
	}
}

func serveHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("ok\n"))
}

func (s *Server) serveVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(struct {
		Relay    string `json:"relay"`
		Version  string `json:"version"`
		Protocol string `json:"protocol"`
		Mode     string `json:"mode"`
	}{s.cfg.ID, s.version, wire.Subprotocol, s.mode()})
	w.Write(b)
}

func (s *Server) mode() string {
	if s.cfg.SelfHosted() {
		return "self-host"
	}
	return "hosted"
}

// ServeRedirect answers plain HTTP on ln, for any name, with only a 308 to
// the same URL over https.
func (s *Server) ServeRedirect(ln net.Listener) error {
	srv := &http.Server{
		Handler:           http.HandlerFunc(redirect),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(discardWriter{}, "", 0),
	}
	return s.serveHTTP(srv, ln)
}

func redirect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = lowerASCII(host)
	if !wire.ValidHostname(host) {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
}

// ServeMetrics serves /metrics and /healthz on ln, and, when ln is on
// loopback, /debug/connlog for the operator over SSH.
func (s *Server) ServeMetrics(ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		var b bytes.Buffer
		s.m.write(&b, s.now())
		w.Write(b.Bytes())
	})
	mux.HandleFunc("GET /healthz", serveHealth)
	if isLoopback(ln.Addr()) {
		mux.HandleFunc("GET /debug/connlog", func(w http.ResponseWriter, r *http.Request) {
			recs, hours := s.connLog.read(lowerASCII(r.URL.Query().Get("handle")))
			w.Header().Set("Content-Type", "application/json")
			b, _ := json.Marshal(struct {
				Recent []connRecord `json:"recent"`
				Hourly []hourTotal  `json:"hourly"`
			}{recs, hours})
			w.Write(b)
		})
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(discardWriter{}, "", 0),
	}
	return s.serveHTTP(srv, ln)
}

func isLoopback(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	return ok && ta.IP.IsLoopback()
}

// chanListener is a net.Listener fed by the router: every connection whose
// ClientHello names the control name.
type chanListener struct {
	addr net.Addr
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{addr: addr, ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// deliver hands c to the control server, or closes it once the listener
// is closed.
func (l *chanListener) deliver(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		c.Close()
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

// replayConn gives back the bytes the router peeked before reading on.
type replayConn struct {
	net.Conn
	replay []byte
}

func (c *replayConn) Read(p []byte) (int, error) {
	if c.replay != nil {
		n := copy(p, c.replay)
		if c.replay = c.replay[n:]; len(c.replay) == 0 {
			c.replay = nil // let the peek buffer go
		}
		return n, nil
	}
	return c.Conn.Read(p)
}

// remoteAddr parses an http.Request's RemoteAddr.
func remoteAddr(s string) netip.AddrPort {
	ap, _ := netip.ParseAddrPort(s)
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
