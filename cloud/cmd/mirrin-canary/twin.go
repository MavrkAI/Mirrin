package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/relay"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
)

// The canary twin is the smallest paying customer there can be: a linked
// machine with its own device key, entitlement, Let's Encrypt account and
// certificate, holding a tunnel to every relay its entitlement names, and
// answering one route, /healthz, with the id of the relay that carried the
// request. The probes reach it exactly as a phone reaches a real twin, so a
// green canary means the whole paid path works: DNS, both relays, the
// entitlement, the deny list and the certificate.

// twinDeps are what tests replace; zero values are production's.
type twinDeps struct {
	ACMEClient *http.Client   // talks to the CA; nil is the default client
	RelayRoots *x509.CertPool // the relays' control names; nil is canary.yaml's relay_roots, then the system roots
	Log        *slog.Logger
	// Status, if set, is told the status listener's address once it is
	// serving.
	Status func(addr string)
}

func (d twinDeps) log() *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Log
}

// errRestart ends one run of the twin when its entitlement comes to name
// other relays or hostnames; the twin starts again with them.
var errRestart = errors.New("the entitlement names other relays or names")

// runTwin runs the canary twin until ctx ends.
func runTwin(ctx context.Context, cfg *Config, d twinDeps) error {
	for {
		err := twinOnce(ctx, cfg, d)
		if ctx.Err() != nil {
			return nil
		}
		if !errors.Is(err, errRestart) {
			return err
		}
		d.log().Info("canary: starting again with the refreshed entitlement")
	}
}

func openClient(cfg *Config) (*cloud.Client, error) {
	keys, err := cfg.entKeys()
	if err != nil {
		return nil, err
	}
	return cloud.New(cfg.DataDir, cfg.API, keys)
}

func newACME(cfg *Config, hosts []string, d twinDeps, ready func(context.Context) error) (*tlsmgr.ACME, error) {
	return tlsmgr.NewACME(tlsmgr.ACMEConfig{
		Directory:  cfg.Twin.ACMEDirectory,
		Hostnames:  hosts,
		Dir:        filepath.Join(cfg.DataDir, "tls"),
		Email:      cfg.Twin.ACMEEmail,
		HTTPClient: d.ACMEClient,
		Ready:      ready,
		Log:        d.log(),
		OnExpiring: func(left time.Duration) {
			d.log().Error("canary: the certificate is close to expiry and hasn't renewed", "left", left.Round(time.Hour))
		},
	})
}

// pinnedFile records the ACME account the control plane was last told, so
// a restart doesn't rewrite the handle's CAA record for nothing.
func pinnedFile(cfg *Config) string {
	return filepath.Join(cfg.DataDir, "canary", "pinned-acme-account")
}

func readPinned(cfg *Config) string {
	b, _ := os.ReadFile(pinnedFile(cfg))
	return strings.TrimSpace(string(b))
}

