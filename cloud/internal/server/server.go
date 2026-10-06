// Package server is mirrin-cloud, the Mirrin Cloud control plane: the
// smallest server that turns a payment into a handle, its DNS records and a
// signed entitlement. docs/cloud-api.md is the wire contract, which
// cloudtest.Contract checks against it. It keeps no password, no email and
// no IP address, and GET /v1/me shows everything it keeps about an account.
package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/billing"
	"github.com/MavrkAI/Mirrin/cloud/internal/dns"
	"github.com/MavrkAI/Mirrin/cloud/internal/keys"
	"github.com/MavrkAI/Mirrin/cloud/internal/storage"
	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// Lifetimes (docs/cloud-design.md §10).
const (
	linkTTL       = time.Hour           // a checkout holds its handle this long
	entMax        = 35 * 24 * time.Hour // an entitlement lasts at most this
	entGrace      = 7 * 24 * time.Hour  // and at most this past paid_through
	deleteWait    = 7 * 24 * time.Hour  // DELETE /v1/account can be undone this long
	denyKeep      = entMax + 24*time.Hour
	denyForget    = 400 * 24 * time.Hour
	linkForget    = 30 * 24 * time.Hour // an expired or refused link of no account is dropped after this
	billingForget = 90 * 24 * time.Hour
	signingSkew   = 300 * time.Second
	maxBody       = 64 << 10
	sweepEvery    = time.Minute
)

// Options are the parts New does not build from Config.
type Options struct {
	Store   *store.Store
	Billing billing.Provider
	DNS     dns.Provider
	Keys    *keys.Set
	// Storage keeps backup objects; nil means this server keeps no
	// backups, and the backup routes answer 404.
	Storage storage.Provider
	// Dev adds the dev routes (docs/cloud-api.md §7), and DevBilling serves
	// the fake checkout and portal pages. Production leaves both unset.
	Dev        bool
	DevBilling *billing.Fake
	// DevStorage is served at its own path in --dev mode, standing in for
	// the bucket the presigned URLs point at.
	DevStorage *storage.Fake
	Now        func() time.Time
	Log        *slog.Logger
	Version    string
}

// Server is the control plane.
type Server struct {
	cfg        Config
	store      *store.Store
	billing    billing.Provider
	dns        dns.Provider
	keys       *keys.Set
	storage    storage.Provider
	dev        bool
	devBilling *billing.Fake
	devStorage *storage.Fake
	now        func() time.Time
	log        *slog.Logger
	ver        string

	origin  string // the public origin, normalized
	iss     string
	relays  []entitle.Relay
	v4, v6  []netip.Addr
	trusted []netip.Prefix // trusted_proxies
	handler http.Handler

	// Nonces of device keys, and of every other key (limits.go).
	nonces, otherNonces *httpsig.MemoryNonceCache
	// Rate limits (limits.go): the link routes per client before the
	// signature is checked, link/start per client and in all, and the
	// device routes per key.
	preAuth, linkStarts *limiter[netip.Prefix]
	linkStartsTotal     *limiter[struct{}]
	perDevice           *limiter[string]

	denyCache struct {
		sync.Mutex
		seq int64
		at  time.Time
		tok string
	}
}

