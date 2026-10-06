package cloudtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// Contract checks the control plane at baseURL against docs/cloud-api.md
// from the outside, the way the daemon sees it: through cloud.Client, plus
// hand-made requests for what the client would never send. The server must
// run in dev mode, where a GET of a checkout_url pays for that link and the
// /dev/ routes (paid-through, supersede, revoke) stand in for the merchant of
// record, a recovery elsewhere and an admin, so every answer the daemon acts
// on can be produced and checked. Each run makes new devices and new
// handles, so a long-lived dev server can be checked again and again. It
// passes against Fake, and cloud/ runs it against its own --dev server.
//
// Backup storage and recovery (docs/cloud-api.md §5.7 and §5.8) are not in
// this suite, and Fake doesn't serve them: they need a storage provider and
// a recovery-key signature path that only the real server has. They are
// tested against the real server, in --dev mode with its disk storage
// (StartDev): cloud/internal/server/backup_test.go from the inside, and
// internal/cloud/backuptarget and cmd/mirrin from the daemon's side.
func Contract(t *testing.T, baseURL string) {
	ctx := t.Context()
	keys, dlKeys := fetchKeys(t, baseURL)

	t.Run("public", func(t *testing.T) {
		var v struct {
			API string `json:"api"`
		}
		if st := getJSON(t, baseURL+"/v1/version", &v); st != 200 || v.API != "v1" {
			t.Fatalf("GET /v1/version: %d %+v, want 200 and api v1", st, v)
		}
		for kid := range keys {
			if _, ok := dlKeys[kid]; ok || !strings.HasPrefix(kid, "ent-") {
				t.Errorf("entitlement kid %q is not an ent-* kid of its own", kid)
			}
		}
		for kid := range dlKeys {
			if !strings.HasPrefix(kid, "dl-") {
				t.Errorf("deny-list kid %q is not a dl-* kid", kid)
			}
		}
		if _, err := denyList(t, baseURL, dlKeys); err != nil {
			t.Fatalf("GET /v1/denylist: %v", err)
		}
	})

	t.Run("device routes refuse unsigned requests", func(t *testing.T) {
		for _, rt := range deviceRoutes {
			resp := send(t, rawRequest(t, rt.method, baseURL+rt.path, rt.body))
			if resp.status != http.StatusUnauthorized || resp.code == "" {
				t.Errorf("%s %s unsigned: %d %q, want 401 with an error code", rt.method, rt.path, resp.status, resp.code)
			}
		}
	})

	// One linked machine for the rest of the suite.
	dataDir := t.TempDir()
	c, err := cloud.New(dataDir, baseURL, keys)
	if err != nil {
		t.Fatal(err)
	}
	handle := "ct-" + randomHex(6)
	var ls cloud.LinkStart
	t.Run("link", func(t *testing.T) {
		ls, err = c.StartLink(ctx, handle, "")
		if err != nil {
			t.Fatal(err)
		}
		if st, ent, err := c.PollLink(ctx, ls.ID); err != nil || st != cloud.LinkPending || ent != "" {
			t.Fatalf("poll before paying: %q %q %v, want pending", st, ent, err)
		}
		// Another device, known to the server by its own link, may not
		// see this one.
		other := newKey(t)
		if resp := linkStart(t, baseURL, other, ""); resp.status != http.StatusOK {
			t.Fatalf("link/start for a second device: %d %q", resp.status, resp.code)
		}
		if resp := send(t, signed(t, other, http.MethodGet, baseURL+"/v1/link/"+ls.ID, nil, time.Now())); resp.status != http.StatusNotFound {
			t.Errorf("another device polling the link: %d, want 404", resp.status)
		}
		// The handle is held while the link is pending.
		if resp := linkStart(t, baseURL, newKey(t), handle); resp.status != http.StatusConflict || resp.code != "handle_taken" {
			t.Errorf("second link for %s: %d %q, want 409 handle_taken", handle, resp.status, resp.code)
		}
		pay(t, ls.CheckoutURL)
		st, ent := pollUntilActive(t, c, ls.ID)
		if st != cloud.LinkActive || ent == "" {
			t.Fatalf("poll after paying: %q, want active with an entitlement", st)
		}
		cl, err := entitle.Verify(ent, keys, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := c.PublicKey()
		if k, _ := cl.Key(); !k.Equal(pub) {
			t.Error("entitlement cnf is not the device key")
		}
		if cl.Handle != handle || len(cl.Hosts) == 0 || !strings.HasPrefix(cl.Hosts[0], handle+".") || cl.Gen < 1 || !cl.Has("reach") || len(cl.Relays) == 0 {
			t.Errorf("entitlement claims: %+v", cl)
		}
		if d := cl.Exp.Sub(cl.Iat); d > 35*24*time.Hour+time.Minute {
			t.Errorf("entitlement lasts %v, more than 35 days", d)
		}
		if _, kind := c.State().Current(time.Now()); kind != cloud.Active {
			t.Errorf("state after link: %v, want active", kind)
		}
	})
	if t.Failed() {
		return
	}
	key := deviceKey(t, dataDir)

	t.Run("signatures", func(t *testing.T) {
		now := time.Now()
		req := signed(t, key, http.MethodGet, baseURL+"/v1/me", nil, now)
		again := req.Clone(ctx)
		if resp := send(t, req); resp.status != http.StatusOK {
			t.Fatalf("signed GET /v1/me: %d", resp.status)
		}
		if resp := send(t, again); resp.status != http.StatusConflict || resp.code != "replay" {
			t.Errorf("replayed request: %d %q, want 409 replay", resp.status, resp.code)
		}
		stale := signed(t, key, http.MethodGet, baseURL+"/v1/me", nil, now.Add(-15*time.Minute))
		if resp := send(t, stale); resp.status != http.StatusUnauthorized {
			t.Errorf("request signed 15 minutes ago: %d, want 401", resp.status)
		}
		moved := signed(t, key, http.MethodGet, baseURL+"/v1/me", nil, now)
		moved.URL.Path = "/v1/billing/portal"
		moved.Method = http.MethodPost
		if resp := send(t, moved); resp.status != http.StatusUnauthorized {
			t.Errorf("request sent to another path than signed: %d, want 401", resp.status)
		}
		swapped := signed(t, key, http.MethodPut, baseURL+"/v1/acme-account", []byte(`{"uri":"https://acme.example/acct/1"}`), now)
		swapped.Body = io.NopCloser(strings.NewReader(`{"uri":"https://acme.example/acct/2"}`))
		if resp := send(t, swapped); resp.status != http.StatusUnauthorized {
			t.Errorf("request with a body other than signed: %d, want 401", resp.status)
		}
	})

	t.Run("link/start refusals", func(t *testing.T) {
		a, b := newKey(t), newKey(t)
		// device_pub must be the key that signed: a new key naming another
		// is refused, and so is one the server knows from a pending link.
		if resp := linkStartBody(t, baseURL, a, map[string]string{"device_pub": pubOf(b)}); resp.status != http.StatusUnauthorized || resp.code != "unauthorized" {
			t.Errorf("new key naming another: %d %q, want 401 unauthorized", resp.status, resp.code)
		}
		if resp := linkStart(t, baseURL, a, ""); resp.status != http.StatusOK {
			t.Fatalf("link/start: %d %q", resp.status, resp.code)
		}
		if resp := linkStartBody(t, baseURL, a, map[string]string{"device_pub": pubOf(b)}); resp.status != http.StatusUnauthorized || resp.code != "unauthorized" {
			t.Errorf("pending key naming another: %d %q, want 401 unauthorized", resp.status, resp.code)
		}
		if resp := linkStartBody(t, baseURL, a, map[string]string{}); resp.status != http.StatusUnauthorized || resp.code != "unauthorized" {
			t.Errorf("pending key naming none: %d %q, want 401 unauthorized", resp.status, resp.code)
		}
		if resp := linkStartBody(t, baseURL, a, map[string]string{"device_pub": pubOf(a), "email": "x@example.com"}); resp.status != http.StatusBadRequest || resp.code != "bad_request" {
			t.Errorf("link/start with a member it does not take: %d %q, want 400 bad_request", resp.status, resp.code)
		}
		if resp := linkStart(t, baseURL, key, ""); resp.status != http.StatusConflict || resp.code != "already_linked" {
			t.Errorf("link/start from a linked device: %d %q, want 409 already_linked", resp.status, resp.code)
		}
		if resp := send(t, signed(t, key, http.MethodGet, baseURL+"/v1/link/lk_nosuchlink", nil, time.Now())); resp.status != http.StatusNotFound || resp.code != "not_found" {
			t.Errorf("polling a link that does not exist: %d %q, want 404 not_found", resp.status, resp.code)
		}
		big := []byte(`{"uri":"https://acme.example/acct/` + strings.Repeat("x", 64<<10) + `"}`)
		if resp := send(t, signed(t, key, http.MethodPut, baseURL+"/v1/acme-account", big, time.Now())); resp.status != http.StatusRequestEntityTooLarge || resp.code != "too_large" {
			t.Errorf("a body over 64 KiB: %d %q, want 413 too_large", resp.status, resp.code)
		}
	})

	t.Run("refresh", func(t *testing.T) {
		cl, err := c.Refresh(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if cl.Handle != handle || cl.Gen < 1 {
			t.Errorf("refreshed claims: %+v", cl)
		}
	})

	t.Run("handles and acme accounts are checked", func(t *testing.T) {
		for _, h := range []string{"Bad", "a", "double--hyphen", "-edge", strings.Repeat("x", 33), "www"} {
			if resp := linkStart(t, baseURL, newKey(t), h); resp.status != http.StatusBadRequest || resp.code != "bad_handle" {
				t.Errorf("handle %q: %d %q, want 400 bad_handle", h, resp.status, resp.code)
			}
		}
		if err := c.SetACMEAccount(ctx, "https://acme-staging-v02.api.letsencrypt.org/acme/acct/123"); err != nil {
			t.Fatal(err)
		}
		for _, u := range []string{"http://acme.example/acct/1", `https://acme.example/acct/1";x`, "https://acme.example/acct/1;validationmethods=http-01", ""} {
			body, _ := json.Marshal(map[string]string{"uri": u})
			if resp := send(t, signed(t, key, http.MethodPut, baseURL+"/v1/acme-account", body, time.Now())); resp.status != http.StatusBadRequest || resp.code != "bad_acme_account" {
				t.Errorf("acme account %q: %d %q, want 400 bad_acme_account", u, resp.status, resp.code)
			}
		}
	})

	t.Run("me and billing", func(t *testing.T) {
		me, err := c.Me(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(me, &doc); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"account", "devices", "handles"} {
			if _, ok := doc[k]; !ok {
				t.Errorf("/v1/me has no %q", k)
			}
		}
		if !bytes.Contains(me, []byte(entitle.EncodeKey(key.Public().(ed25519.PublicKey)))) || !bytes.Contains(me, []byte(handle)) {
			t.Error("/v1/me does not show this device's key and handle")
		}
		if u, err := c.BillingPortal(ctx); err != nil || u == "" {
			t.Errorf("billing portal: %q %v", u, err)
		}
	})

	t.Run("delete and restore", func(t *testing.T) {
		before := time.Now()
		if err := c.DeleteAccount(ctx); err != nil {
			t.Fatal(err)
		}
		info, _, _ := c.State().Info()
		if d := info.DeleteAt.Sub(before); d < 6*24*time.Hour || d > 8*24*time.Hour {
			t.Errorf("deletion due in %v, want seven days", d)
		}
		if err := c.RestoreAccount(ctx); err != nil {
			t.Fatal(err)
		}
		if resp := send(t, signed(t, key, http.MethodPost, baseURL+"/v1/account/restore", nil, time.Now())); resp.status != http.StatusConflict || resp.code != "not_deleting" {
			t.Errorf("restoring an account not being deleted: %d %q, want 409 not_deleting", resp.status, resp.code)
		}
	})

	// A second machine lives through every answer a refresh can bring: a
	// lapse, a supersede and a revocation.
	t.Run("refresh answers", func(t *testing.T) {
		dir := t.TempDir()
		c2, err := cloud.New(dir, baseURL, keys)
		if err != nil {
			t.Fatal(err)
		}
		h2 := "ct-" + randomHex(6)
		ls, err := c2.StartLink(ctx, h2, "")
		if err != nil {
			t.Fatal(err)
		}
		pay(t, ls.CheckoutURL)
		if st, _ := pollUntilActive(t, c2, ls.ID); st != cloud.LinkActive {
			t.Fatalf("second machine: %q, want active", st)
		}
		key2 := deviceKey(t, dir)
		refresh := func() answer {
			return send(t, signed(t, key2, http.MethodPost, baseURL+"/v1/entitlement/refresh", nil, time.Now()))
		}

		lapsed := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		dev(t, baseURL, "/dev/paid-through", map[string]any{"handle": h2, "paid_through": lapsed}, nil)
		var b402 struct {
			PaidThrough time.Time `json:"paid_through"`
		}
		if resp := refresh(); resp.status != http.StatusPaymentRequired || resp.code != "lapsed" || json.Unmarshal(resp.body, &b402) != nil || !b402.PaidThrough.Equal(lapsed) {
			t.Errorf("refresh after a lapse: %d %s, want 402 lapsed with paid_through %s", resp.status, resp.body, lapsed.Format(time.RFC3339))
		}
		if _, err := c2.Refresh(ctx); !errors.Is(err, cloud.ErrLapsed) {
			t.Errorf("client refresh after a lapse: %v, want ErrLapsed", err)
		}
		dev(t, baseURL, "/dev/paid-through", map[string]any{"handle": h2, "paid_through": time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)}, nil)
		if _, err := c2.Refresh(ctx); err != nil {
			t.Errorf("client refresh once paid again: %v", err)
		}

		var sup struct {
			Gen int64 `json:"gen"`
		}
		dev(t, baseURL, "/dev/supersede", map[string]any{"handle": h2}, &sup)
		var b409 struct {
			Gen int64     `json:"gen"`
			At  time.Time `json:"at"`
		}
		if resp := refresh(); resp.status != http.StatusConflict || resp.code != "superseded" || json.Unmarshal(resp.body, &b409) != nil || b409.Gen != sup.Gen || b409.Gen < 2 || b409.At.IsZero() {
			t.Errorf("refresh after a supersede: %d %s, want 409 superseded at generation %d with at", resp.status, resp.body, sup.Gen)
		}
		var se *cloud.SupersededError
		if _, err := c2.Refresh(ctx); !errors.As(err, &se) || se.Gen != sup.Gen {
			t.Errorf("client refresh after a supersede: %v, want SupersededError at generation %d", err, sup.Gen)
		}
		if _, kind := c2.State().Current(time.Now()); kind != cloud.Superseded {
			t.Errorf("state after a supersede: %v, want superseded", kind)
		}

		dev(t, baseURL, "/dev/revoke", map[string]any{"device_pub": pubOf(key2)}, nil)
		if resp := refresh(); resp.status != http.StatusForbidden || resp.code != "revoked" {
			t.Errorf("refresh after a revocation: %d %q, want 403 revoked", resp.status, resp.code)
		}
		if resp := send(t, signed(t, key2, http.MethodGet, baseURL+"/v1/link/"+ls.ID, nil, time.Now())); resp.status != http.StatusForbidden || resp.code != "revoked" {
			t.Errorf("polling the link after a revocation: %d %q, want 403 revoked", resp.status, resp.code)
		}
		if l, err := denyList(t, baseURL, dlKeys); err != nil || !l.DeniesKey(key2.Public().(ed25519.PublicKey)) {
			t.Errorf("the deny list does not name the revoked key (%v)", err)
		}
	})

	t.Run("unlink", func(t *testing.T) {
		if err := c.Unlink(ctx); err != nil {
			t.Fatal(err)
		}
		if c.State().Linked() {
			t.Error("still linked after unlink")
		}
		resp := send(t, signed(t, key, http.MethodPost, baseURL+"/v1/entitlement/refresh", nil, time.Now()))
		if resp.status != http.StatusForbidden && resp.status != http.StatusUnauthorized {
			t.Errorf("refresh with an unlinked key: %d, want 403 or 401", resp.status)
		}
		l, err := denyList(t, baseURL, dlKeys)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range l.Keys {
			found = found || e.Value == entitle.EncodeKey(key.Public().(ed25519.PublicKey))
		}
		if !found {
			t.Error("the deny list does not name the unlinked key")
		}
	})
}

// deviceRoutes are every route that needs a device signature.
var deviceRoutes = []struct {
	method, path string
	body         []byte
}{
	{http.MethodPost, "/v1/link/start", []byte(`{"device_pub":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"}`)},
	{http.MethodGet, "/v1/link/lk_contract", nil},
	{http.MethodPost, "/v1/entitlement/refresh", nil},
	{http.MethodPut, "/v1/acme-account", []byte(`{"uri":"https://acme.example/acct/1"}`)},
	{http.MethodGet, "/v1/me", nil},
	{http.MethodPost, "/v1/billing/portal", nil},
	{http.MethodDelete, "/v1/device", nil},
	{http.MethodDelete, "/v1/account", nil},
	{http.MethodPost, "/v1/account/restore", nil},
}

// answer is a response as the suite looks at it.
type answer struct {
	status int
	code   string // the "error" member, if any
	body   []byte
}

func send(t *testing.T, req *http.Request) answer {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Error string `json:"error"`
	}
	if resp.StatusCode >= 400 {
		if err := json.Unmarshal(b, &e); err != nil {
			t.Errorf("%s %s: %d answer is not {\"error\",\"message\"}: %q", req.Method, req.URL.Path, resp.StatusCode, b)
		}
	}
	return answer{status: resp.StatusCode, code: e.Error, body: b}
}

