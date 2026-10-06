package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// Config is canary.yaml. One file serves every role; each role reads only
// its own section (and the shared keys at the top). Secrets are never in
// it: each *_env names the environment variable that holds one.
type Config struct {
	// API is the control plane's origin, such as https://cloud.mirrin.app.
	API string `yaml:"api"`
	// DataDir holds the canary twin's device key, entitlement, ACME
	// account and certificate (twin and link only).
	DataDir string `yaml:"data_dir"`
	// TenantZone holds the handles. Empty is the zone daemons use.
	TenantZone string `yaml:"tenant_zone"`
	// EntitlementKeys and DenyListKeys are {kid: unpadded base64url
	// Ed25519 public key}. Empty means the sets compiled into this build
	// (internal/entitle/keys.go), which are empty until launch.
	EntitlementKeys map[string]string `yaml:"entitlement_keys"`
	DenyListKeys    map[string]string `yaml:"denylist_keys"`

	Twin   TwinConfig   `yaml:"twin"`
	Probe  ProbeConfig  `yaml:"probe"`
	Watch  WatchConfig  `yaml:"watch"`
	Mirror MirrorConfig `yaml:"mirror"`
}

// TwinConfig is the canary twin: a tunnel client with its own Let's
// Encrypt certificate that answers /healthz through both relays.
type TwinConfig struct {
	// Handle is asked for at link. The twin itself serves whatever handle
	// its entitlement names.
	Handle        string `yaml:"handle"`
	ACMEDirectory string `yaml:"acme_directory"` // empty is Let's Encrypt
	ACMEEmail     string `yaml:"acme_email"`
	// StatusListen serves /healthz (200 while every tunnel is up and the
	// certificate is in hand) and /v1/status, for the watcher and for
	// reading over SSH. Keep it on loopback.
	StatusListen string `yaml:"status_listen"`
	// RelayRoots is a PEM file of CAs for the relays' control names, for a
	// staging rig. Empty is the system roots.
	RelayRoots string `yaml:"relay_roots"`
	// PinSettle is the wait after the control plane is told a new ACME
	// account, while the handle's CAA record changes. Zero is 90 s.
	PinSettle time.Duration `yaml:"pin_settle"`
}

// ProbeConfig is one region's checker: HTTPS to the canary through each
// relay's addresses, every minute.
type ProbeConfig struct {
	Region string `yaml:"region"` // a short name, such as fra
	// Host is the canary twin's public name: <handle>.<tenant zone>.
	Host    string        `yaml:"host"`
	Relays  []ProbeRelay  `yaml:"relays"`
	Every   time.Duration `yaml:"every"`   // zero is a minute
	Timeout time.Duration `yaml:"timeout"` // per address; zero is 10 s
	// Listen serves the recent rounds at /v1/results for the watcher.
	Listen string `yaml:"listen"`
	// TokenEnv names the variable holding the bearer token the watcher
	// sends. Empty serves results to anyone who can reach Listen.
	TokenEnv string `yaml:"token_env"`
	// Roots is a PEM file of CAs to trust for the canary's certificate,
	// for staging (Pebble). Empty is the system roots.
	Roots string `yaml:"roots"`
}

// ProbeRelay is one relay and the addresses the probe dials directly, so
// each relay is checked on its own whatever DNS answers.
type ProbeRelay struct {
	ID    string   `yaml:"id"`
	Addrs []string `yaml:"addrs"` // host:port, IPv4 and IPv6
}

