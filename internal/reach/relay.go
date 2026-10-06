package reach

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/relay"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
)

// Relay mode: reach through your own mirrin-relay (docs/cloud-design.md
// §6.1-§6.5). The relay routes by SNI and splices ciphertext; TLS ends here
// under a key only this machine holds, with a certificate this machine
// obtains itself by TLS-ALPN-01 through the same tunnel. certwatch watches
// CT and CAA for the name, and the alarm playbook (alarm.go) acts on what
// it finds.

// Deps are what the relay endpoint and the playbook need from the daemon.
type Deps struct {
	Server  *api.Server
	DataDir string
	Health  *health.Monitor
	Devices func() *devices.Store
	// Passkeys revokes passkeys enrolled in the alarm window. Nil is
	// StorePasskeys(Devices).
	Passkeys PasskeyRevoker
	// Alarm is the alarm state. StartRelay opens data/reach/alarm.json when
	// it is nil.
	Alarm *Alarm
	// Notify messages the owner in their own chat.
	Notify func(ctx context.Context, text string) error
	// Push sends a security notification to paired phones.
	Push func(id, summary string)
	// Banner shows a notice on the presence screen.
	Banner func(text string)
	// Audit records an event in the audit log.
	Audit func(kind, detail string)
	Log   *slog.Logger
	Now   func() time.Time

	// Tests (and unusual setups) replace the outside world here; nil means
	// Cert Spotter and crt.sh, CAA over DefaultDoH, Let's Encrypt with the
	// default client, and the system roots for the relay.
	CTSources  []certwatch.CTSource
	CAA        func(ctx context.Context, name string) ([]certwatch.CAA, error)
	ACMEClient *http.Client
	RelayRoots *x509.CertPool
	KeyRotate  time.Duration

	// Cloud reach only (StartCloud).
	//
	// Reach is the reach settings: acme_directory, acme_email,
	// admin_remote and stay_awake.
	Reach config.Reach
	// CloudCheck is how often the link's state is read (a file; nothing is
	// sent). Zero is a minute.
	CloudCheck time.Duration
	// PinSettle is the wait after telling the control plane a new ACME
	// account, while the handle's CAA record changes. Zero is 90 s; tests
	// set a nanosecond.
	PinSettle time.Duration
	// ConfirmEvery is the first wait between attempts to confirm a relay's
	// superseded with an unreachable control plane. Zero is 2 s.
	ConfirmEvery time.Duration
}

// Endpoint is a running relay reach, or (from LocalEndpoint) what the CLI
// knows about one.
type Endpoint struct {
	Hostname string
	URL      string // https://hostname
	RelayURL string
	ACME     *tlsmgr.ACME       // nil for a CLI view
	Listener *relay.Listener    // nil for a CLI view
	Watcher  *certwatch.Watcher // nil for a CLI view
	Alarm    *Alarm

	// Pins are the SPKI pins this machine will present (current, next).
	Pins func() []string
	// AccountURI is the ACME account CAA should pin.
	AccountURI func() string
	// Roots verifies the public chain. Nil is the system roots.
	Roots *x509.CertPool
	// Dial reaches host:443 as a phone would. Nil dials it.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	// CAA looks up CAA. Nil is certwatch.LookupCAA over DefaultDoH.
	CAA func(ctx context.Context, name string) ([]certwatch.CAA, error)

	done     chan struct{}
	stop     context.CancelFunc
	playbook func(certwatch.Finding)

	// Cloud reach only (cloud.go).
	cloud *cloudEndpoint
}

// Done is closed once the endpoint has stopped (its context ended).
func (e *Endpoint) Done() <-chan struct{} { return e.done }

// Online reports whether any relay tunnel is up.
func (e *Endpoint) Online() bool {
	if e.Listener == nil {
		return false
	}
	for _, s := range e.Listener.Status() {
		if s.Online {
			return true
		}
	}
	return false
}

