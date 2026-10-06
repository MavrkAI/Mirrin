package deploy

// These tests render every host's files with render.sh and check them the
// way the services will: each config through its own loader, the systemd
// units for hardening and for credentials that match what the configs
// read, the DNS files for the zone rules, and the scripts for syntax.

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/cloud/internal/dns"
	"github.com/MavrkAI/Mirrin/cloud/internal/keys"
	"github.com/MavrkAI/Mirrin/cloud/internal/server"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	relayserver "github.com/MavrkAI/Mirrin/internal/relay/server"
)

// hosts are the host files, one per machine, plus the DNS files.
var hosts = []string{"r1", "r2", "cp", "probe-b", "probe-c", "dns"}

// testValues stand in for the values a maintainer fills in at setup.
func testValues(t *testing.T) string {
	t.Helper()
	pub := func() string {
		k, _, _ := ed25519.GenerateKey(rand.Reader)
		return entitle.EncodeKey(k)
	}
	p := filepath.Join(t.TempDir(), "test.env")
	vals := fmt.Sprintf("ENT_PUB=%s\nDL_PUB=%s\nR2_ACCOUNT_ID=0123456789abcdef0123456789abcdef\nPADDLE_PRICE_ID=pri_01test\nROUTE53_HOSTED_ZONE_ID=Z0123456789TEST\n", pub(), pub())
	if err := os.WriteFile(p, []byte(vals), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func needSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
}

// render runs render.sh for host with extra env files after it, and
// returns the output directory.
func render(t *testing.T, host string, extra ...string) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), host)
	args := append([]string{"render.sh", out, "hosts/common.env", "hosts/" + host + ".env"}, extra...)
	cmd := exec.Command("sh", args...)
	cmd.Env = append(os.Environ(), "ALLOW_PLACEHOLDERS=")
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, b)
	}
	return out, nil
}

func mustRender(t *testing.T, host string) string {
	t.Helper()
	out, err := render(t, host, testValues(t))
	if err != nil {
		t.Fatalf("render %s: %v", host, err)
	}
	return out
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEveryHostRenders(t *testing.T) {
	needSh(t)
	for _, h := range hosts {
		out := mustRender(t, h)
		files, _ := os.ReadDir(out)
		if len(files) == 0 {
			t.Fatalf("%s: nothing rendered", h)
		}
		for _, f := range files {
			if s := read(t, filepath.Join(out, f.Name())); strings.Contains(s, "@@") {
				t.Errorf("%s/%s: a placeholder is left", h, f.Name())
			}
		}
	}
}

func TestRenderRefusesPlaceholdersAndUnknownKeys(t *testing.T) {
	needSh(t)
	if _, err := render(t, "cp"); err == nil || !strings.Contains(err.Error(), "still a placeholder") {
		t.Fatalf("rendered with REPLACE values: %v", err)
	}
	// A template naming a key no file sets is an error, not an empty value.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "only.env"), []byte("ROLE=relay\nRELAY_ID=r1\n"), 0o600)
	cmd := exec.Command("sh", "render.sh", filepath.Join(dir, "out"), filepath.Join(dir, "only.env"))
	cmd.Env = append(os.Environ(), "ALLOW_PLACEHOLDERS=1")
	if b, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(b), "which no env file sets") {
		t.Fatalf("rendered with keys missing: %v %s", err, b)
	}
	// A failed render leaves no half-rendered OUTDIR to copy by mistake.
	if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Fatalf("a failed render left its output: %v", err)
	}
}

