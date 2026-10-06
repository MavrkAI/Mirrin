package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// renderDeploy renders one host's files from cloud/deploy, with test
// values for what a maintainer fills in.
func renderDeploy(t *testing.T, host string) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	pub := func() string {
		k, _, _ := ed25519.GenerateKey(crand.Reader)
		return entitle.EncodeKey(k)
	}
	vals := filepath.Join(t.TempDir(), "test.env")
	os.WriteFile(vals, fmt.Appendf(nil, "ENT_PUB=%s\nDL_PUB=%s\nR2_ACCOUNT_ID=0123456789abcdef\nPADDLE_PRICE_ID=pri_01test\nROUTE53_HOSTED_ZONE_ID=Z0TEST\n", pub(), pub()), 0o600)
	out := filepath.Join(t.TempDir(), host)
	dep := filepath.Join("..", "..", "deploy")
	cmd := exec.Command("sh", filepath.Join(dep, "render.sh"), out, filepath.Join(dep, "hosts", "common.env"), filepath.Join(dep, "hosts", host+".env"), vals)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render %s: %v %s", host, err, b)
	}
	return out
}

// The configs cloud/deploy renders pass check-config for every role their
// host runs, exactly as the unit's ExecStartPre runs it.
func TestDeployConfigs(t *testing.T) {
	for host, roles := range map[string]string{"cp": "twin,probe,watch,mirror", "probe-b": "probe", "probe-c": "probe"} {
		path := filepath.Join(renderDeploy(t, host), "canary.yaml")
		var out, errb bytes.Buffer
		if code := run(context.Background(), []string{"check-config", "--config", path, "--role", roles}, &out, &errb); code != 0 {
			t.Errorf("%s: %s", host, errb.String())
		}
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if host == "cp" {
			w := cfg.Watch
			if w.PageRegions != 2 || w.PageAfter != 3 || len(w.Regions) < 3 {
				t.Errorf("the paging rule in canary.yaml: %d regions, %d rounds, %d probes", w.PageRegions, w.PageAfter, len(w.Regions))
			}
			if cfg.Probe.Host != cfg.Twin.Handle+"."+cfg.TenantZone {
				t.Errorf("the probe checks %q, not the canary's %q", cfg.Probe.Host, cfg.Twin.Handle)
			}
		}
	}
}

func TestConfigChecks(t *testing.T) {
	base := func() *Config {
		c := &Config{
			API: "https://cloud.example.org", DataDir: "/var/lib/x",
			EntitlementKeys: map[string]string{"ent-2026a": entitle.EncodeKey(make(ed25519.PublicKey, 32))},
			DenyListKeys:    map[string]string{"dl-2026a": entitle.EncodeKey(make(ed25519.PublicKey, 32))},
			Probe:           ProbeConfig{Region: "a", Host: "canary.example.net", Relays: []ProbeRelay{{ID: "r1", Addrs: []string{"192.0.2.1:443"}}, {ID: "r2", Addrs: []string{"[2001:db8::1]:443"}}}},
			Watch: WatchConfig{Regions: []Region{{Name: "a", URL: "http://127.0.0.1:9102/v1/results"}, {Name: "b", URL: "https://p.example.org/v1/results"}},
				Page: PageConfig{Format: "pagerduty", RoutingKeyEnv: "K"}, Email: EmailConfig{SMTP: "smtp.example.org:587", From: "a@example.org", To: []string{"b@example.org"}}},
		}
		c.defaults()
		return c
	}
	if err := base().check(roleTwin, roleProbe, roleWatch); err != nil {
		t.Fatalf("base: %v", err)
	}
	for name, c := range map[string]struct {
		edit func(*Config)
		role string
		want string
	}{
		"one region can page":    {func(c *Config) { c.Watch.PageRegions = 1 }, roleWatch, "at least 2"},
		"too few probes":         {func(c *Config) { c.Watch.Regions = c.Watch.Regions[:1] }, roleWatch, "can never make"},
		"no email":               {func(c *Config) { c.Watch.Email = EmailConfig{} }, roleWatch, "everything that doesn't page is emailed"},
		"no pager":               {func(c *Config) { c.Watch.Page = PageConfig{} }, roleWatch, "pagerduty or webhook"},
		"plain http api":         {func(c *Config) { c.API = "http://cloud.example.org" }, roleTwin, "must be https"},
		"reserved handle":        {func(c *Config) { c.Twin.Handle = "a" }, roleTwin, "twin.handle"},
		"bad kid":                {func(c *Config) { c.EntitlementKeys = map[string]string{"dl-x": "x"} }, roleTwin, "not a ent-*"},
		"probe addr":             {func(c *Config) { c.Probe.Relays[0].Addrs = []string{"192.0.2.1"} }, roleProbe, "not host:port"},
		"one probe, two regions": {func(c *Config) { c.Watch.Regions[1].URL = c.Watch.Regions[0].URL + "/" }, roleWatch, "another region's probe"},
		"timeout":                {func(c *Config) { c.Probe.Timeout = c.Probe.Every }, roleProbe, "timeout shorter"},
	} {
		cfg := base()
		c.edit(cfg)
		if err := cfg.check(c.role); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestUnknownFieldsFail(t *testing.T) {
	if _, err := parseConfig([]byte("watch:\n  page_region: 1\n")); err == nil {
		t.Fatal("a misspelt key was accepted; it would silently leave the default")
	}
}