// CheckRelayConfig validates the relay settings.
func CheckRelayConfig(c config.Reach) (host string, err error) {
	u, err := url.Parse(c.RelayURL)
	if err != nil || u.Scheme != "wss" || u.Host == "" {
		return "", fmt.Errorf("the relay address must look like wss://relay.example.com%s", wire.Path)
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(c.Hostname), "."))
	if host == "" || !strings.Contains(host, ".") || strings.ContainsAny(host, "*/: ") {
		return "", errors.New("choose the public name the relay sends here, such as twin.example.com")
	}
	return host, nil
}

// RelayKeyPaths are the device key the relay's allow list names, and the
// status key whose hash the relay keeps.
func RelayKeyPaths(dataDir string) (deviceKey, statusKey string) {
	d := filepath.Join(dataDir, "relay")
	return filepath.Join(d, "device.key"), filepath.Join(d, "status.key")
}

// RelayKeys loads, or creates, this machine's relay keys.
func RelayKeys(dataDir string) (ed25519.PrivateKey, []byte, error) {
	kp, _ := RelayKeyPaths(dataDir)
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		return nil, nil, err
	}
	var key ed25519.PrivateKey
	if b, err := os.ReadFile(kp); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, nil, fmt.Errorf("%s is damaged", kp)
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, nil, err
		}
		var ok bool
		if key, ok = k.(ed25519.PrivateKey); !ok {
			return nil, nil, fmt.Errorf("%s isn't an Ed25519 key", kp)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		_, key, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		if err := writeNew(kp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
			return nil, nil, err
		}
	} else {
		return nil, nil, err
	}
	status, err := StatusKey(dataDir)
	if err != nil {
		return nil, nil, err
	}
	return key, status, nil
}