// WatchConfig is the one process that can page.
type WatchConfig struct {
	// Listen serves /healthz (for the external dead-man check) and
	// /v1/status. Keep it on loopback or behind the operator's proxy.
	Listen string        `yaml:"listen"`
	Every  time.Duration `yaml:"every"` // zero is a minute
	// Relays are the relay ids the canary must be reachable through.
	// Empty is the ids under probe.relays.
	Relays []string `yaml:"relays"`
	// Regions are the probes to read, one per region.
	Regions  []Region `yaml:"regions"`
	TokenEnv string   `yaml:"token_env"`

	// The paging rule: every relay failing, from at least PageRegions
	// regions, for PageAfter consecutive rounds. ClearAfter good rounds
	// resolve it.
	PageRegions int `yaml:"page_regions"` // zero is 2
	PageAfter   int `yaml:"page_after"`   // zero is 3
	ClearAfter  int `yaml:"clear_after"`  // zero is 3

	// Email-only alarms.
	RelayAfter  int           `yaml:"relay_after"`  // one relay failing from PageRegions regions; zero is 3
	SilentAfter int           `yaml:"silent_after"` // a probe with no fresh round; zero is 10
	CertWarn    time.Duration `yaml:"cert_warn"`    // the canary's certificate has less than this; zero is 20 days
	Checks      []Check       `yaml:"checks"`

	Page  PageConfig  `yaml:"page"`
	Email EmailConfig `yaml:"email"`
}

// Region is one probe the watcher reads.
type Region struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"` // the probe's /v1/results
}

// Check is an email-only check the watcher runs itself each round.
type Check struct {
	Name string `yaml:"name"`
	// Kind is http (any 2xx answer is good) or denylist (the answer is a
	// deny list signed by a denylist_keys key, dated within MaxAge).
	Kind   string        `yaml:"kind"`
	URL    string        `yaml:"url"`
	MaxAge time.Duration `yaml:"max_age"` // denylist; zero is 15 minutes
	After  int           `yaml:"after"`   // failing rounds before the email; zero is 5
}

// PageConfig is where the one paging alert goes.
type PageConfig struct {
	// Format is pagerduty (Events API v2) or webhook (a JSON POST of
	// {event, key, summary}).
	Format string `yaml:"format"`
	// URL is the endpoint. Empty with pagerduty is PagerDuty's.
	URL string `yaml:"url"`
	// URLEnv names a variable holding the URL instead, when the URL is
	// itself a secret (ntfy topic, chat webhook).
	URLEnv string `yaml:"url_env"`
	// RoutingKeyEnv names the variable holding PagerDuty's routing key.
	RoutingKeyEnv string `yaml:"routing_key_env"`
}

// EmailConfig is where everything else goes, and a copy of every page.
type EmailConfig struct {
	SMTP        string   `yaml:"smtp"` // host:port; STARTTLS is used when offered
	From        string   `yaml:"from"`
	To          []string `yaml:"to"`
	UsernameEnv string   `yaml:"username_env"`
	PasswordEnv string   `yaml:"password_env"`
}

// MirrorConfig copies the signed deny list to a public bucket, the relays'
// fallback when the control plane is down.
type MirrorConfig struct {
	Source       string        `yaml:"source"`  // the control plane's /v1/denylist
	Every        time.Duration `yaml:"every"`   // zero is a minute
	Refresh      time.Duration `yaml:"refresh"` // upload an unchanged list this often; zero is 5 minutes
	Endpoint     string        `yaml:"endpoint"`
	Bucket       string        `yaml:"bucket"`
	Key          string        `yaml:"key"` // zero is denylist.paseto
	Region       string        `yaml:"region"`
	AccessKeyEnv string        `yaml:"access_key_env"`
	SecretKeyEnv string        `yaml:"secret_key_env"`
}

// Roles, as check-config and the commands name them.
const (
	roleTwin   = "twin"
	roleProbe  = "probe"
	roleWatch  = "watch"
	roleMirror = "mirror"
)

// loadConfig reads path and fills in the defaults.
func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfig(b)
}

func parseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("canary.yaml: %w", err)
	}
	c.defaults()
	return &c, nil
}