func writePinned(cfg *Config, uri string) error {
	if err := os.MkdirAll(filepath.Dir(pinnedFile(cfg)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(pinnedFile(cfg), []byte(uri+"\n"), 0o600)
}

// twin is one run of the canary twin.
type twin struct {
	cfg    *Config
	d      twinDeps
	log    *slog.Logger
	client *cloud.Client
	claims entitle.Claims
	acme   *tlsmgr.ACME
	ln     *relay.Listener

	pinned chan struct{} // closed once the CAA record names this ACME account

	mu          sync.Mutex
	lastRefresh time.Time
}

func twinOnce(ctx context.Context, cfg *Config, d twinDeps) error {
	c, err := openClient(cfg)
	if err != nil {
		return err
	}
	cl, kind := c.State().Current(time.Now())
	switch kind {
	case cloud.Active, cloud.Grace:
	default:
		return fmt.Errorf("the canary isn't linked and current (%s); run mirrin-canary link", kind)
	}
	if len(cl.Hosts) == 0 || len(cl.Relays) == 0 {
		return errors.New("the canary's entitlement names no hostname or no relays")
	}
	key, err := c.TunnelKey()
	if err != nil {
		return err
	}
	roots := d.RelayRoots
	if roots == nil {
		if roots, err = loadRoots(cfg.Twin.RelayRoots); err != nil {
			return err
		}
	}
	statusKey := make([]byte, 32)
	rand.Read(statusKey)

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	t := &twin{cfg: cfg, d: d, log: d.log(), client: c, claims: cl, pinned: make(chan struct{})}
	t.acme, err = newACME(cfg, cl.Hosts, d, t.readyToIssue)
	if err != nil {
		return err
	}
	var refs []relay.RelayRef
	for _, r := range cl.Relays {
		refs = append(refs, relay.RelayRef{ID: r.ID, URL: r.URL})
	}
	t.ln, err = relay.Listen(ctx, relay.ClientConfig{
		Relays: refs,
		Key:    key,
		Entitlement: func() string {
			tok, _ := c.State().Entitlement()
			return tok
		},
		StatusKey:  statusKey,
		OnRefused:  t.refused,
		Client:     "mirrin-canary/" + version,
		RootCAs:    roots,
		MaxBackoff: 4 * time.Second,
		Log:        t.log,
	})
	if err != nil {
		return err
	}
	defer t.ln.Close()

	var wg sync.WaitGroup
	defer wg.Wait()
	goRun := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	goRun(func() { c.Keep(ctx, t.log) })
	// Pin first, then issue: registering the ACME account takes the lock
	// an issuance holds while it waits to be ready, so the two must not
	// race.
	goRun(func() {
		if t.pin(ctx) {
			t.acme.Run(ctx)
		}
	})
	goRun(func() { t.watch(ctx, cancel) })

	if cfg.Twin.StatusListen != "" {
		sl, err := net.Listen("tcp", cfg.Twin.StatusListen)
		if err != nil {
			return err
		}
		ss := &http.Server{Handler: t.statusHandler(), ReadHeaderTimeout: 5 * time.Second}
		goRun(func() { ss.Serve(sl) })
		goRun(func() { <-ctx.Done(); ss.Close() })
		if d.Status != nil {
			d.Status(sl.Addr().String())
		}
	}

	srv := &http.Server{
		Handler:           t.handler(),
		TLSConfig:         tlsmgr.TLSConfig(t.acme),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ConnContext:       relayContext,
		ErrorLog:          slog.NewLogLogger(t.log.Handler(), slog.LevelDebug),
	}
	goRun(func() { <-ctx.Done(); srv.Close() })
	t.log.Info("canary twin up", "host", cl.Hosts[0], "relays", len(refs))
	err = srv.Serve(tls.NewListener(t.ln, srv.TLSConfig))
	if cause := context.Cause(ctx); errors.Is(cause, errRestart) {
		return errRestart
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type relayKey struct{}

// relayContext records which relay carried a connection.
func relayContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	if rc, ok := c.(*relay.Conn); ok {
		return context.WithValue(ctx, relayKey{}, rc.Relay())
	}
	return ctx
}

// healthz is the canary's answer, which each probe checks names the relay
// it dialled.
type healthz struct {
	OK    bool      `json:"ok"`
	Host  string    `json:"host"`
	Relay string    `json:"relay"`
	Time  time.Time `json:"time"`
}

func (t *twin) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(relayKey{}).(string)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(healthz{OK: true, Host: t.claims.Hosts[0], Relay: id, Time: time.Now().UTC().Truncate(time.Second)})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "This is the Mirrin service canary. There is nothing else here.\n")
	})
	return mux
}

// twinStatus is /v1/status on the status listener.
type twinStatus struct {
	Host        string          `json:"host"`
	Handle      string          `json:"handle"`
	Certificate string          `json:"certificate"` // "" before the first
	NotAfter    time.Time       `json:"not_after,omitzero"`
	Tunnels     []tunnelSummary `json:"tunnels"`
	Healthy     bool            `json:"healthy"`
}

type tunnelSummary struct {
	Relay     string    `json:"relay"`
	Online    bool      `json:"online"`
	Since     time.Time `json:"since,omitzero"`
	LastError string    `json:"last_error,omitempty"`
	Refused   string    `json:"refused,omitempty"`
}

func (t *twin) status() twinStatus {
	s := twinStatus{Host: t.claims.Hosts[0], Handle: t.claims.Handle, Healthy: true}
	if leaf := t.acme.Leaf(); leaf != nil {
		s.Certificate, s.NotAfter = tlsmgr.SPKIPin(leaf), leaf.NotAfter
	} else {
		s.Healthy = false
	}
	for _, ts := range t.ln.Status() {
		sum := tunnelSummary{Relay: ts.Relay, Online: ts.Online, Since: ts.Since, LastError: ts.LastError}
		if ts.Refused != nil {
			sum.Refused = ts.Refused.Code
		}
		s.Tunnels = append(s.Tunnels, sum)
		s.Healthy = s.Healthy && ts.Online
	}
	return s
}

func (t *twin) statusHandler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, s twinStatus) {
		w.Header().Set("Content-Type", "application/json")
		if !s.Healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(s)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { write(w, t.status()) })
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) { write(w, t.status()) })
	return mux
}

// allOnline reports whether every tunnel is up: Let's Encrypt validates
// from several places at once, and each may arrive through either relay.
func (t *twin) allOnline() bool {
	st := t.ln.Status()
	for _, s := range st {
		if !s.Online {
			return false
		}
	}
	return len(st) > 0
}