// New checks cfg and builds a server on opts.
func New(cfg Config, opts Options) (*Server, error) {
	if err := cfg.Validate(opts.Dev); err != nil {
		return nil, err
	}
	if opts.Store == nil || opts.Billing == nil || opts.DNS == nil || opts.Keys == nil {
		return nil, errors.New("server: a store, billing, DNS and keys are required")
	}
	if opts.DevBilling != nil && !opts.Dev {
		return nil, errors.New("server: the fake merchant of record is for --dev only")
	}
	if opts.DevStorage != nil && !opts.Dev {
		return nil, errors.New("server: the fake backup storage is for --dev only")
	}
	if _, err := dns.HandleCAA("", cfg.IODEF); err != nil {
		return nil, err
	}
	u, _ := url.Parse(cfg.PublicURL)
	trusted, _ := cfg.trustedProxies()
	preAuth := 0
	if cfg.LinkStartsPerMinute > 0 {
		preAuth = max(preAuthPerMinute, cfg.LinkStartsPerMinute)
	}
	s := &Server{
		cfg: cfg, store: opts.Store, billing: opts.Billing, dns: opts.DNS, keys: opts.Keys,
		storage: opts.Storage, devStorage: opts.DevStorage,
		dev: opts.Dev, devBilling: opts.DevBilling, now: opts.Now, log: opts.Log, ver: opts.Version,
		origin: u.Scheme + "://" + u.Host, iss: u.Hostname(), trusted: trusted,
		nonces:          httpsig.NewMemoryNonceCache(0, 0),
		otherNonces:     httpsig.NewMemoryNonceCache(otherNonces, otherNoncesPerKey),
		preAuth:         newLimiter[netip.Prefix](preAuth),
		linkStarts:      newLimiter[netip.Prefix](cfg.LinkStartsPerMinute),
		linkStartsTotal: newLimiter[struct{}](cfg.LinkStartsTotalPerMinute),
		perDevice:       newLimiter[string](cfg.DeviceRequestsPerMinute),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.ver == "" {
		s.ver = "dev"
	}
	s.relays, s.v4, s.v6 = cfg.relays()
	s.handler = s.routes()
	return s, nil
}

// Handler serves the API.
func (s *Server) Handler() http.Handler { return s.handler }

// Origin is the origin requests must be signed for.
func (s *Server) Origin() string { return s.origin }

// Run serves on cfg.Listen until ctx ends, sweeping every minute.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve is Run on a listener the caller made.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hs := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		// net/http logs client addresses in some errors; they are dropped.
		ErrorLog: log.New(addrScrubber{s.log}, "", 0),
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(sweepEvery)
		defer t.Stop()
		for {
			if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("sweep", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
	errc := make(chan error, 1)
	go func() {
		if s.cfg.CertFile != "" {
			errc <- hs.ServeTLS(ln, s.cfg.CertFile, s.cfg.KeyFile)
		} else {
			errc <- hs.Serve(ln)
		}
	}()
	s.log.Info("serving", "origin", s.origin, "dev", s.dev)
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
		sctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		err = hs.Shutdown(sctx)
		stop()
	}
	cancel() // stops the sweeper, also when serving failed
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// addrScrubber passes net/http's own error lines to the log with any
// address removed, so no client IP is ever written.
type addrScrubber struct{ log *slog.Logger }

var addrRE = regexp.MustCompile(`(\[[0-9a-fA-F:.%]+\]|[0-9]{1,3}(\.[0-9]{1,3}){3}|[0-9a-fA-F]{0,4}(:[0-9a-fA-F]{0,4}){2,7})(:[0-9]+)?`)

func (a addrScrubber) Write(p []byte) (int, error) {
	a.log.Warn("http", "msg", addrRE.ReplaceAllString(string(p), "<addr>"))
	return len(p), nil
}

// Sweep does the timed work: links run out, deletions fall due, the deny
// list's window moves, DNS writes that failed are tried again, and backup
// uploads never committed are removed. A
// deletion first stops the account's billing at the merchant of record, and
// waits for the next sweep if that fails, since the customer id is gone
// once the rows are.
func (s *Server) Sweep(ctx context.Context) error {
	now := s.now().UTC()
	var due []store.Account
	if err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		due, err = tx.AccountsDeletingBy(now)
		return err
	}); err != nil {
		return err
	}
	var errs []error
	var ready []store.Account
	for _, a := range due {
		if err := s.billing.CancelAll(ctx, a.BillingCustomer); err != nil {
			errs = append(errs, fmt.Errorf("stopping billing for deleted account %s: %w", a.ID, err))
			continue
		}
		if err := s.deleteBackups(ctx, a.ID); err != nil {
			errs = append(errs, fmt.Errorf("removing the backups of deleted account %s: %w", a.ID, err))
			continue
		}
		ready = append(ready, a)
	}
	err := s.store.Update(ctx, func(tx *store.Tx) error {
		if err := tx.ExpireLinks(now, now.Add(-linkForget)); err != nil {
			return err
		}
		if err := tx.ForgetBillingEvents(now.Add(-billingForget)); err != nil {
			return err
		}
		for _, a := range ready {
			if err := s.finishDeletion(tx, a, now); err != nil {
				return err
			}
		}
		return tx.MoveDenyEdge(now.Add(-denyKeep), now.Add(-denyForget))
	})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, a := range ready {
		s.log.Info("account deleted", "account", a.ID)
	}
	return errors.Join(append(errs, s.syncPendingDNS(ctx), s.sweepBackups(ctx, now))...)
}

