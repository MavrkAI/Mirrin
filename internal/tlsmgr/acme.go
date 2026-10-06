package tlsmgr

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

// The daemon's own ACME loop (docs/cloud-design.md §6.2). It is not
// autocert, which makes a new key on every renewal: here the ECDSA P-256
// key is reused, and a next key is generated ahead of time so pairing codes,
// /trust and `mirrin reach verify` can announce it before it is used.
// Challenges are answered by TLS-ALPN-01 on the listener that serves the
// hostname, which through a relay means a relay stream carrying ALPN
// acme-tls/1.

// LetsEncrypt is the default ACME directory.
const LetsEncrypt = "https://acme-v02.api.letsencrypt.org/directory"

const (
	// AlarmLeft is when an unrenewed certificate raises the alarm.
	AlarmLeft        = 10 * 24 * time.Hour
	defaultKeyRotate = 365 * 24 * time.Hour
	ariRecheck       = 6 * time.Hour
	retryMin         = time.Minute
	retryMax         = time.Hour
	keepIssued       = 50
)

// Files in ACMEConfig.Dir (data/tls). All of them belong in backups.
const (
	accountKeyFile = "acme-account.key"
	currentKeyFile = "key-current.pem"
	nextKeyFile    = "key-next.pem"
	certFile       = "cert.pem"
	issuedFile     = "issued.json"
)

// ACMEConfig configures NewACME.
type ACMEConfig struct {
	// Directory is the ACME directory URL. Empty is Let's Encrypt.
	Directory string
	// Hostnames go on every certificate. The first is the primary name.
	Hostnames []string
	// Dir holds the keys, the certificate and issued.json (data/tls).
	Dir string
	// KeyRotate is how long a key stays current before the next key takes
	// over, at the renewal that follows. Zero is a year.
	KeyRotate time.Duration
	// Email is an optional account contact.
	Email string
	// HTTPClient talks to the CA. Nil is http.DefaultClient.
	HTTPClient *http.Client
	// Ready, if set, blocks until challenges can be answered (the relay
	// tunnel is up). A failed validation counts against the CA's limits,
	// so issuance never starts before this returns nil.
	Ready func(context.Context) error
	// OnIssued is called after every issuance: certwatch polls CT then.
	OnIssued func(Issued)
	// OnExpiring is called, at most daily, while the certificate has less
	// than AlarmLeft to run.
	OnExpiring func(left time.Duration)
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
	// Log receives renewal events. Nil discards them.
	Log *slog.Logger
}

// Issued records one certificate this machine obtained.
type Issued struct {
	Serial    string    `json:"serial"` // hex
	SPKI      string    `json:"spki"`   // SPKIPin form: b64url SHA-256 of the SubjectPublicKeyInfo
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	Hostnames []string  `json:"hostnames"`
	At        time.Time `json:"at"`
}

// acmeState is issued.json.
type acmeState struct {
	Version    int    `json:"version"`
	Directory  string `json:"directory"`
	AccountURI string `json:"account_uri,omitempty"`
	// Checkpoint is when this machine began answering for its names. CT
	// entries from before it are someone else's history, not an alarm.
	// A restore keeps it.
	Checkpoint time.Time `json:"checkpoint"`
	// KeySince is when the current key became current.
	KeySince time.Time `json:"key_since"`
	Issued   []Issued  `json:"issued"`
	// History is every SPKI this machine has used or announced, current
	// and next included, so a restore does not alarm on its own past.
	History []string `json:"history_spki,omitempty"`
}

// ACME obtains and renews the certificate for Hostnames. It is a Source.
type ACME struct {
	cfg ACMEConfig
	log *slog.Logger

	mu         sync.RWMutex
	account    crypto.Signer
	current    crypto.Signer
	next       crypto.Signer
	pair       *tls.Certificate
	state      acmeState
	challenges map[string]*tls.Certificate
	warning    error
	ari        ariCache
	lastAlarm  time.Time
	failures   int

	issueMu sync.Mutex // one issuance at a time
	client  *acme.Client
	rand    func() float64
	wait    func(context.Context, time.Duration) bool
}