func (c *Config) defaults() {
	if c.TenantZone == "" {
		c.TenantZone = cloud.TenantZone
	}
	if c.Twin.PinSettle == 0 {
		c.Twin.PinSettle = 90 * time.Second
	}
	p := &c.Probe
	if p.Every == 0 {
		p.Every = time.Minute
	}
	if p.Timeout == 0 {
		p.Timeout = 10 * time.Second
	}
	w := &c.Watch
	if w.Every == 0 {
		w.Every = time.Minute
	}
	if len(w.Relays) == 0 {
		for _, r := range p.Relays {
			w.Relays = append(w.Relays, r.ID)
		}
	}
	orDefault(&w.PageRegions, 2)
	orDefault(&w.PageAfter, 3)
	orDefault(&w.ClearAfter, 3)
	orDefault(&w.RelayAfter, 3)
	orDefault(&w.SilentAfter, 10)
	if w.CertWarn == 0 {
		w.CertWarn = 20 * 24 * time.Hour
	}
	for i := range w.Checks {
		ch := &w.Checks[i]
		if ch.Kind == "" {
			ch.Kind = "http"
		}
		if ch.After == 0 {
			ch.After = 5
		}
		if ch.Kind == "denylist" && ch.MaxAge == 0 {
			ch.MaxAge = 15 * time.Minute
		}
	}
	m := &c.Mirror
	if m.Every == 0 {
		m.Every = time.Minute
	}
	if m.Refresh == 0 {
		m.Refresh = 5 * time.Minute
	}
	if m.Key == "" {
		m.Key = "denylist.paseto"
	}
	if m.Region == "" {
		m.Region = "auto"
	}
}

func orDefault(n *int, d int) {
	if *n == 0 {
		*n = d
	}
}

// check reports every problem with the sections roles use.
func (c *Config) check(roles ...string) error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	needURL := func(name, s string, httpsOnly bool) {
		u, err := url.Parse(s)
		switch {
		case s == "":
			bad("%s is required", name)
		case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
			bad("%s: %q is not an http(s) URL", name, s)
		case httpsOnly && u.Scheme != "https" && !loopbackHost(u.Hostname()):
			bad("%s: %q must be https (plain http only on loopback)", name, s)
		}
	}
	for _, r := range roles {
		switch r {
		case roleTwin:
			needURL("api", c.API, true)
			if c.DataDir == "" {
				bad("data_dir is required")
			}
			if c.Twin.Handle != "" {
				if err := cloud.CheckHandle(c.Twin.Handle); err != nil {
					bad("twin.handle: %v", err)
				}
			}
			if _, err := c.entKeys(); err != nil {
				bad("%v", err)
			}
			if c.Twin.StatusListen != "" {
				checkListen(&errs, "twin.status_listen", c.Twin.StatusListen)
			}
		case roleProbe:
			p := c.Probe
			if p.Region == "" {
				bad("probe.region is required")
			}
			if p.Host == "" || strings.ContainsAny(p.Host, "/: ") || !strings.Contains(p.Host, ".") {
				bad("probe.host: %q is not a host name", p.Host)
			}
			if len(p.Relays) == 0 {
				bad("probe.relays: name each relay and its addresses")
			}
			seen := map[string]bool{}
			for _, r := range p.Relays {
				if r.ID == "" || seen[r.ID] {
					bad("probe.relays: ids must be set and different")
				}
				seen[r.ID] = true
				if len(r.Addrs) == 0 {
					bad("probe.relays %s: no addrs", r.ID)
				}
				for _, a := range r.Addrs {
					if _, _, err := net.SplitHostPort(a); err != nil {
						bad("probe.relays %s: %q is not host:port", r.ID, a)
					}
				}
			}
			if p.Every < 10*time.Second || p.Timeout <= 0 || p.Timeout >= p.Every {
				bad("probe: every must be at least 10s and timeout shorter than every")
			}
			if p.Listen != "" {
				checkListen(&errs, "probe.listen", p.Listen)
			}
		case roleWatch:
			w := c.Watch
			if len(w.Relays) == 0 {
				bad("watch.relays: name the relays (or list them under probe.relays)")
			}
			if len(w.Regions) < w.PageRegions {
				bad("watch.regions: %d probes can never make the %d regions the paging rule needs", len(w.Regions), w.PageRegions)
			}
			names, urls := map[string]bool{}, map[string]bool{}
			for _, r := range w.Regions {
				if r.Name == "" || names[r.Name] {
					bad("watch.regions: names must be set and different")
				}
				u := strings.ToLower(strings.TrimRight(r.URL, "/"))
				if urls[u] {
					bad("watch.regions %s: url %q is another region's probe; one region's network would count twice", r.Name, r.URL)
				}
				names[r.Name], urls[u] = true, true
				needURL("watch.regions "+r.Name+" url", r.URL, false)
			}
			if w.PageRegions < 2 {
				bad("watch.page_regions: at least 2, so one region's own network can't page")
			}
			if w.PageAfter < 1 || w.ClearAfter < 1 || w.RelayAfter < 1 || w.SilentAfter < 1 {
				bad("watch: page_after, clear_after, relay_after and silent_after count rounds, at least 1")
			}
			if w.Every < 10*time.Second {
				bad("watch.every: at least 10s")
			}
			for _, ch := range w.Checks {
				if ch.Name == "" {
					bad("watch.checks: every check needs a name")
				}
				needURL("watch.checks "+ch.Name+" url", ch.URL, false)
				switch ch.Kind {
				case "http":
				case "denylist":
					if _, err := c.dlKeys(); err != nil {
						bad("watch.checks %s: %v", ch.Name, err)
					}
				default:
					bad("watch.checks %s: kind is http or denylist, not %q", ch.Name, ch.Kind)
				}
			}
			switch w.Page.Format {
			case "pagerduty":
				if w.Page.RoutingKeyEnv == "" {
					bad("watch.page: pagerduty needs routing_key_env")
				}
			case "webhook":
				if w.Page.URL == "" && w.Page.URLEnv == "" {
					bad("watch.page: webhook needs url or url_env")
				}
			default:
				bad("watch.page.format is pagerduty or webhook, not %q", w.Page.Format)
			}
			if w.Page.URL != "" {
				needURL("watch.page.url", w.Page.URL, true)
			}
			e := w.Email
			if e.SMTP == "" || e.From == "" || len(e.To) == 0 {
				bad("watch.email needs smtp, from and to: everything that doesn't page is emailed")
			} else if _, _, err := net.SplitHostPort(e.SMTP); err != nil {
				bad("watch.email.smtp: %q is not host:port", e.SMTP)
			}
			if w.Listen != "" {
				checkListen(&errs, "watch.listen", w.Listen)
			}
		case roleMirror:
			m := c.Mirror
			needURL("mirror.source", m.Source, true)
			needURL("mirror.endpoint", m.Endpoint, true)
			if m.Bucket == "" || m.AccessKeyEnv == "" || m.SecretKeyEnv == "" {
				bad("mirror needs bucket, access_key_env and secret_key_env")
			}
			if m.Key == "" || strings.HasPrefix(m.Key, "/") {
				bad("mirror.key: %q", m.Key)
			}
			if m.Refresh < m.Every {
				bad("mirror.refresh: at least mirror.every")
			}
			if _, err := c.dlKeys(); err != nil {
				bad("%v", err)
			}
		default:
			bad("unknown role %q", r)
		}
	}
	return errors.Join(errs...)
}

