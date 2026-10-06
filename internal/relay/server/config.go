package server

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// Config is relay.yaml. A relay runs in one of two modes:
//   - hosted: tunnels present an entitlement signed by an issuer key, and a
//     signed deny list is polled (zones, issuer_keys, denylist_url);
//   - self-host: a static allow list of {hostname, key} (allow), with no
//     MavrkAI involvement at all.
type Config struct {
	ID              string `yaml:"id"`               // relay id, such as "r1"
	Listen          string `yaml:"listen"`           // TLS: SNI routing, default ":443"
	HTTPListen      string `yaml:"http_listen"`      // plain HTTP that only redirects, default ":80"; "" is off
	ControlHostname string `yaml:"control_hostname"` // the one name this relay terminates TLS for
	CertFile        string `yaml:"cert_file"`        // the control name's certificate, instead of ACME
	KeyFile         string `yaml:"key_file"`
	ACMEEmail       string `yaml:"acme_email"`
	ACMEDirectory   string `yaml:"acme_directory"` // default Let's Encrypt
	StateDir        string `yaml:"state_dir"`      // ACME cache and the last good deny list

	Zones       []string    `yaml:"zones"`       // hosted: tenant zones; every host is <handle>.<zone>
	IssuerKeys  []IssuerKey `yaml:"issuer_keys"` // hosted: ent-* and dl-* public keys; empty uses the compiled-in sets
	DenylistURL string      `yaml:"denylist_url"`
	MirrorURL   string      `yaml:"mirror_url"`

	Allow []Allow `yaml:"allow"` // self-host

	Limits        Limits  `yaml:"limits"`
	Abuse         Abuse   `yaml:"abuse"`
	MetricsListen string  `yaml:"metrics_listen"` // default 127.0.0.1:9100; "" is off
	ConnLog       ConnLog `yaml:"conn_log"`
}

// IssuerKey is one public key a hosted relay trusts. The kid's prefix says
// what it signs: ent-* entitlements, dl-* deny lists.
type IssuerKey struct {
	Kid string `yaml:"kid"`
	Key string `yaml:"key"` // unpadded base64url Ed25519 public key
}

// Allow lets one device key carry one hostname through a self-hosted
// relay. `mirrin reach use relay` prints the line.
type Allow struct {
	Hostname string `yaml:"hostname"`
	Key      string `yaml:"key"` // the daemon's device key, unpadded base64url
}

// Limits bound what one tunnel and one address can use.
type Limits struct {
	StreamsPerTunnel int           `yaml:"streams_per_tunnel"`   // concurrent client connections
	BPS              int64         `yaml:"bps"`                  // bits per second per tunnel, both directions; 0 is unlimited
	HelloPerIPPerMin int           `yaml:"hello_per_ip_per_min"` // tunnel hellos per address (IPv6 /64); 0 is unlimited
	Idle             time.Duration `yaml:"idle"`                 // a client connection with no bytes either way
}

// Abuse configures the distinct-client heuristic.
type Abuse struct {
	DistinctNetsPerDay int           `yaml:"distinct_nets_per_day"` // client /24s and /48s per handle per UTC day; 0 is off
	SuspendFor         time.Duration `yaml:"suspend_for"`
	// NotifyCommand, an absolute path, is run as `<command> <handle>
	// <networks>` on each suspension, so the operator hears of it (a
	// mail, a chat message). mirrin-relay serve runs it; the server
	// package only checks it.
	NotifyCommand string `yaml:"notify_command"`
}

// ConnLog bounds the in-memory connection log.
type ConnLog struct {
	Entries       int `yaml:"entries"`        // raw records, kept at most 72 h
	HourlyEntries int `yaml:"hourly_entries"` // per-handle hourly totals, kept at most 30 days
}

// DefaultConfig is what a relay.yaml leaves unset.
func DefaultConfig() Config {
	return Config{
		Listen:        ":443",
		HTTPListen:    ":80",
		ACMEDirectory: autocert.DefaultACMEDirectory,
		Limits:        Limits{StreamsPerTunnel: 64, BPS: 20e6, HelloPerIPPerMin: 10, Idle: 10 * time.Minute},
		Abuse:         Abuse{DistinctNetsPerDay: 200, SuspendFor: 7 * 24 * time.Hour},
		MetricsListen: "127.0.0.1:9100",
		ConnLog:       ConnLog{Entries: 1 << 20, HourlyEntries: 1 << 20},
	}
}