type ariCache struct {
	certSerial string
	at         time.Time // when to act: a time in the suggested window
	recheck    time.Time // when to ask again
}

// NewACME loads, or creates, the account key, the current and next keys,
// the certificate and issued.json in cfg.Dir. It makes no network calls.
func NewACME(cfg ACMEConfig) (*ACME, error) {
	if cfg.Dir == "" {
		return nil, errors.New("tlsmgr: ACME needs a directory for its keys")
	}
	hosts := make([]string, 0, len(cfg.Hostnames))
	for _, h := range cfg.Hostnames {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h == "" || strings.ContainsAny(h, "*/: ") || !strings.Contains(h, ".") && h != "localhost" {
			return nil, fmt.Errorf("%q isn't a hostname a certificate can name", h)
		}
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return nil, errors.New("tlsmgr: ACME needs a hostname")
	}
	cfg.Hostnames = hosts
	if cfg.Directory == "" {
		cfg.Directory = LetsEncrypt
	}
	if cfg.KeyRotate <= 0 {
		cfg.KeyRotate = defaultKeyRotate
	}
	a := &ACME{cfg: cfg, log: cfg.Log, challenges: map[string]*tls.Certificate{}, rand: mathrand.Float64, wait: wait}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	var err error
	if a.account, err = loadOrCreateKey(filepath.Join(cfg.Dir, accountKeyFile)); err != nil {
		return nil, fmt.Errorf("ACME account key: %w", err)
	}
	if a.current, err = loadOrCreateKey(filepath.Join(cfg.Dir, currentKeyFile)); err != nil {
		return nil, fmt.Errorf("HTTPS key: %w", err)
	}
	if a.next, err = loadOrCreateKey(filepath.Join(cfg.Dir, nextKeyFile)); err != nil {
		return nil, fmt.Errorf("next HTTPS key: %w", err)
	}
	if keyPin(a.next) == keyPin(a.current) {
		if a.next, err = replaceKey(filepath.Join(cfg.Dir, nextKeyFile)); err != nil {
			return nil, err
		}
	}
	now := a.now()
	st, err := readACMEState(cfg.Dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		st = acmeState{Version: 1, Checkpoint: now, KeySince: now}
	case err != nil:
		return nil, err
	}
	// Written back only if opening changed it: `mirrin reach use relay`
	// opens the same files while a daemon may be running on them.
	before, _ := json.Marshal(st)
	if err != nil {
		before = nil
	}
	if st.Directory != cfg.Directory {
		st.Directory, st.AccountURI = cfg.Directory, "" // an account belongs to one CA
	}
	if st.KeySince.IsZero() {
		st.KeySince = now
	}
	if st.Checkpoint.IsZero() {
		st.Checkpoint = now
	}
	a.state = st
	if err := a.loadCert(); err != nil {
		a.log.Warn("the saved HTTPS certificate can't be used; a new one will be requested", "err", err)
	}
	a.remember(keyPin(a.current), keyPin(a.next))
	if after, _ := json.Marshal(a.state); !bytes.Equal(before, after) {
		if err := a.saveState(); err != nil {
			return nil, err
		}
	}
	a.client = &acme.Client{Key: a.account, DirectoryURL: cfg.Directory, HTTPClient: cfg.HTTPClient, UserAgent: "mirrin"}
	if st.AccountURI != "" {
		a.client.KID = acme.KeyID(st.AccountURI)
	}
	return a, nil
}