func rawRequest(t *testing.T, method, u string, body []byte) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// signed is a request signed with key as of now.
func signed(t *testing.T, key ed25519.PrivateKey, method, u string, body []byte, now time.Time) *http.Request {
	t.Helper()
	req := rawRequest(t, method, u, body)
	if err := httpsig.Sign(req, httpsig.KeyID(key.Public().(ed25519.PublicKey)), key, now); err != nil {
		t.Fatal(err)
	}
	return req
}

// linkStart is a signed link/start that skips the client's own checks.
func linkStart(t *testing.T, base string, key ed25519.PrivateKey, handle string) answer {
	t.Helper()
	in := map[string]string{"device_pub": pubOf(key)}
	if handle != "" {
		in["handle"] = handle
	}
	return linkStartBody(t, base, key, in)
}

// linkStartBody is a link/start with body in, signed by key.
func linkStartBody(t *testing.T, base string, key ed25519.PrivateKey, in map[string]string) answer {
	t.Helper()
	body, _ := json.Marshal(in)
	return send(t, signed(t, key, http.MethodPost, base+"/v1/link/start", body, time.Now()))
}

func pubOf(key ed25519.PrivateKey) string {
	return entitle.EncodeKey(key.Public().(ed25519.PublicKey))
}

// dev drives one of a dev server's control routes, and reads its answer
// into out if out is not nil.
func dev(t *testing.T, base, path string, in, out any) {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	resp := send(t, rawRequest(t, http.MethodPost, base+path, body))
	if resp.status < 200 || resp.status > 299 {
		t.Fatalf("POST %s (dev mode): %d %q", path, resp.status, resp.code)
	}
	if out != nil {
		if err := json.Unmarshal(resp.body, out); err != nil {
			t.Fatalf("POST %s (dev mode): %v", path, err)
		}
	}
}