// readyToIssue holds issuance until both tunnels are up and the handle's
// CAA record names this ACME account: a failed validation counts against
// Let's Encrypt's limits.
func (t *twin) readyToIssue(ctx context.Context) error {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-t.pinned:
			if t.allOnline() {
				return nil
			}
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// pin makes sure the handle's CAA record names this machine's ACME
// account, retrying until it does or ctx ends; it reports which.
func (t *twin) pin(ctx context.Context) bool {
	wait := 5 * time.Second
	for {
		err := t.pinOnce(ctx)
		if err == nil {
			close(t.pinned)
			return true
		}
		t.log.Warn("canary: couldn't pin the ACME account yet", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
		wait = min(wait*2, 10*time.Minute)
	}
}

func (t *twin) pinOnce(ctx context.Context) error {
	uri, err := t.acme.Register(ctx)
	if err != nil {
		return err
	}
	if readPinned(t.cfg) == uri {
		return nil
	}
	if err := t.client.SetACMEAccount(ctx, uri); err != nil {
		return err
	}
	if err := writePinned(t.cfg, uri); err != nil {
		return err
	}
	select { // the CAA record changes at the DNS provider meanwhile
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(t.cfg.Twin.PinSettle):
	}
	return nil
}

// refused hears a relay turn the canary away. An expired entitlement or a
// newer one elsewhere asks for a refresh, at most every 20 s. Superseded
// means something else holds the canary's handle: the probes fail, and
// that is the page.
func (t *twin) refused(relayID string, e wire.Error) {
	t.log.Warn("canary: a relay refused the tunnel", "relay", relayID, "code", e.Code, "message", e.Message)
	switch e.Code {
	case wire.CodeEntitlementExpired, wire.CodeSupersededRetry:
	default:
		return
	}
	t.mu.Lock()
	if time.Since(t.lastRefresh) < 20*time.Second {
		t.mu.Unlock()
		return
	}
	t.lastRefresh = time.Now()
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := t.client.Refresh(ctx); err != nil {
		t.log.Warn("canary: entitlement refresh failed", "err", err)
	}
}

// watch ends this run when the stored entitlement comes to name other
// relays or hosts, or stops being current.
func (t *twin) watch(ctx context.Context, cancel context.CancelCauseFunc) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cl, kind := t.client.State().Current(time.Now())
		if kind != cloud.Active && kind != cloud.Grace {
			t.log.Error("canary: the link is no longer current", "state", kind.String())
			continue // keep answering until the relays refuse; the probes tell
		}
		same := slices.Equal(cl.Hosts, t.claims.Hosts) && slices.EqualFunc(cl.Relays, t.claims.Relays, func(a, b entitle.Relay) bool { return a.ID == b.ID && a.URL == b.URL })
		if !same {
			cancel(errRestart)
			return
		}
	}
}

// link links the canary with the control plane as `mirrin cloud link`
// does, pinning its ACME account in the handle's CAA record from the start.
// The checkout is paid by the operator (a 100% discount code at the
// merchant of record). open is given the checkout URL; poll is how often
// the link is asked about, and limit how long to wait.
func link(ctx context.Context, cfg *Config, d twinDeps, out io.Writer, open func(string) error, poll, limit time.Duration) error {
	c, err := openClient(cfg)
	if err != nil {
		return err
	}
	if info, ok, err := c.State().Info(); err != nil {
		return err
	} else if ok {
		fmt.Fprintf(out, "Already linked to %s as %s.\n", info.API, info.Handle)
		return nil
	}
	ls, _, pending := c.PendingLink()
	var uri string
	if !pending {
		host := cfg.Twin.Handle + "." + cfg.TenantZone
		if cfg.Twin.Handle == "" {
			return errors.New("twin.handle: name the canary's handle before linking")
		}
		a, err := newACME(cfg, []string{host}, d, nil)
		if err != nil {
			return err
		}
		if uri, err = a.Register(ctx); err != nil {
			return err
		}
		if ls, err = c.StartLink(ctx, cfg.Twin.Handle, uri); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Pay for the canary's handle at:\n  %s\n(use the operator's 100%% discount code), then wait here.\n", ls.CheckoutURL)
	if open != nil {
		if err := open(ls.CheckoutURL); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(limit)
	for {
		st, _, err := c.PollLink(ctx, ls.ID)
		if err != nil {
			return err
		}
		switch st {
		case cloud.LinkActive:
			if uri != "" {
				if err := writePinned(cfg, uri); err != nil {
					return err
				}
			}
			info, _, _ := c.State().Info()
			fmt.Fprintf(out, "Linked as %s. Start the twin: mirrin-canary twin --config canary.yaml\n", info.Handle)
			return nil
		case cloud.LinkExpired:
			return errors.New("the checkout expired unpaid; run mirrin-canary link again")
		}
		if time.Now().After(deadline) {
			return errors.New("still unpaid; run mirrin-canary link again to keep waiting")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