const maxConfigSize = 4 << 20

// LoadConfig reads and checks a relay.yaml.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxConfigSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, maxConfigSize)
	}
	c, err := ParseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// ParseConfig reads one YAML document over DefaultConfig. Unknown fields
// are errors, so a typo cannot silently turn a check off.
func ParseConfig(b []byte) (*Config, error) {
	c := DefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one YAML document")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// SelfHosted reports whether the relay checks a static allow list rather
// than entitlements.
func (c *Config) SelfHosted() bool { return len(c.Allow) > 0 }

// Validate checks the whole config.
func (c *Config) Validate() error {
	_, err := c.compile()
	return err
}

// policy is a checked Config in the form the server uses.
type policy struct {
	entKeys, dlKeys map[string]ed25519.PublicKey // hosted
	allow           map[string][]string          // self-host: EncodeKey(key) → sorted hostnames
}

func (c *Config) compile() (*policy, error) {
	switch {
	case !wire.ValidRelayID(c.ID):
		return nil, fmt.Errorf("id %q: 1-32 of a-z 0-9 . _ -, starting with a letter or digit", c.ID)
	case !wire.ValidHostname(c.ControlHostname):
		return nil, fmt.Errorf("control_hostname %q is not a lower-case DNS name", c.ControlHostname)
	case c.Listen == "":
		return nil, errors.New("listen is required")
	case (c.CertFile == "") != (c.KeyFile == ""):
		return nil, errors.New("cert_file and key_file go together")
	case c.CertFile == "" && c.StateDir == "":
		return nil, errors.New("state_dir is required for the ACME cache (or set cert_file and key_file)")
	}
	for _, a := range []struct{ name, v string }{{"listen", c.Listen}, {"http_listen", c.HTTPListen}, {"metrics_listen", c.MetricsListen}} {
		if a.v == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(a.v); err != nil {
			return nil, fmt.Errorf("%s %q: %v", a.name, a.v, err)
		}
	}
	if c.CertFile == "" {
		if err := httpsURL("acme_directory", c.ACMEDirectory); err != nil {
			return nil, err
		}
	}
	if err := c.Limits.check(); err != nil {
		return nil, err
	}
	switch {
	case c.Abuse.DistinctNetsPerDay < 0:
		return nil, errors.New("abuse.distinct_nets_per_day is negative")
	case c.Abuse.DistinctNetsPerDay > 0 && c.Abuse.SuspendFor <= 0:
		return nil, errors.New("abuse.suspend_for must be positive")
	case c.Abuse.NotifyCommand != "" && !filepath.IsAbs(c.Abuse.NotifyCommand):
		return nil, fmt.Errorf("abuse.notify_command %q must be an absolute path", c.Abuse.NotifyCommand)
	case c.ConnLog.Entries < 0 || c.ConnLog.HourlyEntries < 0:
		return nil, errors.New("conn_log sizes are negative")
	}
	if c.SelfHosted() {
		return c.compileSelfHost()
	}
	return c.compileHosted()
}

func (l Limits) check() error {
	switch {
	case l.StreamsPerTunnel < 1 || l.StreamsPerTunnel > 4096:
		return errors.New("limits.streams_per_tunnel: 1 to 4096")
	case l.BPS != 0 && l.BPS < 1e6:
		return errors.New("limits.bps: 0 (unlimited) or at least 1000000")
	case l.HelloPerIPPerMin < 0:
		return errors.New("limits.hello_per_ip_per_min is negative")
	case l.Idle < time.Second:
		return errors.New("limits.idle: at least 1s")
	}
	return nil
}

func (c *Config) compileSelfHost() (*policy, error) {
	switch {
	case len(c.Zones) > 0 || len(c.IssuerKeys) > 0:
		return nil, errors.New("allow (self-host) cannot be combined with zones or issuer_keys (hosted)")
	case c.DenylistURL != "" || c.MirrorURL != "":
		return nil, errors.New("a self-hosted relay has no deny list; remove denylist_url and mirror_url")
	}
	p := &policy{allow: map[string][]string{}}
	for i, a := range c.Allow {
		if !wire.ValidHostname(a.Hostname) {
			return nil, fmt.Errorf("allow[%d]: hostname %q is not a lower-case DNS name", i, a.Hostname)
		}
		if a.Hostname == c.ControlHostname {
			return nil, fmt.Errorf("allow[%d]: %s is the control name", i, a.Hostname)
		}
		k, err := entitle.ParseKey(a.Key)
		if err != nil {
			return nil, fmt.Errorf("allow[%d]: key: %v", i, err)
		}
		enc := entitle.EncodeKey(k)
		if slices.Contains(p.allow[enc], a.Hostname) {
			return nil, fmt.Errorf("allow[%d]: listed twice", i)
		}
		p.allow[enc] = append(p.allow[enc], a.Hostname)
	}
	for k := range p.allow {
		slices.Sort(p.allow[k])
		if len(p.allow[k]) > wire.MaxHostnames {
			return nil, fmt.Errorf("allow: one key has more than %d hostnames", wire.MaxHostnames)
		}
	}
	return p, nil
}

func (c *Config) compileHosted() (*policy, error) {
	if len(c.Zones) == 0 {
		return nil, errors.New("hosted mode needs zones (or allow, for a self-hosted relay)")
	}
	for _, z := range c.Zones {
		if !wire.ValidHostname(z) {
			return nil, fmt.Errorf("zone %q is not a lower-case DNS name", z)
		}
		if c.ControlHostname == z || strings.HasSuffix(c.ControlHostname, "."+z) {
			return nil, fmt.Errorf("control_hostname %s is inside tenant zone %s", c.ControlHostname, z)
		}
	}
	if err := httpsURL("denylist_url", c.DenylistURL); err != nil {
		return nil, err
	}
	if c.MirrorURL != "" {
		if err := httpsURL("mirror_url", c.MirrorURL); err != nil {
			return nil, err
		}
	}
	p := &policy{entKeys: map[string]ed25519.PublicKey{}, dlKeys: map[string]ed25519.PublicKey{}}
	if len(c.IssuerKeys) == 0 {
		maps.Copy(p.entKeys, entitle.EntitlementKeys)
		maps.Copy(p.dlKeys, entitle.DenyListKeys)
	}
	for i, k := range c.IssuerKeys {
		pub, err := entitle.ParseKey(k.Key)
		if err != nil {
			return nil, fmt.Errorf("issuer_keys[%d]: %v", i, err)
		}
		var set map[string]ed25519.PublicKey
		switch {
		case validKid(k.Kid, "ent-"):
			set = p.entKeys
		case validKid(k.Kid, "dl-"):
			set = p.dlKeys
		default:
			return nil, fmt.Errorf("issuer_keys[%d]: kid %q is neither ent-* nor dl-*", i, k.Kid)
		}
		if _, dup := set[k.Kid]; dup {
			return nil, fmt.Errorf("issuer_keys[%d]: kid %s listed twice", i, k.Kid)
		}
		set[k.Kid] = pub
	}
	for kid, ek := range p.entKeys {
		for dkid, dk := range p.dlKeys {
			if ek.Equal(dk) {
				return nil, fmt.Errorf("issuer_keys: %s and %s are the same key; each purpose needs its own", kid, dkid)
			}
		}
	}
	switch {
	case len(p.entKeys) == 0:
		return nil, errors.New("hosted mode needs at least one ent-* issuer key")
	case len(p.dlKeys) == 0:
		return nil, errors.New("hosted mode needs at least one dl-* issuer key for the deny list")
	}
	return p, nil
}

// validKid is entitle's kid rule: the prefix, then 1-32 of a-z 0-9 -.
func validKid(kid, prefix string) bool {
	rest, ok := strings.CutPrefix(kid, prefix)
	if !ok || rest == "" || len(rest) > 32 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if c := rest[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func httpsURL(name, s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("%s %q must be an https:// URL", name, s)
	}
	return nil
}