func checkListen(errs *[]error, name, addr string) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not host:port", name, addr))
	}
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// entKeys are the keys entitlements are verified with.
func (c *Config) entKeys() (map[string]ed25519.PublicKey, error) {
	return keySet("entitlement_keys", "ent-", c.EntitlementKeys, entitle.EntitlementKeys)
}

// dlKeys are the keys deny lists are verified with.
func (c *Config) dlKeys() (map[string]ed25519.PublicKey, error) {
	return keySet("denylist_keys", "dl-", c.DenyListKeys, entitle.DenyListKeys)
}

func keySet(name, prefix string, in map[string]string, compiled map[string]ed25519.PublicKey) (map[string]ed25519.PublicKey, error) {
	if len(in) == 0 {
		if len(compiled) == 0 {
			return nil, fmt.Errorf("%s: this build trusts no keys yet; list them (mirrin-cloud keys public)", name)
		}
		return compiled, nil
	}
	out := map[string]ed25519.PublicKey{}
	for kid, s := range in {
		if !strings.HasPrefix(kid, prefix) {
			return nil, fmt.Errorf("%s: %q is not a %s* kid", name, kid, prefix)
		}
		k, err := entitle.ParseKey(s)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", name, kid, err)
		}
		out[kid] = k
	}
	return out, nil
}

// loadRoots reads a PEM file of CAs; "" is nil (the system roots).
func loadRoots(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s holds no PEM certificates", path)
	}
	return p, nil
}