// loadCert reads cert.pem. A certificate for the next key means a rotation
// was interrupted after the certificate was written: it is finished here.
func (a *ACME) loadCert() error {
	b, err := os.ReadFile(filepath.Join(a.cfg.Dir, certFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var chain [][]byte
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type == "CERTIFICATE" {
			chain = append(chain, blk.Bytes)
		}
	}
	if len(chain) == 0 {
		return errors.New("cert.pem holds no certificate")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return err
	}
	switch spkiPin(leaf) {
	case keyPin(a.current):
	case keyPin(a.next):
		if err := a.promote(); err != nil {
			return err
		}
	default:
		return errors.New("cert.pem is for a key this machine doesn't hold")
	}
	a.pair = &tls.Certificate{Certificate: chain, PrivateKey: a.current, Leaf: leaf}
	return nil
}

// promote makes the next key current and generates a new next key. Callers
// hold mu or own a before it is shared.
func (a *ACME) promote() error {
	if err := writeKey(filepath.Join(a.cfg.Dir, currentKeyFile), a.next); err != nil {
		return err
	}
	next, err := replaceKey(filepath.Join(a.cfg.Dir, nextKeyFile))
	if err != nil {
		return err
	}
	a.current, a.next = a.next, next
	a.state.KeySince = a.now()
	a.remember(keyPin(a.next))
	return nil
}

func (a *ACME) remember(pins ...string) {
	for _, p := range pins {
		if !slices.Contains(a.state.History, p) {
			a.state.History = append(a.state.History, p)
		}
	}
}

func (a *ACME) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}
	return time.Now()
}

// AccountURI is the ACME account's URL, which CAA records pin with
// accounturi. It is empty until the account is registered.
func (a *ACME) AccountURI() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state.AccountURI
}

// Directory is the CA's directory URL.
func (a *ACME) Directory() string { return a.cfg.Directory }

// Hostnames are the names the certificate covers.
func (a *ACME) Hostnames() []string { return slices.Clone(a.cfg.Hostnames) }

// SPKIs are the pins of the current and the next key.
func (a *ACME) SPKIs() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return []string{keyPin(a.current), keyPin(a.next)}
}

// Known is every SPKI this machine has used or announced, and the
// checkpoint before which CT entries are not ours to judge.
func (a *ACME) Known() ([]string, time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return slices.Clone(a.state.History), a.state.Checkpoint
}

// Issued lists the certificates this machine obtained, oldest first.
func (a *ACME) Issued() []Issued {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return slices.Clone(a.state.Issued)
}

// Leaf is the certificate being served, or nil before the first issuance.
func (a *ACME) Leaf() *x509.Certificate {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.pair == nil {
		return nil
	}
	return a.pair.Leaf
}

// GetCertificate serves the current certificate.
func (a *ACME) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.pair == nil {
		return nil, errors.New("no HTTPS certificate yet; Mirrin is requesting one")
	}
	if !a.now().Before(a.pair.Leaf.NotAfter) {
		return nil, errors.New("the HTTPS certificate has expired; Mirrin keeps trying to renew it")
	}
	return a.pair, nil
}

// ChallengeConfig answers TLS-ALPN-01 (RFC 8737): a hello offering
// acme-tls/1 gets the pending challenge certificate for its name, and
// nothing else. Every other hello gets the listener's usual config.
func (a *ACME) ChallengeConfig(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	if !slices.Contains(hello.SupportedProtos, acme.ALPNProto) {
		return nil, nil
	}
	a.mu.RLock()
	cert := a.challenges[strings.ToLower(hello.ServerName)]
	a.mu.RUnlock()
	if cert == nil {
		return nil, errors.New("no ACME challenge is pending for that name")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert}, NextProtos: []string{acme.ALPNProto}}, nil
}

// Warning is set while renewal is failing or the certificate is close to
// expiry.
func (a *ACME) Warning() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.pair != nil {
		if left := a.pair.Leaf.NotAfter.Sub(a.now()); left < AlarmLeft {
			return fmt.Errorf("the HTTPS certificate expires in %s and hasn't renewed", roughly(left))
		}
	}
	return a.warning
}

// Expiring reports whether the certificate has less than AlarmLeft to run.
func (a *ACME) Expiring() (time.Duration, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.pair == nil {
		return 0, false
	}
	left := a.pair.Leaf.NotAfter.Sub(a.now())
	return left, left < AlarmLeft
}

func roughly(d time.Duration) string {
	switch days := int(d.Hours() / 24); {
	case d <= 0:
		return "no time"
	case days >= 2:
		return strconv.Itoa(days) + " days"
	case d >= time.Hour:
		return strconv.Itoa(int(d.Hours())) + " hours"
	default:
		return "under an hour"
	}
}

