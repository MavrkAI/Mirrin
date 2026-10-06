package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/billing"
	"github.com/MavrkAI/Mirrin/cloud/internal/dns"
	"github.com/MavrkAI/Mirrin/cloud/internal/keys"
	"github.com/MavrkAI/Mirrin/cloud/internal/storage"
	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// clock is a settable fake clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// env is a dev server on a loopback port with its fakes.
type env struct {
	s     *Server
	url   string
	dns   *dns.Fake
	fake  *billing.Fake
	store *store.Store
	// objects is the backup storage, a disk fake served at /storage/.
	objects *storage.Fake
	clock   *clock // nil when the server runs on real time
}

// newEnv starts a --dev server on 127.0.0.1:0 with a real SQLite store in
// a temporary directory. With fakeClock, the server, the fake merchant of
// record and signatures in these tests share a settable clock.
func newEnv(t *testing.T, fakeClock bool, tweak func(*Config)) *env {
	t.Helper()
	return newEnvWith(t, fakeClock, tweak, nil)
}

// newEnvWith is newEnv with prep run on the server before it serves.
func newEnvWith(t *testing.T, fakeClock bool, tweak func(*Config), prep func(*Server)) *env {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	origin := "http://" + ts.Listener.Addr().String()
	cfg := DevConfig()
	cfg.PublicURL = origin
	if tweak != nil {
		tweak(&cfg)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "cloud.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	objects, err := storage.NewFake(t.TempDir(), origin+"/storage")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{url: origin, dns: dns.NewFake(), fake: billing.NewFake(origin), store: st, objects: objects}
	opts := Options{Store: st, Billing: e.fake, DevBilling: e.fake, DNS: e.dns, Keys: keys.Dev(), Dev: true,
		Storage: objects, DevStorage: objects}
	if fakeClock {
		e.clock = &clock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
		opts.Now, e.fake.Now, objects.Now = e.clock.Now, e.clock.Now, e.clock.Now
	}
	e.s, err = New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if prep != nil {
		prep(e.s)
	}
	ts.Config.Handler = e.s.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return e
}

func (e *env) now() time.Time {
	if e.clock != nil {
		return e.clock.Now()
	}
	return time.Now()
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pubOf(k ed25519.PrivateKey) string { return entitle.EncodeKey(k.Public().(ed25519.PublicKey)) }

// resp is an answer as the tests look at it.
type resp struct {
	status int
	code   string
	body   []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("%s: %v", r.body, err)
	}
}

func (e *env) request(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.url+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// signed is a request signed by key at the env's time.
func (e *env) signed(t *testing.T, key ed25519.PrivateKey, method, path string, body []byte) *http.Request {
	t.Helper()
	req := e.request(t, method, path, body)
	if err := httpsig.Sign(req, httpsig.KeyID(key.Public().(ed25519.PublicKey)), key, e.now()); err != nil {
		t.Fatal(err)
	}
	return req
}

func send(t *testing.T, req *http.Request) resp {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(b, &e)
	return resp{status: r.StatusCode, code: e.Error, body: b}
}

// link starts a link for key with handle and ACME account, pays for it
// through the dev checkout, and returns the link id and entitlement.
func (e *env) link(t *testing.T, key ed25519.PrivateKey, handle, acme string) (string, string) {
	t.Helper()
	in := map[string]string{"device_pub": pubOf(key)}
	if handle != "" {
		in["handle"] = handle
	}
	if acme != "" {
		in["acme_account"] = acme
	}
	b, _ := json.Marshal(in)
	r := send(t, e.signed(t, key, "POST", "/v1/link/start", b))
	if r.status != 200 {
		t.Fatalf("link/start: %d %s", r.status, r.body)
	}
	var ls struct {
		ID          string `json:"id"`
		CheckoutURL string `json:"checkout_url"`
	}
	r.json(t, &ls)
	req, _ := http.NewRequest("GET", ls.CheckoutURL, nil)
	if r := send(t, req); r.status != 200 {
		t.Fatalf("checkout: %d %s", r.status, r.body)
	}
	r = send(t, e.signed(t, key, "GET", "/v1/link/"+ls.ID, nil))
	var poll struct {
		Status      string `json:"status"`
		Entitlement string `json:"entitlement"`
	}
	r.json(t, &poll)
	if r.status != 200 || poll.Status != "active" || poll.Entitlement == "" {
		t.Fatalf("poll after paying: %d %s", r.status, r.body)
	}
	return ls.ID, poll.Entitlement
}

// startLink starts a link for key, unpaid, and returns its id.
func (e *env) startLink(t *testing.T, key ed25519.PrivateKey, handle string) string {
	t.Helper()
	in := map[string]string{"device_pub": pubOf(key)}
	if handle != "" {
		in["handle"] = handle
	}
	b, _ := json.Marshal(in)
	r := send(t, e.signed(t, key, "POST", "/v1/link/start", b))
	if r.status != 200 {
		t.Fatalf("link/start: %d %s", r.status, r.body)
	}
	var ls struct {
		ID string `json:"id"`
	}
	r.json(t, &ls)
	return ls.ID
}

// poll polls link id as key and returns the answer and its status.
func (e *env) poll(t *testing.T, key ed25519.PrivateKey, id string) (resp, string) {
	t.Helper()
	r := send(t, e.signed(t, key, "GET", "/v1/link/"+id, nil))
	var p struct {
		Status string `json:"status"`
	}
	json.Unmarshal(r.body, &p)
	return r, p.Status
}

// dev posts to a dev route, as the contract suite does.
func (e *env) dev(t *testing.T, path, body string) resp {
	t.Helper()
	return send(t, e.request(t, "POST", path, []byte(body)))
}

// fetchKeys reads /v1/keys the way a relay or a person checking would.
func (e *env) fetchKeys(t *testing.T) (ent, dl map[string]ed25519.PublicKey) {
	t.Helper()
	r := send(t, e.request(t, "GET", "/v1/keys", nil))
	var doc struct {
		Entitlement map[string]string `json:"entitlement"`
		DenyList    map[string]string `json:"denylist"`
	}
	r.json(t, &doc)
	parse := func(m map[string]string) map[string]ed25519.PublicKey {
		out := map[string]ed25519.PublicKey{}
		for kid, s := range m {
			k, err := entitle.ParseKey(s)
			if err != nil {
				t.Fatal(err)
			}
			out[kid] = k
		}
		return out
	}
	return parse(doc.Entitlement), parse(doc.DenyList)
}

func (e *env) denyList(t *testing.T) entitle.DenyList {
	t.Helper()
	_, dl := e.fetchKeys(t)
	r := send(t, e.request(t, "GET", "/v1/denylist", nil))
	l, err := entitle.VerifyDenyList(string(bytes.TrimSpace(r.body)), dl, e.now())
	if err != nil {
		t.Fatal(err)
	}
	return l
}