// deleteBackups removes every object in an account's namespaces from
// storage; the rows go with the account's.
func (s *Server) deleteBackups(ctx context.Context, account string) error {
	if s.storage == nil {
		return nil
	}
	var nss []store.Namespace
	if err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		nss, err = tx.AccountNamespaces(account)
		return err
	}); err != nil {
		return err
	}
	for _, n := range nss {
		if err := s.storage.DeletePrefix(ctx, storage.Prefix(n.NS)); err != nil {
			return err
		}
	}
	return nil
}

// finishDeletion ends an account whose undo window has passed: its device
// keys and handle go on the deny list, its rows go, and its handle is
// released (kept, never reassigned) with its records queued for removal.
func (s *Server) finishDeletion(tx *store.Tx, a store.Account, now time.Time) error {
	devs, err := tx.Devices(a.ID)
	if err != nil {
		return err
	}
	for _, d := range devs {
		if _, err := tx.Deny(store.DenyEntry{Kind: store.DenyKey, Value: d.Pub, KeyID: d.KeyID, Why: "deleted", Created: now}); err != nil {
			return err
		}
	}
	if h, err := tx.AccountHandle(a.ID); err == nil {
		if _, err := tx.Deny(store.DenyEntry{Kind: store.DenyHandle, Value: h.Name, Why: "deleted", Created: now}); err != nil {
			return err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return tx.DeleteAccount(a.ID, now)
}

// syncPendingDNS writes the records of every handle marked pending.
func (s *Server) syncPendingDNS(ctx context.Context) error {
	var hs []store.Handle
	if err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		hs, err = tx.HandlesPendingDNS()
		return err
	}); err != nil {
		return err
	}
	var errs []error
	for _, h := range hs {
		errs = append(errs, s.syncDNS(ctx, h.Name))
	}
	return errors.Join(errs...)
}

// syncDNS makes a handle's records match the store: A, AAAA and CAA while
// an account holds it, nothing once released. It clears the pending mark
// only if nothing changed meanwhile.
func (s *Server) syncDNS(ctx context.Context, name string) error {
	var h store.Handle
	var acme string
	err := s.store.View(ctx, func(tx *store.Tx) error {
		var err error
		if h, err = tx.Handle(name); err != nil {
			return err
		}
		if h.Account != "" {
			a, err := tx.Account(h.Account)
			if err != nil {
				return err
			}
			acme = a.ACMEAccount
		}
		return nil
	})
	if err != nil {
		return err
	}
	if h.Account == "" {
		err = s.dns.DeleteHandle(ctx, name)
	} else {
		var caa []dns.CAA
		if caa, err = dns.HandleCAA(acme, s.cfg.IODEF); err == nil {
			err = s.dns.UpsertHandle(ctx, name, s.v4, s.v6, caa)
		}
	}
	if err != nil {
		return fmt.Errorf("dns for %s: %w", name, err)
	}
	return s.store.Update(ctx, func(tx *store.Tx) error {
		cur, err := tx.Handle(name)
		if err != nil {
			return err
		}
		if cur.Account != h.Account {
			return nil // changed while we wrote; the next sweep writes again
		}
		if a, err := tx.Account(cur.Account); err == nil && a.ACMEAccount != acme {
			return nil
		}
		cur.DNSPending = false
		return tx.UpdateHandle(cur)
	})
}

// publicKeys encodes a key set for /v1/keys.
func publicKeys(m map[string]ed25519.PublicKey) map[string]string {
	out := map[string]string{}
	for kid, k := range m {
		out[kid] = entitle.EncodeKey(k)
	}
	return out
}