func (a *ACME) acmeClient() *acme.Client { return a.client }

// Register creates the ACME account, or finds the one this key already has,
// and records its URL. `mirrin reach use relay` calls it to print the CAA
// record before the first certificate.
func (a *ACME) Register(ctx context.Context) (string, error) {
	a.issueMu.Lock()
	defer a.issueMu.Unlock()
	return a.register(ctx)
}

func (a *ACME) register(ctx context.Context) (string, error) {
	if uri := a.AccountURI(); uri != "" {
		return uri, nil
	}
	c := a.acmeClient()
	acct := &acme.Account{}
	if a.cfg.Email != "" {
		acct.Contact = []string{"mailto:" + a.cfg.Email}
	}
	got, err := c.Register(ctx, acct, acme.AcceptTOS)
	if errors.Is(err, acme.ErrAccountAlreadyExists) {
		got, err = c.GetReg(ctx, "")
	}
	if err != nil {
		return "", fmt.Errorf("couldn't open an account with the certificate authority: %w", err)
	}
	if got.URI == "" {
		return "", errors.New("the certificate authority didn't say which account it opened")
	}
	a.mu.Lock()
	a.state.AccountURI = got.URI
	err = a.saveState()
	a.mu.Unlock()
	return got.URI, err
}

// Run keeps the certificate current until ctx ends: it issues when there is
// none, renews at a third of the lifetime left or inside the CA's ARI
// window, and rotates to the next key once KeyRotate has passed.
func (a *ACME) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		delay, err := a.Step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			a.log.Warn("HTTPS certificate: will retry", "err", err, "in", delay)
		}
		a.checkExpiry()
		if !a.wait(ctx, delay) {
			return nil
		}
	}
	return nil
}

// Step issues or renews if that is due now, and says how long to wait before
// the next step. Run calls it in a loop.
func (a *ACME) Step(ctx context.Context) (time.Duration, error) {
	due, rotate := a.due(ctx)
	now := a.now()
	if due.After(now) {
		return min(due.Sub(now), ariRecheck), nil
	}
	err := a.issue(ctx, rotate)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.failures++
		a.warning = errors.New("couldn't renew the HTTPS certificate yet; retrying")
		delay := retryMin << min(a.failures-1, 6)
		return min(delay, retryMax), err
	}
	a.failures, a.warning = 0, nil
	return 0, nil
}

// Renew issues a new certificate for the current key now.
func (a *ACME) Renew(ctx context.Context) error { return a.issue(ctx, false) }

// Rotate issues a certificate for the next key now, which then becomes
// current, and announces a new next key.
func (a *ACME) Rotate(ctx context.Context) error { return a.issue(ctx, true) }

// due is when the next issuance should happen, and whether it rotates keys.
func (a *ACME) due(ctx context.Context) (time.Time, bool) {
	a.mu.RLock()
	pair := a.pair
	rotateAt := a.state.KeySince.Add(a.cfg.KeyRotate)
	a.mu.RUnlock()
	now := a.now()
	if !now.Before(rotateAt) {
		return now, true
	}
	if pair == nil || !covers(pair.Leaf, a.cfg.Hostnames) {
		return now, false
	}
	leaf := pair.Leaf
	at := leaf.NotAfter.Add(-leaf.NotAfter.Sub(leaf.NotBefore) / 3)
	if ari, ok := a.renewalWindow(ctx, leaf); ok {
		at = ari
	}
	if rotateAt.Before(at) {
		return rotateAt, true
	}
	return at, false
}

func covers(leaf *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if !slices.Contains(leaf.DNSNames, h) {
			return false
		}
	}
	return true
}