// StatusKey loads, or creates, this twin's status key: the secret behind
// each relay's status endpoint (GET /v1/status/<name>?k=), which lets a
// paired phone ask whether the twin is online. The relay keeps only its
// SHA-256, from the hello. One key serves every relay and every mode.
func StatusKey(dataDir string) ([]byte, error) {
	_, sp := RelayKeyPaths(dataDir)
	if err := os.MkdirAll(filepath.Dir(sp), 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(sp)
	if err == nil {
		status, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(status) != wire.StatusKeySize {
			return nil, fmt.Errorf("%s is damaged", sp)
		}
		return status, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	status := make([]byte, wire.StatusKeySize)
	rand.Read(status)
	if err := writeNew(sp, []byte(base64.RawURLEncoding.EncodeToString(status)+"\n")); err != nil {
		return nil, err
	}
	return status, nil
}

func writeNew(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AllowLine is the relay.yaml allow entry for this machine.
func AllowLine(host string, key ed25519.PrivateKey) string {
	return fmt.Sprintf("  - {hostname: %s, key: %s}", host, base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
}

func relayRoots(c config.Reach, extra *x509.CertPool) (*x509.CertPool, error) {
	if extra != nil {
		return extra, nil
	}
	if c.RelayCAFile == "" {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	b, err := os.ReadFile(c.RelayCAFile)
	if err != nil {
		return nil, err
	}
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s holds no certificate", c.RelayCAFile)
	}
	return pool, nil
}

// StartRelay connects to the self-hosted relay, serves the remote API over
// it, keeps the certificate current, and watches CT and CAA. It returns once
// everything is started; ctx ends it.
func StartRelay(ctx context.Context, cfg config.Reach, d Deps) (*Endpoint, error) {
	host, err := CheckRelayConfig(cfg)
	if err != nil {
		return nil, err
	}
	if d.DataDir == "" {
		return nil, errors.New("reach: StartRelay needs the API server and the data directory")
	}
	key, status, err := RelayKeys(d.DataDir)
	if err != nil {
		return nil, fmt.Errorf("relay keys: %w", err)
	}
	roots, err := relayRoots(cfg, d.RelayRoots)
	if err != nil {
		return nil, err
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return startTunnels(ctx, tunnelSpec{
		via:      "relay",
		hosts:    []string{host},
		relays:   []relay.RelayRef{{URL: cfg.RelayURL}},
		relayURL: cfg.RelayURL,
		key:      key,
		status:   status,
		roots:    roots,
		cfg:      cfg,
		mount:    true,
		onRefused: func(e *Endpoint, r string, we wire.Error) {
			if we.Code != wire.CodeSuperseded {
				log.Warn("the relay refused this machine", "relay", r, "code", we.Code, "message", we.Message)
				return
			}
			now := time.Now()
			e.playbook(certwatch.Finding{Kind: certwatch.Superseded, Severity: certwatch.Critical, Host: host, At: now, NotBefore: now,
				Detail: "The relay says another computer with this machine's key took over " + host + " at " + now.UTC().Format("2 Jan 15:04 MST") + "."})
		},
		health: relayHealth,
		expiring: func(left time.Duration) string {
			return fmt.Sprintf("My HTTPS certificate for %s runs out in %s and hasn't renewed. Check that the relay still sends %s here (`mirrin reach verify`).", host, roughDays(left), host)
		},
	}, d)
}

// tunnelSpec is what differs between reach through your own relay and
// through the paid relays: the names, the relays, the keys, and what a
// refusal means.
type tunnelSpec struct {
	via      string   // "relay" or "cloud": the listener's name in logs and devices
	hosts    []string // the names the certificate carries; the first is the address
	relays   []relay.RelayRef
	relayURL string // for Endpoint.RelayURL
	key      ed25519.PrivateKey
	status   []byte
	roots    *x509.CertPool
	cfg      config.Reach // acme_directory, acme_email, admin_remote, stay_awake
	// entitlement is the token each hello carries (nil: none).
	entitlement func() string
	// maxBackoff caps the wait between tunnel attempts (0: a minute).
	maxBackoff time.Duration
	// mount adds the alarm to the API server here. The daemon mounts it
	// itself for an endpoint it may start more than once.
	mount bool
	// onRefused hears every refusal, before the tunnel reconnects.
	onRefused func(e *Endpoint, relay string, we wire.Error)
	// prepare runs once the first tunnel is up and before the first
	// issuance; issuance waits for it to succeed.
	prepare func(ctx context.Context, e *Endpoint) error
	// ready runs before every issuance, once a tunnel is up.
	ready func(ctx context.Context, e *Endpoint) error
	// health is the reach self-check.
	health func(e *Endpoint) (health.State, string, string)
	// expiring is what the owner is told when the certificate hasn't
	// renewed and is running out.
	expiring func(left time.Duration) string
}

// startTunnels runs a tunnelled endpoint: the relay tunnels, the remote API
// behind them, the certificate and the CT/CAA watch.
func startTunnels(ctx context.Context, spec tunnelSpec, d Deps) (*Endpoint, error) {
	if d.Server == nil || d.DataDir == "" {
		return nil, errors.New("reach: starting remote access needs the API server and the data directory")
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var err error
	if d.Alarm == nil {
		if d.Alarm, err = OpenAlarm(d.DataDir); err != nil {
			return nil, err
		}
	}
	host := spec.hosts[0]
	e := &Endpoint{Hostname: host, URL: "https://" + host, RelayURL: spec.relayURL, Alarm: d.Alarm, CAA: d.CAA, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(ctx)
	e.stop = cancel
	fail := func(err error) (*Endpoint, error) { cancel(); return nil, err }
	e.playbook = func(f certwatch.Finding) {
		go func() {
			if err := Playbook(context.WithoutCancel(ctx), f, d); err != nil {
				log.Warn("certificate alarm playbook", "err", err)
			}
		}()
	}

	var watcherMu sync.Mutex
	e.ACME, err = tlsmgr.NewACME(tlsmgr.ACMEConfig{
		Directory:  spec.cfg.ACMEDirectory,
		Hostnames:  spec.hosts,
		Dir:        filepath.Join(d.DataDir, "tls"),
		Email:      spec.cfg.ACMEEmail,
		HTTPClient: d.ACMEClient,
		KeyRotate:  d.KeyRotate,
		Now:        d.Now,
		Log:        log,
		Ready: func(ctx context.Context) error {
			if err := waitOnline(ctx, e); err != nil {
				return err
			}
			if spec.ready != nil {
				return spec.ready(ctx, e)
			}
			return nil
		},
		OnIssued: func(tlsmgr.Issued) {
			watcherMu.Lock()
			w := e.Watcher
			watcherMu.Unlock()
			if w != nil {
				w.Poke()
			}
		},
		OnExpiring: func(left time.Duration) {
			text := spec.expiring(left)
			if d.Push != nil {
				d.Push("cert-expiring", text)
			}
			if d.Notify != nil {
				_ = d.Notify(context.WithoutCancel(ctx), text)
			}
		},
	})
	if err != nil {
		return fail(err)
	}
	e.Pins, e.AccountURI = e.ACME.SPKIs, e.ACME.AccountURI

	e.Listener, err = relay.Listen(ctx, relay.ClientConfig{
		Relays:      spec.relays,
		Key:         spec.key,
		StatusKey:   spec.status,
		Entitlement: spec.entitlement,
		RootCAs:     spec.roots,
		MaxBackoff:  spec.maxBackoff,
		Log:         log,
		OnRefused: func(r string, we wire.Error) {
			if spec.onRefused != nil {
				spec.onRefused(e, r, we)
			}
		},
	})
	if err != nil {
		return fail(err)
	}

	srcs := d.CTSources
	if srcs == nil {
		srcs = []certwatch.CTSource{&certwatch.CertSpotter{}, &certwatch.CrtSh{NotBefore: func() time.Time { _, cp := e.ACME.Known(); return cp }}}
	}
	w := &certwatch.Watcher{
		Hosts:      func() []string { return slices.Clone(spec.hosts) },
		Known:      e.ACME.Known,
		Sources:    srcs,
		AccountURI: e.ACME.AccountURI,
		CAA:        d.CAA,
		Now:        d.Now,
		Log:        log,
		// A cleared alarm's certificate stays in CT for its life; CAA's
		// last good sighting survives a restart.
		Acknowledged: d.Alarm.Acknowledged,
		StateFile:    filepath.Join(d.DataDir, "reach", "certwatch.json"),
		OnFinding: func(f certwatch.Finding) {
			if f.Severity == certwatch.Critical {
				e.playbook(f)
				return
			}
			log.Warn("certificate watch", "kind", f.Kind, "detail", f.Detail)
		},
	}
	watcherMu.Lock()
	e.Watcher = w
	watcherMu.Unlock()

	if spec.mount {
		d.Server.WithAlarm(d.Alarm)
		mountAlarm(d.Server, d.Alarm)
	}
	if d.Health != nil {
		d.Health.Add(
			health.Func("reach", "Remote access", func(context.Context) (health.State, string, string) { return spec.health(e) }, nil),
			health.Func("certwatch", "Certificate watch", w.Health, nil),
			health.Func("alarm", "Certificate alarm", d.Alarm.Health, nil),
		)
	}

	var wg sync.WaitGroup
	run := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil && ctx.Err() == nil {
				log.Warn("remote access", "via", spec.via, "part", name, "err", err)
			}
		}()
	}
	run("https", func() error {
		return d.Server.ServeRemote(ctx, e.Listener, e.ACME, api.RemoteOptions{Via: spec.via, AllowAdmin: spec.cfg.AdminRemote})
	})
	run("certificate", func() error {
		if spec.prepare != nil {
			if err := prepareOnce(ctx, e, spec.prepare, log); err != nil {
				return err
			}
		}
		return e.ACME.Run(ctx)
	})
	run("certwatch", func() error {
		// The first poll waits for the first certificate or a minute, so a
		// fresh machine doesn't report a gap it is about to fill.
		deadline := time.NewTimer(time.Minute)
		defer deadline.Stop()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
	wait:
		for e.ACME.Leaf() == nil {
			select {
			case <-ctx.Done():
				return nil
			case <-deadline.C:
				break wait
			case <-tick.C:
			}
		}
		return w.Run(ctx)
	})
	if spec.cfg.StayAwake {
		stop := KeepAwake(ctx, true)
		context.AfterFunc(ctx, stop)
	}
	go func() {
		<-ctx.Done()
		e.Listener.Close()
		<-e.Listener.Done()
		wg.Wait()
		close(e.done)
	}()
	return e, nil
}

// prepareOnce retries prepare, once a tunnel is up, until it succeeds or
// ctx ends: seconds apart at first, then up to an hour.
func prepareOnce(ctx context.Context, e *Endpoint, prepare func(context.Context, *Endpoint) error, log *slog.Logger) error {
	delay := 5 * time.Second
	for {
		if err := waitOnline(ctx, e); err != nil {
			return nil
		}
		err := prepare(ctx, e)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		log.Warn("remote access: getting ready for the certificate", "err", err, "retry_in", delay)
		if !pause(ctx, delay) {
			return nil
		}
		delay = min(delay*4, time.Hour)
	}
}

func roughDays(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days >= 2 {
		return fmt.Sprintf("%d days", days)
	}
	return "under two days"
}

// waitOnline blocks until a relay tunnel is up.
func waitOnline(ctx context.Context, e *Endpoint) error {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for !e.Online() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

func relayHealth(e *Endpoint) (health.State, string, string) {
	var last string
	for _, s := range e.Listener.Status() {
		if s.Online {
			if err := tlsmgr.Warning(e.ACME); err != nil {
				return health.Warn, err.Error(), "Run `mirrin reach verify`; Mirrin keeps retrying"
			}
			if e.ACME.Leaf() == nil {
				return health.Warn, "connected to the relay; getting a certificate for " + e.Hostname, "Mirrin is asking the certificate authority now"
			}
			return health.OK, "reachable at " + e.URL + " through your relay", ""
		}
		switch {
		case s.Stopped:
			return health.Fail, "another computer took over " + e.Hostname + " at the relay", "Check which computer should answer for this name"
		case s.Refused != nil:
			last = s.Refused.Message
		case s.LastError != "":
			last = s.LastError
		}
	}
	if last == "" {
		last = "connecting"
	}
	return health.Warn, "relay not connected: " + last, "Check the relay address and that its allow list has this machine (`mirrin reach use relay` prints it)"
}

// MountAlarm adds the certificate alarm to s: approvals from other devices
// pause while it is active, and GET /reach/alarm and POST
// /reach/alarm/clear answer on this computer. The daemon mounts it once for
// cloud reach, whose endpoint may start more than once.
func MountAlarm(s *api.Server, a *Alarm) {
	s.WithAlarm(a)
	mountAlarm(s, a)
}

// mountAlarm adds the loopback-only alarm routes: GET /reach/alarm and
// POST /reach/alarm/clear.
func mountAlarm(s *api.Server, a *Alarm) {
	s.Mount("reach-alarm", api.LoopbackOnly, func(mux *http.ServeMux, z api.Authz) {
		mux.HandleFunc("GET /reach/alarm", z.Local(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(a.State())
		}))
		mux.HandleFunc("POST /reach/alarm/clear", z.Local(func(w http.ResponseWriter, r *http.Request) {
			if err := a.Clear("this computer"); err != nil {
				http.Error(w, "Couldn't save that. Try again.", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(a.State())
		}))
	})
}