// During a key rotation ENT_KID2 / DL_KID2 add the second kid to the
// control plane's credentials, the relays' issuer keys, the canary's
// trusted keys and install.sh's checks; the rest of the year they add
// nothing.
func TestSecondKidDuringRotation(t *testing.T) {
	needSh(t)
	pub := func() string {
		k, _, _ := ed25519.GenerateKey(rand.Reader)
		return entitle.EncodeKey(k)
	}
	rot := filepath.Join(t.TempDir(), "rotation.env")
	os.WriteFile(rot, []byte("ENT_KID2=ent-2027a\nENT_PUB2="+pub()+"\nDL_KID2=dl-2027a\nDL_PUB2="+pub()+"\n"), 0o600)
	for _, rotating := range []bool{false, true} {
		extra := []string{testValues(t)}
		if rotating {
			extra = append(extra, rot)
		}
		cp, err := render(t, "cp", extra...)
		if err != nil {
			t.Fatal(err)
		}
		r1, err := render(t, "r1", extra...)
		if err != nil {
			t.Fatal(err)
		}
		creds := parseUnit(t, filepath.Join(cp, "mirrin-cloud.service")).creds()
		relay, err := relayserver.LoadConfig(filepath.Join(r1, "relay.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var kids []string
		for _, ik := range relay.IssuerKeys {
			kids = append(kids, ik.Kid)
		}
		var canary struct {
			Ent map[string]string `yaml:"entitlement_keys"`
			DL  map[string]string `yaml:"denylist_keys"`
		}
		if err := yaml.Unmarshal([]byte(read(t, filepath.Join(cp, "canary.yaml"))), &canary); err != nil {
			t.Fatal(err)
		}
		install := read(t, filepath.Join(cp, "install.sh"))
		for _, kid := range []string{"ent-2027a", "dl-2027a"} {
			has := []bool{slices.Contains(creds, kid+".pem"), slices.Contains(kids, kid),
				canary.Ent[kid] != "" || canary.DL[kid] != "", strings.Contains(install, "need_creds "+kid+".pem")}
			for i, h := range has {
				if h != rotating {
					t.Errorf("rotating %v: %s in [unit relay canary install][%d] is %v", rotating, kid, i, h)
				}
			}
		}
		if n := len(canary.Ent) + len(canary.DL); n != 2+2*btoi(rotating) {
			t.Errorf("rotating %v: canary trusts %d keys", rotating, n)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestRelayConfigs(t *testing.T) {
	needSh(t)
	cp := mustRender(t, "cp")
	cloud, err := server.LoadConfig(filepath.Join(cp, "cloud.yaml"), server.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r1", "r2"} {
		out := mustRender(t, id)
		c, err := relayserver.LoadConfig(filepath.Join(out, "relay.yaml"))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if c.ID != id || c.SelfHosted() || !slices.Equal(c.Zones, []string{cloud.TenantZone}) {
			t.Errorf("%s: id %q, zones %v", id, c.ID, c.Zones)
		}
		if c.DenylistURL != cloud.PublicURL+"/v1/denylist" || !strings.HasPrefix(c.MirrorURL, "https://") {
			t.Errorf("%s: deny list %q, mirror %q", id, c.DenylistURL, c.MirrorURL)
		}
		// The control plane names this relay by the same control name.
		i := slices.IndexFunc(cloud.Relays, func(r server.Relay) bool { return r.ID == id })
		if i < 0 || cloud.Relays[i].URL != "wss://"+c.ControlHostname+"/v1/tunnel" {
			t.Errorf("%s: cloud.yaml names %+v, relay.yaml %q", id, cloud.Relays, c.ControlHostname)
		}
		kids := []string{}
		for _, k := range c.IssuerKeys {
			kids = append(kids, k.Kid)
		}
		if !slices.Contains(kids, cloud.Keys.Entitlement) || !slices.Contains(kids, cloud.Keys.DenyList) {
			t.Errorf("%s: issuer keys %v don't include the kids that sign (%s, %s)", id, kids, cloud.Keys.Entitlement, cloud.Keys.DenyList)
		}
		if c.Abuse.NotifyCommand != "/usr/local/lib/mirrin/relay-notify" {
			t.Errorf("%s: abuse notices go nowhere", id)
		}
	}
}

func TestCloudConfig(t *testing.T) {
	needSh(t)
	out := mustRender(t, "cp")
	c, err := server.LoadConfig(filepath.Join(out, "cloud.yaml"), server.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(false); err != nil {
		t.Fatal(err)
	}
	if _, err := dns.HandleCAA("", c.IODEF); err != nil {
		t.Fatal(err)
	}
	if p, ok := keys.Purpose(c.Keys.Entitlement); !ok || p != keys.Entitlement {
		t.Errorf("keys.entitlement %q", c.Keys.Entitlement)
	}
	if p, ok := keys.Purpose(c.Keys.DenyList); !ok || p != keys.DenyList {
		t.Errorf("keys.denylist %q", c.Keys.DenyList)
	}
	if c.Keys.Dir != "$CREDENTIALS_DIRECTORY" {
		t.Errorf("keys.dir %q: the signing keys must come from systemd credentials", c.Keys.Dir)
	}
	if c.Billing.Provider != "paddle" || c.DNS.Provider != "route53" || c.Storage.Provider != "r2" {
		t.Errorf("providers: %q %q %q", c.Billing.Provider, c.DNS.Provider, c.Storage.Provider)
	}
	// Every relay has an IPv4 and an IPv6 address.
	for _, r := range c.Relays {
		var v4, v6 bool
		for _, ip := range r.IPs {
			a := netip.MustParseAddr(ip)
			v4, v6 = v4 || a.Is4(), v6 || a.Is6()
		}
		if !v4 || !v6 {
			t.Errorf("relay %s: %v", r.ID, r.IPs)
		}
	}

	// Every secret cloud.yaml names is a credential of the unit, and the
	// unit loads the two signing keys.
	unit := parseUnit(t, filepath.Join(out, "mirrin-cloud.service"))
	creds := unit.creds()
	for _, env := range []string{c.Billing.Paddle.APIKeyEnv, c.Billing.Paddle.WebhookSecretEnv, c.DNS.Route53.AccessKeyEnv,
		c.DNS.Route53.SecretKeyEnv, c.Storage.R2.AccessKeyEnv, c.Storage.R2.SecretKeyEnv} {
		if !slices.Contains(creds, env) {
			t.Errorf("mirrin-cloud.service doesn't load %s", env)
		}
	}
	for _, kid := range []string{c.Keys.Entitlement, c.Keys.DenyList} {
		if !slices.Contains(creds, kid+".pem") {
			t.Errorf("mirrin-cloud.service doesn't load %s.pem", kid)
		}
	}

	// Litestream replicates the database mirrin-cloud writes, with the
	// credentials its unit loads.
	var ls struct {
		Addr string `yaml:"addr"`
		DBs  []struct {
			Path     string `yaml:"path"`
			Replicas []struct {
				Type, Bucket, Endpoint string
				AccessKeyID            string `yaml:"access-key-id"`
				SecretAccessKey        string `yaml:"secret-access-key"`
			} `yaml:"replicas"`
		} `yaml:"dbs"`
	}
	if err := yaml.Unmarshal([]byte(read(t, filepath.Join(out, "litestream.yml"))), &ls); err != nil {
		t.Fatal(err)
	}
	if len(ls.DBs) != 1 || ls.DBs[0].Path != c.DataDir+"/cloud.db" || len(ls.DBs[0].Replicas) != 1 {
		t.Fatalf("litestream.yml: %+v", ls)
	}
	rep := ls.DBs[0].Replicas[0]
	lsCreds := parseUnit(t, filepath.Join(out, "litestream.service")).creds()
	for _, v := range []string{rep.AccessKeyID, rep.SecretAccessKey} {
		if !strings.HasPrefix(v, "$") || !slices.Contains(lsCreds, v[1:]) {
			t.Errorf("litestream.yml reads %q, which litestream.service doesn't load", v)
		}
	}
	if rep.Type != "s3" || rep.Bucket == c.Storage.R2.Bucket {
		t.Errorf("the replica must be its own bucket, not the backups': %+v", rep)
	}

	// Caddy proxies to mirrin-cloud's listener and keeps no access log.
	caddy := read(t, filepath.Join(out, "Caddyfile"))
	if !strings.Contains(caddy, "reverse_proxy "+c.Listen) {
		t.Errorf("Caddyfile doesn't proxy to %s", c.Listen)
	}
	if regexp.MustCompile(`(?m)^\s*log\b`).MatchString(caddy) {
		t.Error("Caddyfile keeps an access log, with client addresses")
	}
}

// unitFile is a systemd unit as key → values.
type unitFile map[string][]string

func parseUnit(t *testing.T, path string) unitFile {
	t.Helper()
	u := unitFile{}
	sc := bufio.NewScanner(strings.NewReader(read(t, path)))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || l[0] == '#' || l[0] == ';' || l[0] == '[' {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			t.Fatalf("%s: %q", path, l)
		}
		u[k] = append(u[k], v)
	}
	return u
}

// creds are the names of the credentials a unit loads.
func (u unitFile) creds() []string {
	var out []string
	for _, v := range u["LoadCredentialEncrypted"] {
		name, _, _ := strings.Cut(v, ":")
		out = append(out, name)
	}
	return out
}

func TestUnitsAreHardened(t *testing.T) {
	needSh(t)
	units := map[string]string{}
	for _, h := range []string{"r1", "cp", "probe-b"} {
		out := mustRender(t, h)
		files, _ := filepath.Glob(filepath.Join(out, "*.service"))
		for _, f := range files {
			units[h+"/"+filepath.Base(f)] = f
		}
	}
	if len(units) != 5 {
		t.Fatalf("units: %v", units)
	}
	want := map[string]string{
		"NoNewPrivileges": "yes", "ProtectSystem": "strict", "ProtectHome": "yes", "PrivateTmp": "yes",
		"PrivateDevices": "yes", "ProtectKernelTunables": "yes", "ProtectKernelModules": "yes",
		"ProtectControlGroups": "yes", "RestrictNamespaces": "yes", "RestrictSUIDSGID": "yes",
		"LockPersonality": "yes", "MemoryDenyWriteExecute": "yes", "SystemCallArchitectures": "native",
		"RestrictAddressFamilies": "AF_INET AF_INET6 AF_UNIX",
	}
	for name, path := range units {
		u := parseUnit(t, path)
		for k, v := range want {
			if !slices.Contains(u[k], v) {
				t.Errorf("%s: %s=%s is missing", name, k, v)
			}
		}
		if len(u["SystemCallFilter"]) == 0 || len(u["CapabilityBoundingSet"]) == 0 {
			t.Errorf("%s: no system call filter or capability bound", name)
		}
		if len(u["User"]) == 0 && !slices.Contains(u["DynamicUser"], "yes") {
			t.Errorf("%s: runs as root", name)
		}
		// Secrets never sit in the unit or in a plain environment file.
		if len(u["EnvironmentFile"]) > 0 || len(u["Environment"]) > 0 || len(u["LoadCredential"]) > 0 {
			t.Errorf("%s: secrets must be LoadCredentialEncrypted", name)
		}
		for _, v := range u["LoadCredentialEncrypted"] {
			n, p, _ := strings.Cut(v, ":")
			if p != "/etc/credstore.encrypted/"+n {
				t.Errorf("%s: credential %q is not /etc/credstore.encrypted/NAME", name, v)
			}
		}
		for _, v := range u["ExecStart"] {
			if !strings.HasPrefix(v, "/") {
				t.Errorf("%s: ExecStart %q is not an absolute path", name, v)
			}
		}
	}
}

// install.sh refuses to start until every credential the units and
// drop-ins load exists.
func TestInstallChecksEveryCredential(t *testing.T) {
	needSh(t)
	for _, h := range []string{"r1", "cp", "probe-b"} {
		out := mustRender(t, h)
		checked := map[string]bool{}
		for _, m := range regexp.MustCompile(`(?s)need_creds ((?:[^\n]*\\\n)*[^\n]*)`).FindAllStringSubmatch(read(t, filepath.Join(out, "install.sh")), -1) {
			for _, f := range strings.Fields(strings.ReplaceAll(m[1], "\\", " ")) {
				checked[f] = true
			}
		}
		files, _ := filepath.Glob(filepath.Join(out, "*.service"))
		confs, _ := filepath.Glob(filepath.Join(out, "*.conf"))
		for _, f := range append(files, confs...) {
			for _, c := range parseUnit(t, f).creds() {
				if !checked[c] {
					t.Errorf("%s: install.sh doesn't check for %s (%s)", h, c, filepath.Base(f))
				}
			}
		}
	}
}

func TestDNSFiles(t *testing.T) {
	needSh(t)
	out := mustRender(t, "dns")
	cp := mustRender(t, "cp")
	cloud, err := server.LoadConfig(filepath.Join(cp, "cloud.yaml"), server.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	// The apex: no CA may issue for the zone or any wildcard in it.
	var apex struct {
		Changes []struct {
			Action            string
			ResourceRecordSet struct {
				Name, Type      string
				ResourceRecords []struct{ Value string }
			}
		}
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(out, "tenant-apex.json"))), &apex); err != nil {
		t.Fatal(err)
	}
	if len(apex.Changes) != 1 {
		t.Fatalf("tenant-apex.json: %+v", apex)
	}
	rs := apex.Changes[0].ResourceRecordSet
	var vals []string
	for _, r := range rs.ResourceRecords {
		vals = append(vals, r.Value)
	}
	if rs.Name != cloud.TenantZone+"." || rs.Type != "CAA" || !slices.Contains(vals, `0 issue ";"`) || !slices.Contains(vals, `0 issuewild ";"`) {
		t.Fatalf("apex CAA: %+v", rs)
	}

	// The control plane's IAM policy writes only A, AAAA and CAA below
	// the apex, so a stolen key can't touch the apex CAA or add records of
	// other types.
	var pol struct {
		Statement []struct {
			Action    []string
			Resource  string
			Condition map[string]map[string][]string
		}
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(out, "route53-policy.json"))), &pol); err != nil {
		t.Fatal(err)
	}
	var write bool
	for _, s := range pol.Statement {
		if !strings.HasSuffix(s.Resource, "/Z0123456789TEST") {
			t.Errorf("policy resource %q", s.Resource)
		}
		if slices.Contains(s.Action, "route53:ChangeResourceRecordSets") {
			write = true
			names := s.Condition["ForAllValues:StringLike"]["route53:ChangeResourceRecordSetsNormalizedRecordNames"]
			types := s.Condition["ForAllValues:StringEquals"]["route53:ChangeResourceRecordSetsRecordTypes"]
			if !slices.Equal(names, []string{"*." + cloud.TenantZone}) || !slices.Equal(types, []string{"A", "AAAA", "CAA"}) {
				t.Errorf("write condition: names %v types %v", names, types)
			}
		}
	}
	if !write {
		t.Error("the policy can't write handle records")
	}

	// The operator zone points each relay's control name at the addresses
	// the control plane puts in every handle's records.
	zone := read(t, filepath.Join(out, "operator-zone.zone"))
	for _, r := range cloud.Relays {
		host := strings.TrimSuffix(strings.TrimPrefix(r.URL, "wss://"), "/v1/tunnel")
		label := strings.TrimSuffix(host, ".mirrin.app")
		for _, ip := range r.IPs {
			typ := "A"
			if netip.MustParseAddr(ip).Is6() {
				typ = "AAAA"
			}
			if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(label) + `\s+\d+\s+` + typ + `\s+` + regexp.QuoteMeta(ip) + `$`).MatchString(zone) {
				t.Errorf("operator zone: no %s %s %s", label, typ, ip)
			}
		}
	}
	if strings.Contains(zone, "*.") {
		t.Error("the operator zone has a wildcard")
	}
}

func TestScriptsParse(t *testing.T) {
	needSh(t)
	var scripts []string
	for _, h := range hosts {
		out := mustRender(t, h)
		s, _ := filepath.Glob(filepath.Join(out, "*.sh"))
		scripts = append(scripts, s...)
	}
	s, _ := filepath.Glob("*.sh")
	scripts = append(scripts, s...)
	for _, f := range scripts {
		if b, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", f, err, b)
		}
	}
	if len(scripts) < 8 {
		t.Fatalf("only %d scripts", len(scripts))
	}
}