// renewalWindow asks the CA's renewalInfo endpoint (RFC 9773), when the
// directory has one, and picks a time uniformly inside the window it
// suggests. The answer is kept until its Retry-After.
func (a *ACME) renewalWindow(ctx context.Context, leaf *x509.Certificate) (time.Time, bool) {
	serial := leaf.SerialNumber.Text(16)
	now := a.now()
	a.mu.RLock()
	cached := a.ari
	a.mu.RUnlock()
	if cached.certSerial == serial && now.Before(cached.recheck) {
		return cached.at, !cached.at.IsZero()
	}
	at, recheck, err := a.fetchRenewalInfo(ctx, leaf)
	if err != nil {
		a.log.Debug("ACME renewal info", "err", err)
		recheck = now.Add(ariRecheck)
	}
	a.mu.Lock()
	a.ari = ariCache{certSerial: serial, at: at, recheck: recheck}
	a.mu.Unlock()
	return at, !at.IsZero()
}

func (a *ACME) httpClient() *http.Client {
	if a.cfg.HTTPClient != nil {
		return a.cfg.HTTPClient
	}
	return http.DefaultClient
}

func (a *ACME) fetchRenewalInfo(ctx context.Context, leaf *x509.Certificate) (time.Time, time.Time, error) {
	var dir struct {
		RenewalInfo string `json:"renewalInfo"`
	}
	if err := a.getJSON(ctx, a.cfg.Directory, &dir, nil); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if dir.RenewalInfo == "" {
		return time.Time{}, a.now().Add(24 * time.Hour), nil // this CA doesn't offer ARI
	}
	id, err := ARICertID(leaf)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	var info struct {
		SuggestedWindow struct {
			Start time.Time `json:"start"`
			End   time.Time `json:"end"`
		} `json:"suggestedWindow"`
	}
	var h http.Header
	if err := a.getJSON(ctx, strings.TrimRight(dir.RenewalInfo, "/")+"/"+id, &info, &h); err != nil {
		return time.Time{}, time.Time{}, err
	}
	w := info.SuggestedWindow
	if w.Start.IsZero() || w.End.Before(w.Start) {
		return time.Time{}, time.Time{}, errors.New("renewal info has no usable window")
	}
	at := w.Start.Add(time.Duration(a.rand() * float64(w.End.Sub(w.Start))))
	recheck := a.now().Add(ariRecheck)
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
		recheck = a.now().Add(min(max(time.Duration(s)*time.Second, time.Minute), 24*time.Hour))
	}
	return at, recheck, nil
}

func (a *ACME) getJSON(ctx context.Context, url string, v any, h *http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := a.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, res.Status)
	}
	if h != nil {
		*h = res.Header
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(v)
}

// ARICertID is a certificate's ARI identifier: the authority key id and the
// serial number's DER content bytes, each unpadded base64url.
func ARICertID(leaf *x509.Certificate) (string, error) {
	if len(leaf.AuthorityKeyId) == 0 {
		return "", errors.New("the certificate has no authority key id")
	}
	serial := leaf.SerialNumber.Bytes()
	if len(serial) == 0 || serial[0]&0x80 != 0 {
		serial = append([]byte{0}, serial...)
	}
	return base64.RawURLEncoding.EncodeToString(leaf.AuthorityKeyId) + "." + base64.RawURLEncoding.EncodeToString(serial), nil
}

