package cloudtest

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// DevServer is the real control plane, `mirrin-cloud serve --dev`, built
// from the nested cloud/ module and run on a loopback port with its fakes:
// the merchant of record, DNS, and backup storage in a folder under
// DataDir/storage served at Origin/storage/. Nothing in it reaches a real
// service.
type DevServer struct {
	Origin  string
	DataDir string
	// Ent and DL are its entitlement and deny-list keys, from /v1/keys.
	Ent, DL map[string]ed25519.PublicKey
}

// StartDev builds and starts a dev control plane for the test, with
// configYAML ("" for none) as its --config. It skips the test with -short,
// or where there is no go command or cloud/ module to build from.
func StartDev(t testing.TB, configYAML string) *DevServer {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs mirrin-cloud")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to build mirrin-cloud with")
	}
	_, here, _, _ := runtime.Caller(0)
	modDir := filepath.Join(filepath.Dir(here), "..", "..", "..", "cloud")
	if _, err := os.Stat(filepath.Join(modDir, "go.mod")); err != nil {
		t.Skip("no cloud/ module here")
	}
	bin := filepath.Join(t.TempDir(), "mirrin-cloud")
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/mirrin-cloud")
	build.Dir = modDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mirrin-cloud: %v\n%s", err, out)
	}
	d := &DevServer{DataDir: t.TempDir(), Ent: map[string]ed25519.PublicKey{}, DL: map[string]ed25519.PublicKey{}}
	args := []string{"serve", "--dev", "--listen", "127.0.0.1:0", "--data", d.DataDir}
	if configYAML != "" {
		p := filepath.Join(t.TempDir(), "cloud.yaml")
		if err := os.WriteFile(p, []byte(configYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--config", p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serve := exec.CommandContext(ctx, bin, args...)
	stderr, err := serve.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); serve.Wait() })
	originRE := regexp.MustCompile(`--dev on (http://127\.0\.0\.1:\d+)`)
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if m := originRE.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case d.Origin = <-found:
	case <-time.After(60 * time.Second):
		t.Fatal("mirrin-cloud --dev didn't start")
	}
	var keys struct {
		Entitlement map[string]string `json:"entitlement"`
		DenyList    map[string]string `json:"denylist"`
	}
	res, err := http.Get(d.Origin + "/v1/keys")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(res.Body).Decode(&keys)
	res.Body.Close()
	for kid, s := range keys.Entitlement {
		if k, err := entitle.ParseKey(s); err == nil {
			d.Ent[kid] = k
		}
	}
	for kid, s := range keys.DenyList {
		if k, err := entitle.ParseKey(s); err == nil {
			d.DL[kid] = k
		}
	}
	if len(d.Ent) == 0 || len(d.DL) == 0 {
		t.Fatalf("/v1/keys: %+v", keys)
	}
	return d
}

// Link links a new machine whose data folder is dataDir, paying through the
// dev checkout, and returns its client.
func (d *DevServer) Link(t testing.TB, dataDir string) *cloud.Client {
	t.Helper()
	c, err := cloud.New(dataDir, d.Origin, d.Ent)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ls, err := c.StartLink(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(ls.CheckoutURL)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("checkout: %v %v", res, err)
	}
	res.Body.Close()
	if st, _, err := c.PollLink(ctx, ls.ID); err != nil || st != cloud.LinkActive {
		t.Fatalf("link: %q %v", st, err)
	}
	return c
}

// Dev posts body to one of the dev routes, such as /dev/paid-through.
func (d *DevServer) Dev(t testing.TB, path, body string) int {
	t.Helper()
	res, err := http.Post(d.Origin+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// DenyList fetches and verifies the deny list, as a relay's poll does.
func (d *DevServer) DenyList(t testing.TB) entitle.DenyList {
	t.Helper()
	res, err := http.Get(d.Origin + "/v1/denylist")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	sc.Scan()
	l, err := entitle.VerifyDenyList(sc.Text(), d.DL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return l
}