func newKey(t *testing.T) ed25519.PrivateKey {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// deviceKey reads the device key from its documented file,
// data/cloud/device.key, for requests the client would not make.
func deviceKey(t *testing.T, dataDir string) ed25519.PrivateKey {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir, "cloud", "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		t.Fatal("device.key is not a PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	priv, ok := k.(ed25519.PrivateKey)
	if err != nil || !ok {
		t.Fatalf("device.key is not an Ed25519 PKCS #8 key: %v", err)
	}
	return priv
}

func getJSON(t *testing.T, u string, out any) int {
	t.Helper()
	resp := send(t, rawRequest(t, http.MethodGet, u, nil))
	if resp.status == http.StatusOK {
		if err := json.Unmarshal(resp.body, out); err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
	}
	return resp.status
}

// fetchKeys reads /v1/keys: the entitlement and deny-list keys by kid.
func fetchKeys(t *testing.T, base string) (ent, dl map[string]ed25519.PublicKey) {
	t.Helper()
	var doc struct {
		Entitlement map[string]string `json:"entitlement"`
		DenyList    map[string]string `json:"denylist"`
	}
	if st := getJSON(t, base+"/v1/keys", &doc); st != http.StatusOK {
		t.Fatalf("GET /v1/keys: %d", st)
	}
	parse := func(m map[string]string) map[string]ed25519.PublicKey {
		out := map[string]ed25519.PublicKey{}
		for kid, s := range m {
			k, err := entitle.ParseKey(s)
			if err != nil {
				t.Fatalf("/v1/keys: %s: %v", kid, err)
			}
			out[kid] = k
		}
		return out
	}
	ent, dl = parse(doc.Entitlement), parse(doc.DenyList)
	if len(ent) == 0 || len(dl) == 0 {
		t.Fatal("/v1/keys lists no entitlement or no deny-list key")
	}
	return ent, dl
}

func denyList(t *testing.T, base string, keys map[string]ed25519.PublicKey) (entitle.DenyList, error) {
	t.Helper()
	resp := send(t, rawRequest(t, http.MethodGet, base+"/v1/denylist", nil))
	if resp.status != http.StatusOK {
		return entitle.DenyList{}, errors.New(http.StatusText(resp.status))
	}
	return entitle.VerifyDenyList(strings.TrimSpace(string(resp.body)), keys, time.Now())
}

// pay opens the checkout page, which pays in dev mode.
func pay(t *testing.T, checkout string) {
	t.Helper()
	resp := send(t, rawRequest(t, http.MethodGet, checkout, nil))
	if resp.status < 200 || resp.status > 299 {
		t.Fatalf("dev checkout %s: %d", checkout, resp.status)
	}
}

// pollUntilActive polls until the link is active, for up to ten seconds: a
// dev server may take the payment through its webhook path.
func pollUntilActive(t *testing.T, c *cloud.Client, id string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		st, ent, err := c.PollLink(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if st != cloud.LinkPending {
			return st, ent
		}
		select {
		case <-ctx.Done():
			return st, ent
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