// issue runs one order: authorize every name by TLS-ALPN-01, finalize with
// a CSR for the current key (or the next key, when rotating), and install
// the result.
func (a *ACME) issue(ctx context.Context, rotate bool) error {
	a.issueMu.Lock()
	defer a.issueMu.Unlock()
	if a.cfg.Ready != nil {
		if err := a.cfg.Ready(ctx); err != nil {
			return err
		}
	}
	if _, err := a.register(ctx); err != nil {
		return err
	}
	a.mu.RLock()
	key := a.current
	if rotate {
		key = a.next
	}
	a.mu.RUnlock()
	c := a.acmeClient()
	order, err := c.AuthorizeOrder(ctx, acme.DomainIDs(a.cfg.Hostnames...))
	var problem *acme.Error
	if errors.As(err, &problem) && problem.ProblemType == "urn:ietf:params:acme:error:accountDoesNotExist" {
		a.mu.Lock()
		a.state.AccountURI = ""
		a.mu.Unlock()
		c.KID = ""
		if _, err = a.register(ctx); err != nil {
			return err
		}
		order, err = c.AuthorizeOrder(ctx, acme.DomainIDs(a.cfg.Hostnames...))
	}
	if err != nil {
		return fmt.Errorf("order a certificate: %w", err)
	}
	for _, u := range order.AuthzURLs {
		if err := a.authorize(ctx, c, u); err != nil {
			return err
		}
	}
	// The order's URL comes only with newOrder: RFC 8555 doesn't promise a
	// Location on a poll or on finalize, and Pebble sends none.
	orderURI := order.URI
	if order, err = c.WaitOrder(ctx, orderURI); err != nil {
		return fmt.Errorf("certificate order: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: a.cfg.Hostnames[0]}, DNSNames: a.cfg.Hostnames}, key)
	if err != nil {
		return err
	}
	chain, err := finalize(ctx, c, orderURI, order.FinalizeURL, csr)
	if err != nil {
		return fmt.Errorf("finalize the certificate: %w", err)
	}
	return a.install(chain, key, rotate)
}

// finalizeWait bounds the wait for an asynchronous finalization.
const finalizeWait = 5 * time.Minute

// finalize sends the CSR and returns the issued chain. When the CA answers
// "processing" without a Location, x/crypto's CreateOrderCert polls an
// empty URL and fails; the order is then polled by the URL newOrder gave
// until it is valid, and its certificate fetched from there.
func finalize(ctx context.Context, c *acme.Client, orderURI, finalizeURL string, csr []byte) ([][]byte, error) {
	chain, _, err := c.CreateOrderCert(ctx, finalizeURL, csr, true)
	if err == nil {
		return chain, nil
	}
	wctx, cancel := context.WithTimeout(ctx, finalizeWait)
	defer cancel()
	o, werr := c.WaitOrder(wctx, orderURI)
	if werr != nil || o.Status != acme.StatusValid || o.CertURL == "" {
		// Still "ready" means the CA refused the CSR: its error says why.
		return nil, err
	}
	return c.FetchCert(ctx, o.CertURL, true)
}

func (a *ACME) authorize(ctx context.Context, c *acme.Client, url string) error {
	z, err := c.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("authorization: %w", err)
	}
	if z.Status == acme.StatusValid {
		return nil
	}
	var chal *acme.Challenge
	for _, ch := range z.Challenges {
		if ch.Type == "tls-alpn-01" {
			chal = ch
		}
	}
	if chal == nil {
		return fmt.Errorf("the certificate authority didn't offer TLS-ALPN-01 for %s", z.Identifier.Value)
	}
	cert, err := c.TLSALPN01ChallengeCert(chal.Token, z.Identifier.Value)
	if err != nil {
		return err
	}
	name := strings.ToLower(z.Identifier.Value)
	a.mu.Lock()
	a.challenges[name] = &cert
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.challenges, name)
		a.mu.Unlock()
	}()
	if _, err := c.Accept(ctx, chal); err != nil {
		return fmt.Errorf("start the TLS-ALPN-01 check for %s: %w", name, err)
	}
	if _, err := c.WaitAuthorization(ctx, z.URI); err != nil {
		return fmt.Errorf("the certificate authority couldn't reach this machine as %s: %w", name, err)
	}
	return nil
}

