package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// Config is cloud.yaml. Secrets are never in it: it names the environment
// variables that hold them.
type Config struct {
	// Listen is the address to serve on. Plain HTTP unless cert_file and
	// key_file are set, for a TLS proxy in front.
	Listen   string `yaml:"listen"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// TrustedProxies are the addresses (or prefixes) of the TLS proxies in
	// front. For a request from one of them, the client's address is the
	// last one in X-Forwarded-For that is not a proxy; for any other
	// request, the peer's. It is used for rate limits, in memory only.
	// Serving plain HTTP in production needs it, or every client would share
	// the proxy's limits.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// PublicURL is the origin daemons send to and sign for, such as
	// https://cloud.mirrin.app. A request signed for any other is refused.
	PublicURL string `yaml:"public_url"`
	// DataDir holds cloud.db (replicate it with Litestream).
	DataDir string `yaml:"data_dir"`
	// TenantZone holds the handles, such as mirrin.link.
	TenantZone string `yaml:"tenant_zone"`
	// Relays are named in every entitlement; their addresses are the
	// handles' A and AAAA records.
	Relays []Relay `yaml:"relays"`
	// IODEF is where CAs report refused issuance, such as
	// mailto:security@mirrin.app.
	IODEF string `yaml:"iodef"`
	// Plan, Features and BackupQuota go into entitlements.
	Plan        string   `yaml:"plan"`
	Features    []string `yaml:"features"`
	BackupQuota int64    `yaml:"backup_quota"`
	// ActivationsPerWeek caps new handles in any 7 days, under Let's
	// Encrypt's per-domain limit; 0 is no cap.
	ActivationsPerWeek int `yaml:"activations_per_week"`
	// LinkStartsPerMinute limits link/start per client (an IPv4 address,
	// or an IPv6 /48), held in memory only; 0 is no limit, and also turns
	// off the looser limit on the link routes before signatures are checked.
	LinkStartsPerMinute int `yaml:"link_starts_per_minute"`
	// LinkStartsTotalPerMinute limits link/start from everyone together, so
	// no crowd of addresses can open unbounded checkouts; 0 is no limit.
	LinkStartsTotalPerMinute int `yaml:"link_starts_total_per_minute"`
	// DeviceRequestsPerMinute limits the signed device routes per device
	// key, which share the DNS and merchant-of-record quotas; 0 is no limit.
	DeviceRequestsPerMinute int `yaml:"device_requests_per_minute"`
	// ACMEAccountHosts are the ACME servers whose account URIs a CAA
	// record may name; empty allows any https URI.
	ACMEAccountHosts []string `yaml:"acme_account_hosts"`
	Keys             Keys     `yaml:"keys"`
	Billing          Billing  `yaml:"billing"`
	DNS              DNS      `yaml:"dns"`
	Storage          Storage  `yaml:"storage"`
}

// Storage configures where backup objects are kept. With no provider the
// server keeps no backups (--dev uses a fake on disk).
type Storage struct {
	Provider string `yaml:"provider"` // r2, or empty for none
	R2       struct {
		Endpoint     string `yaml:"endpoint"` // https://<account>.r2.cloudflarestorage.com
		Bucket       string `yaml:"bucket"`
		Region       string `yaml:"region"` // auto
		AccessKeyEnv string `yaml:"access_key_env"`
		SecretKeyEnv string `yaml:"secret_key_env"`
	} `yaml:"r2"`
}

// Relay is one relay as entitlements name it.
type Relay struct {
	ID  string   `yaml:"id"`
	URL string   `yaml:"url"`
	IPs []string `yaml:"ips"`
}

// Keys says where the signing keys are and which sign.
type Keys struct {
	Dir         string `yaml:"dir"`         // <kid>.pem files; "$CREDENTIALS_DIRECTORY" is expanded
	Entitlement string `yaml:"entitlement"` // the ent-* kid that signs
	DenyList    string `yaml:"denylist"`    // the dl-* kid that signs
}

// Billing configures the merchant of record.
type Billing struct {
	Provider string `yaml:"provider"` // paddle
	Paddle   struct {
		APIBase          string            `yaml:"api_base"`
		APIKeyEnv        string            `yaml:"api_key_env"`
		WebhookSecretEnv string            `yaml:"webhook_secret_env"`
		Prices           map[string]string `yaml:"prices"` // plan → price id
	} `yaml:"paddle"`
}

// DNS configures the tenant zone's provider.
type DNS struct {
	Provider string `yaml:"provider"` // route53
	Route53  struct {
		HostedZoneID    string `yaml:"hosted_zone_id"`
		AccessKeyEnv    string `yaml:"access_key_env"`
		SecretKeyEnv    string `yaml:"secret_key_env"`
		SessionTokenEnv string `yaml:"session_token_env"`
		Endpoint        string `yaml:"endpoint"`
		TTL             int    `yaml:"ttl"`
	} `yaml:"route53"`
}

// DefaultConfig is what cloud.yaml leaves unset.
func DefaultConfig() Config {
	return Config{
		Listen:                   "127.0.0.1:8787",
		TenantZone:               "mirrin.link",
		IODEF:                    "mailto:security@mirrin.app",
		Plan:                     "cloud",
		Features:                 []string{"reach", "backup"},
		BackupQuota:              20 << 30,
		ActivationsPerWeek:       40,
		LinkStartsPerMinute:      10,
		LinkStartsTotalPerMinute: 60,
		DeviceRequestsPerMinute:  30,
		ACMEAccountHosts:         []string{"acme-v02.api.letsencrypt.org"},
	}
}

// DevConfig is the config of `mirrin-cloud serve --dev` with no --config:
// loopback only, no caps, any ACME server, and relays at documentation
// addresses.
func DevConfig() Config {
	c := DefaultConfig()
	c.PublicURL = "http://" + c.Listen
	c.ActivationsPerWeek, c.LinkStartsPerMinute, c.LinkStartsTotalPerMinute, c.DeviceRequestsPerMinute = 0, 0, 0, 0
	c.ACMEAccountHosts = nil
	c.Relays = []Relay{
		{ID: "r1", URL: "wss://r1.relay.mirrin.app/v1/tunnel", IPs: []string{"192.0.2.1", "2001:db8::1"}},
		{ID: "r2", URL: "wss://r2.relay.mirrin.app/v1/tunnel", IPs: []string{"198.51.100.1", "2001:db8::2"}},
	}
	return c
}

const maxConfigSize = 1 << 20

// LoadConfig reads cloud.yaml over base (DefaultConfig or DevConfig).
func LoadConfig(path string, base Config) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return Config{}, err
	}
	if len(b) > maxConfigSize {
		return Config{}, fmt.Errorf("%s: larger than %d bytes", path, maxConfigSize)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&base); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: more than one YAML document", path)
	}
	return base, nil
}

// Validate checks the parts every mode needs. dev allows a plain-http
// loopback origin, and insists on a loopback listener.
func (c *Config) Validate(dev bool) error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q: %v", c.Listen, err)
	}
	if dev {
		if err := CheckDevListen(c.Listen); err != nil {
			return err
		}
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("cert_file and key_file go together")
	}
	if _, err := c.trustedProxies(); err != nil {
		return err
	}
	if !dev && c.CertFile == "" && len(c.TrustedProxies) == 0 && c.LinkStartsPerMinute > 0 {
		return errors.New("listen serves plain HTTP, so a TLS proxy is in front: set trusted_proxies to its address, " +
			"or every client shares one rate limit (or set cert_file and key_file to end TLS here)")
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
		!(u.Scheme == "https" || dev && u.Scheme == "http" && isLoopback(u.Hostname())) {
		return fmt.Errorf("public_url %q must be an https origin (http only for loopback in --dev)", c.PublicURL)
	}
	for l := range strings.SplitSeq(c.TenantZone, ".") {
		if l == "" || strings.Trim(l, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return fmt.Errorf("tenant_zone %q is not a lowercase DNS name", c.TenantZone)
		}
	}
	if len(c.Relays) == 0 {
		return errors.New("at least one relay is required")
	}
	ids := map[string]bool{}
	for _, r := range c.Relays {
		ru, err := url.Parse(r.URL)
		if r.ID == "" || ids[r.ID] || err != nil || ru.Scheme != "wss" || ru.Host == "" {
			return fmt.Errorf("relay %q: a unique id and a wss:// url are required", r.ID)
		}
		ids[r.ID] = true
		if len(r.IPs) == 0 {
			return fmt.Errorf("relay %s: ips are required (they become the handles' A and AAAA records)", r.ID)
		}
		for _, ip := range r.IPs {
			a, err := netip.ParseAddr(ip)
			if err != nil || a.Zone() != "" || a.Is4In6() {
				return fmt.Errorf("relay %s: %q is not an IP address", r.ID, ip)
			}
		}
	}
	if c.Plan == "" || len(c.Features) == 0 || slices.Contains(c.Features, "") || c.BackupQuota < 0 {
		return errors.New("plan, features and a non-negative backup_quota are required")
	}
	if c.ActivationsPerWeek < 0 || c.LinkStartsPerMinute < 0 || c.LinkStartsTotalPerMinute < 0 || c.DeviceRequestsPerMinute < 0 {
		return errors.New("activations_per_week and the per-minute limits are not negative")
	}
	return nil
}

// CheckDevListen refuses a --dev listener that is not a loopback address:
// a dev server's routes take no signature, and anyone can derive its keys.
func CheckDevListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen %q: %v", addr, err)
	}
	if a, err := netip.ParseAddr(host); err != nil || !a.IsLoopback() {
		return fmt.Errorf("--dev listens on a loopback address only, such as 127.0.0.1:8787, not %q", addr)
	}
	return nil
}

// trustedProxies parses trusted_proxies: addresses or prefixes.
func (c *Config) trustedProxies() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range c.TrustedProxies {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil || a.Zone() != "" {
				return nil, fmt.Errorf("trusted_proxies: %q is not an address or prefix", s)
			}
			p = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// relays returns the relays as entitlements carry them, and their v4 and
// v6 addresses for DNS.
func (c *Config) relays() ([]entitle.Relay, []netip.Addr, []netip.Addr) {
	var out []entitle.Relay
	var v4, v6 []netip.Addr
	for _, r := range c.Relays {
		out = append(out, entitle.Relay{ID: r.ID, URL: r.URL, IPs: slices.Clone(r.IPs)})
		for _, ip := range r.IPs {
			a := netip.MustParseAddr(ip)
			if a.Is4() {
				v4 = append(v4, a)
			} else {
				v6 = append(v6, a)
			}
		}
	}
	return out, v4, v6
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}
