package reach

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr/acmetest"
)

// The paid journey against the real control plane: `mirrin-cloud serve
// --dev` (the nested cloud/ module, built here from source, on its fake
// merchant of record and fake DNS), two mirrin-relays in this process and
// the fake CA. The handle's CAA record is whatever the control plane wrote
// for the account, and the CA issues only to the account it names; a plain
// Go TLS client then reaches the daemon at the handle and sees the
// daemon's own key.
func TestCloudReachAgainstTheDevControlPlane(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs mirrin-cloud")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to build mirrin-cloud with")
	}
	modDir, err := filepath.Abs(filepath.Join("..", "..", "cloud"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(modDir, "go.mod")); err != nil {
		t.Skip("no cloud/ module here")
	}
	bin := filepath.Join(t.TempDir(), "mirrin-cloud"+exeSuffix())
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/mirrin-cloud")
	build.Dir = modDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mirrin-cloud: %v\n%s", err, out)
	}

	// The relays come first: the control plane names them in every
	// entitlement. Their deny list comes from the control plane through a
	// TLS front, as relays fetch it only over https.
	rca := newRelayCA(t)
	var origin string
	front := httptest.NewTLSServer(&httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		u, _ := url.Parse(origin)
		pr.SetURL(u)
	}})
	t.Cleanup(front.Close)
	ent := map[string]ed25519.PublicKey{}
	dl := map[string]ed25519.PublicKey{}
	var relays []*hostedRelay
	// The relays need the control plane's keys and the control plane needs
	// the relays' addresses: reserve the relays' ports first.
	var addrs []string
	for range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addrs = append(addrs, ln.Addr().String())
		ln.Close()
	}
	var yaml strings.Builder
	yaml.WriteString("relays:\n")
	for i, a := range addrs {
		_, port, _ := net.SplitHostPort(a)
		fmt.Fprintf(&yaml, "  - {id: r%d, url: \"wss://localhost:%s/v1/tunnel\", ips: [127.0.0.1]}\n", i+1, port)
	}
	cfgPath := filepath.Join(t.TempDir(), "cloud.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serve := exec.CommandContext(ctx, bin, "serve", "--dev", "--config", cfgPath, "--listen", "127.0.0.1:0", "--data", t.TempDir())
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
	case origin = <-found:
	case <-time.After(30 * time.Second):
		t.Fatal("mirrin-cloud --dev didn't start")
	}
	var keys struct {
		Entitlement map[string]string `json:"entitlement"`
		DenyList    map[string]string `json:"denylist"`
	}
	res, err := http.Get(origin + "/v1/keys")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(res.Body).Decode(&keys)
	res.Body.Close()
	for kid, s := range keys.Entitlement {
		k, err := entitle.ParseKey(s)
		if err != nil {
			t.Fatal(err)
		}
		ent[kid] = k
	}
	for kid, s := range keys.DenyList {
		if k, err := entitle.ParseKey(s); err == nil {
			dl[kid] = k
		}
	}
	if len(ent) == 0 || len(dl) == 0 {
		t.Fatalf("/v1/keys: %+v", keys)
	}
	for i, a := range addrs {
		h := startHostedRelay(t, fmt.Sprintf("r%d", i+1), rca, ent, dl, front)
		h.stop()
		h.start(a)
		relays = append(relays, h)
	}

	// Link as `mirrin cloud link` does: the checkout page is the dev
	// merchant of record's, and opening it pays.
	dir := t.TempDir()
	c, err := cloud.New(dir, origin, ent)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := c.StartLink(ctx, cloudHandle, "")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := http.Get(ls.CheckoutURL); err != nil || res.StatusCode != 200 {
		t.Fatalf("checkout: %v %v", res, err)
	}
	if st, _, err := c.PollLink(ctx, ls.ID); err != nil || st != cloud.LinkActive {
		t.Fatalf("link: %q %v", st, err)
	}
	cl, _ := c.State().Current(time.Now())
	if len(cl.Relays) != 2 || cl.Hosts[0] != cloudHost {
		t.Fatalf("entitlement %+v", cl)
	}

	// The CA checks the account the control plane has for the handle,
	// which is what it writes into the handle's CAA record.
	pinned := func() (acct string, written bool) {
		me, err := c.Me(ctx)
		if err != nil {
			return "", false
		}
		var doc struct {
			Account struct {
				ACMEAccount string `json:"acme_account"`
			} `json:"account"`
			Handles []struct {
				DNSPending bool `json:"dns_pending"`
			} `json:"handles"`
		}
		json.Unmarshal(me, &doc)
		return doc.Account.ACMEAccount, len(doc.Handles) == 1 && !doc.Handles[0].DNSPending
	}
	ca := acmetest.New()
	t.Cleanup(ca.Close)
	ca.Dial = relays[1].dial
	ca.CAA = func(name, account string) error {
		if got, written := pinned(); got != account || !written {
			return fmt.Errorf("CAA names %q (written %v), not %q", got, written, account)
		}
		return nil
	}

	srv := api.New("127.0.0.1:0", "dev-rig-master", streamer{}).WithDevices(devices.NewMemory()).WithScreen(fakeScreen{bus: events.New()})
	e, err := StartCloud(ctx, c, nil, Deps{
		Server: srv, DataDir: dir, Health: health.New(),
		CTSources:  []certwatch.CTSource{&fakeCT{}},
		CAA:        func(context.Context, string) ([]certwatch.CAA, error) { return nil, nil },
		ACMEClient: ca.Client(),
		RelayRoots: rca.pool,
		Reach:      config.Reach{Mode: "cloud", ACMEDirectory: ca.Directory()},
		PinSettle:  time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the handle's certificate", func() bool { return e.ACME.Leaf() != nil })
	if len(ca.Issued()) != 1 {
		t.Fatalf("%d certificates; the first order should pass CAA", len(ca.Issued()))
	}
	if got, _ := pinned(); got != e.ACME.AccountURI() {
		t.Fatalf("the control plane pins %q", got)
	}

	// A plain Go TLS client reaches the daemon at the handle through each
	// relay, and the key it sees is the daemon's own.
	for _, r := range relays {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", r.addr, &tls.Config{ServerName: cloudHost, RootCAs: ca.Roots})
		if err != nil {
			t.Fatalf("via %s: %v", r.id, err)
		}
		leaf := conn.ConnectionState().PeerCertificates[0]
		if tlsmgr.SPKIPin(leaf) != e.Pins()[0] || !bytes.Equal(leaf.Raw, e.ACME.Leaf().Raw) {
			conn.Close()
			t.Fatalf("via %s: not the daemon's key", r.id)
		}
		fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", cloudHost)
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil || res.StatusCode != 200 {
			conn.Close()
			t.Fatalf("via %s: %v %v", r.id, res, err)
		}
		conn.Close()
	}
}

// exeSuffix is what Windows needs at the end of a program's name to run it.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