// install checks the chain the CA returned and makes it current.
func (a *ACME) install(chain [][]byte, key crypto.Signer, rotate bool) error {
	if len(chain) == 0 {
		return errors.New("the certificate authority returned no certificate")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return err
	}
	if spkiPin(leaf) != keyPin(key) {
		return errors.New("the certificate authority returned a certificate for another key")
	}
	if !covers(leaf, a.cfg.Hostnames) {
		return errors.New("the certificate authority left a name off the certificate")
	}
	var pemChain []byte
	for _, der := range chain {
		pemChain = append(pemChain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// The certificate is written first: a crash before the keys move leaves
	// a certificate for the next key, which loadCert finishes promoting.
	if err := writeFileAtomic(filepath.Join(a.cfg.Dir, certFile), pemChain); err != nil {
		return err
	}
	if rotate {
		if err := a.promote(); err != nil {
			return err
		}
	}
	a.pair = &tls.Certificate{Certificate: chain, PrivateKey: a.current, Leaf: leaf}
	rec := Issued{Serial: hex.EncodeToString(leaf.SerialNumber.Bytes()), SPKI: spkiPin(leaf), NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter, Hostnames: slices.Clone(leaf.DNSNames), At: a.now()}
	a.state.Issued = append(a.state.Issued, rec)
	if n := len(a.state.Issued); n > keepIssued {
		a.state.Issued = slices.Clone(a.state.Issued[n-keepIssued:])
	}
	a.remember(rec.SPKI)
	a.ari = ariCache{}
	if err := a.saveState(); err != nil {
		return err
	}
	a.log.Info("HTTPS certificate issued", "names", leaf.DNSNames, "until", leaf.NotAfter, "rotated", rotate)
	if f := a.cfg.OnIssued; f != nil {
		go f(rec)
	}
	return nil
}

func (a *ACME) checkExpiry() {
	left, expiring := a.Expiring()
	if !expiring || a.cfg.OnExpiring == nil {
		return
	}
	a.mu.Lock()
	fire := a.lastAlarm.IsZero() || a.now().Sub(a.lastAlarm) >= 24*time.Hour
	if fire {
		a.lastAlarm = a.now()
	}
	a.mu.Unlock()
	if fire {
		a.cfg.OnExpiring(left)
	}
}

// saveState writes issued.json. Callers hold mu or own a.
func (a *ACME) saveState() error {
	a.state.Version = 1
	b, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.cfg.Dir, issuedFile), append(b, '\n'))
}

func readACMEState(dir string) (acmeState, error) {
	var st acmeState
	b, err := os.ReadFile(filepath.Join(dir, issuedFile))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s is damaged: %w", filepath.Join(dir, issuedFile), err)
	}
	return st, nil
}

// ACMEState is what `mirrin reach fingerprint` and `verify` read from disk
// while the daemon runs: nothing is created or changed.
type ACMEState struct {
	Directory  string
	AccountURI string
	Current    string // SPKI pin of the current key
	Next       string // SPKI pin of the next key
	History    []string
	Checkpoint time.Time
	Issued     []Issued
	Leaf       *x509.Certificate // nil before the first certificate
}

// ReadACMEState reads data/tls without changing it.
func ReadACMEState(dir string) (*ACMEState, error) {
	st, err := readACMEState(dir)
	if err != nil {
		return nil, err
	}
	out := &ACMEState{Directory: st.Directory, AccountURI: st.AccountURI, History: st.History, Checkpoint: st.Checkpoint, Issued: st.Issued}
	for _, f := range []struct {
		name string
		pin  *string
	}{{currentKeyFile, &out.Current}, {nextKeyFile, &out.Next}} {
		k, err := readKey(filepath.Join(dir, f.name))
		if err != nil {
			return nil, err
		}
		*f.pin = keyPin(k)
	}
	if b, err := os.ReadFile(filepath.Join(dir, certFile)); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			out.Leaf, _ = x509.ParseCertificate(blk.Bytes)
		}
	}
	return out, nil
}

// SPKIPin is the pin form used everywhere: unpadded base64url SHA-256 of
// the certificate's SubjectPublicKeyInfo.
func SPKIPin(c *x509.Certificate) string { return spkiPin(c) }

func spkiPin(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func readKey(path string) (crypto.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s isn't a PEM key", filepath.Base(path))
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%s holds an unsupported key", filepath.Base(path))
	}
	return s, nil
}

func loadOrCreateKey(path string) (crypto.Signer, error) {
	k, err := readKey(path)
	if errors.Is(err, os.ErrNotExist) {
		return replaceKey(path)
	}
	return k, err
}

func replaceKey(path string) (crypto.Signer, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return k, writeKey(path, k)
}

func writeKey(path string, k crypto.Signer) error {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// writeFileAtomic writes 0600 via a temp file and a rename, so a crash never
// leaves half a key.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mirrin-tls-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		f.Close()
		return err
	}
	_, err = f.Write(b)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