// with-credentials exports the credentials named like environment
// variables, and nothing else.
func TestWithCredentials(t *testing.T) {
	needSh(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "PADDLE_API_KEY"), []byte("pdl_secret\n"), 0o400)
	os.WriteFile(filepath.Join(dir, "ent-2026a.pem"), []byte("-----BEGIN"), 0o400)
	os.WriteFile(filepath.Join(dir, "lower_case"), []byte("no"), 0o400)
	cmd := exec.Command("sh", "with-credentials.sh", "env")
	cmd.Env = []string{"CREDENTIALS_DIRECTORY=" + dir, "PATH=" + os.Getenv("PATH")}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, b)
	}
	env := string(b)
	if !strings.Contains(env, "PADDLE_API_KEY=pdl_secret\n") {
		t.Errorf("not exported:\n%s", env)
	}
	if strings.Contains(env, "BEGIN") || strings.Contains(env, "lower_case") {
		t.Errorf("exported what it shouldn't:\n%s", env)
	}
}

// relay-notify checks its arguments before it sends anything: the handle
// goes into a mail.
func TestRelayNotifyRejectsBadInput(t *testing.T) {
	needSh(t)
	out := mustRender(t, "r1")
	for _, args := range [][]string{{"evil\nBcc: x@example.org", "300"}, {"ok-handle", "3; rm"}, {"", "1"}} {
		cmd := exec.Command("sh", append([]string{filepath.Join(out, "relay-notify.sh")}, args...)...)
		cmd.Env = []string{"PATH=/nonexistent"} // no curl: nothing may be sent
		b, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
			t.Errorf("%q: %v %s", args, err, b)
		}
	}
}
