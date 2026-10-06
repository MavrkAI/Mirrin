package cloud_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/cloud/cloudtest"
	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// The fake control plane passes the same contract the real one must.
func TestContractAgainstFake(t *testing.T) {
	cloudtest.Contract(t, cloudtest.NewFake(t).URL)
}

// link runs a whole link against f and returns the linked client.
func link(t *testing.T, f *cloudtest.Fake, dataDir string) *cloud.Client {
	t.Helper()
	c, err := cloud.New(dataDir, f.URL, f.Keys())
	if err != nil {
		t.Fatal(err)
	}
	ls, err := c.StartLink(t.Context(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Pay(ls.ID); err != nil {
		t.Fatal(err)
	}
	if st, _, err := c.PollLink(t.Context(), ls.ID); err != nil || st != cloud.LinkActive {
		t.Fatalf("poll: %q %v", st, err)
	}
	return c
}

// Linking: pending, pending, then active once paid, with an entitlement
// bound to this machine's key and every file private to the user.
func TestLinkAgainstTheFake(t *testing.T) {
	f := cloudtest.NewFake(t)
	dataDir := t.TempDir()
	c, err := cloud.New(dataDir, f.URL, f.Keys())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	ls, err := c.StartLink(ctx, "ember-otter-42", "https://acme-v02.api.letsencrypt.org/acme/acct/1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ls.CheckoutURL, f.URL+"/checkout/") {
		t.Errorf("checkout URL %q", ls.CheckoutURL)
	}
	if got, _, ok := c.PendingLink(); !ok || got != ls {
		t.Fatalf("pending link: %+v %v, want %+v", got, ok, ls)
	}
	for i := range 2 {
		st, ent, err := c.PollLink(ctx, ls.ID)
		if err != nil || st != cloud.LinkPending || ent != "" {
			t.Fatalf("poll %d: %q %q %v, want pending", i+1, st, ent, err)
		}
		if _, kind := c.State().Current(time.Now()); kind != cloud.None {
			t.Fatalf("state while pending: %v, want none", kind)
		}
	}
	pay(t, ls.CheckoutURL) // the user pays in the browser
	st, ent, err := c.PollLink(ctx, ls.ID)
	if err != nil || st != cloud.LinkActive {
		t.Fatalf("poll after paying: %q %v, want active", st, err)
	}
	cl, err := entitle.Verify(ent, f.Keys(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pub, err := c.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := cl.Key(); !k.Equal(pub) {
		t.Fatal("cnf is not the device key")
	}
	if cl.Handle != "ember-otter-42" || cl.Gen != 1 {
		t.Errorf("claims: handle %q gen %d", cl.Handle, cl.Gen)
	}
	got, kind := c.State().Current(time.Now())
	if kind != cloud.Active || got.Cnf != entitle.EncodeKey(pub) {
		t.Fatalf("state: %v, want active and bound here", kind)
	}
	if tok, _ := c.State().Entitlement(); tok != ent {
		t.Error("stored entitlement is not the one returned")
	}
	if _, _, ok := c.PendingLink(); ok {
		t.Error("the pending link outlived the link")
	}
	checkPrivate(t, filepath.Join(dataDir, "cloud"), "device.key", "link.json", "entitlement.paseto", "egress.jsonl")
}

// checkPrivate insists dir is 0700 and holds exactly files, each 0600.
func checkPrivate(t *testing.T, dir string, files ...string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != strings.Join(sorted(files), " ") {
		t.Errorf("%s holds %v, want %v", dir, names, sorted(files))
	}
	if runtime.GOOS == "windows" {
		return // no Unix modes
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("%s: mode %v, want 0700", dir, fi.Mode().Perm())
	}
	for _, f := range files {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600 (%v)", f, fi.Mode().Perm(), err)
		}
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func pay(t *testing.T, checkout string) {
	t.Helper()
	resp, err := http.Get(checkout)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("checkout: %d", resp.StatusCode)
	}
}

// A control plane that binds the entitlement to some other key is refused,
// and nothing is stored.
func TestTokenForAnotherKeyIsRefused(t *testing.T) {
	f := cloudtest.NewFake(t)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	f.Tamper(func(c *entitle.Claims) { c.Cnf = entitle.EncodeKey(other) })
	c, err := cloud.New(t.TempDir(), f.URL, f.Keys())
	if err != nil {
		t.Fatal(err)
	}
	ls, err := c.StartLink(t.Context(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.Pay(ls.ID)
	if _, _, err := c.PollLink(t.Context(), ls.ID); !errors.Is(err, cloud.ErrWrongKey) {
		t.Fatalf("poll: %v, want ErrWrongKey", err)
	}
	if c.State().Linked() {
		t.Fatal("a token for another key was stored")
	}
	if _, kind := c.State().Current(time.Now()); kind != cloud.None {
		t.Fatalf("state: %v, want none", kind)
	}
	// Nor is a token signed with a key this client does not trust.
	c2, _ := cloud.New(t.TempDir(), f.URL, map[string]ed25519.PublicKey{})
	f.Tamper(nil)
	ls, _ = c2.StartLink(t.Context(), "", "")
	f.Pay(ls.ID)
	if _, _, err := c2.PollLink(t.Context(), ls.ID); !errors.Is(err, entitle.ErrSignature) {
		t.Fatalf("poll with no trusted key: %v, want ErrSignature", err)
	}
}

// With a fake clock: Active until paid_through, Grace until exp, then
// Expired. Has is true only while Active or in Grace.
func TestStatesFollowTheClock(t *testing.T) {
	f := cloudtest.NewFake(t)
	t0 := time.Now().UTC().Truncate(time.Second)
	f.SetClock(func() time.Time { return t0 })
	c := link(t, f, t.TempDir())
	cl, _ := c.State().Current(t0)
	if !cl.PaidThrough.Equal(t0.Add(30*24*time.Hour)) || !cl.Exp.Equal(t0.Add(35*24*time.Hour)) {
		t.Fatalf("paid_through %v exp %v", cl.PaidThrough, cl.Exp)
	}
	for _, tc := range []struct {
		at   time.Duration
		want cloud.StateKind
	}{
		{0, cloud.Active},
		{29 * 24 * time.Hour, cloud.Active},
		{30*24*time.Hour - time.Second, cloud.Active},
		{30 * 24 * time.Hour, cloud.Grace},
		{35*24*time.Hour - time.Second, cloud.Grace},
		{35 * 24 * time.Hour, cloud.Expired},
		{400 * 24 * time.Hour, cloud.Expired},
	} {
		now := t0.Add(tc.at)
		if _, kind := c.State().Current(now); kind != tc.want {
			t.Errorf("at +%v: %v, want %v", tc.at, kind, tc.want)
		}
		want := tc.want == cloud.Active || tc.want == cloud.Grace
		for _, feat := range []string{"reach", "backup"} {
			if got := c.State().Has(now, feat); got != want {
				t.Errorf("at +%v: Has(%s) = %v, want %v", tc.at, feat, got, want)
			}
		}
		if c.State().Has(now, "wake") {
			t.Errorf("at +%v: Has(wake) for a feature not granted", tc.at)
		}
	}
}

// What a refresh can come back with, and what each leaves behind.
func TestRefreshOutcomes(t *testing.T) {
	f := cloudtest.NewFake(t)
	t0 := time.Now().UTC().Truncate(time.Second)
	clock := t0
	f.SetClock(func() time.Time { return clock })
	c := link(t, f, t.TempDir())
	c.Now = func() time.Time { return clock }
	ctx := t.Context()

	clock = t0.Add(24 * time.Hour)
	cl, err := c.Refresh(ctx)
	if err != nil || !cl.Iat.Equal(clock) {
		t.Fatalf("refresh: %v %v", cl.Iat, err)
	}

	// Lapsed: 402, and the entitlement lasts until it expires.
	clock = t0.Add(31 * 24 * time.Hour)
	if _, err := c.Refresh(ctx); !errors.Is(err, cloud.ErrLapsed) {
		t.Fatalf("refresh after lapse: %v, want ErrLapsed", err)
	}
	if _, kind := c.State().Current(clock); kind != cloud.Grace {
		t.Fatalf("after 402: %v, want grace", kind)
	}
	if info, _, _ := c.State().Info(); !info.CheckedAt.Equal(clock) {
		t.Errorf("402 not recorded: %v", info.CheckedAt)
	}

	// Paid again, then another machine takes the handle: 409 and standby.
	h, _ := c.State().Current(clock)
	f.SetPaidThrough(h.Handle, clock.Add(30*24*time.Hour))
	if _, err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	gen, _ := f.Supersede(h.Handle)
	_, err = c.Refresh(ctx)
	var sup *cloud.SupersededError
	if !errors.As(err, &sup) || sup.Gen != gen {
		t.Fatalf("refresh after supersede: %v, want SupersededError at gen %d", err, gen)
	}
	if _, kind := c.State().Current(clock); kind != cloud.Superseded || c.State().Has(clock, "reach") {
		t.Fatalf("after 409: %v, want superseded and no features", kind)
	}
}

func TestRevokedMachineExpires(t *testing.T) {
	f := cloudtest.NewFake(t)
	c := link(t, f, t.TempDir())
	pub, _ := c.PublicKey()
	f.Revoke(pub)
	if _, err := c.Refresh(t.Context()); !errors.Is(err, cloud.ErrRevoked) {
		t.Fatalf("refresh: %v, want ErrRevoked", err)
	}
	if _, kind := c.State().Current(time.Now()); kind != cloud.Expired || c.State().Has(time.Now(), "reach") {
		t.Fatalf("after 403: %v, want expired", kind)
	}
}

// A refresh may not move this machine back a generation or to another
// handle.
func TestRefreshRefusesRollback(t *testing.T) {
	f := cloudtest.NewFake(t)
	c := link(t, f, t.TempDir())
	before, _ := c.State().Entitlement()
	f.Tamper(func(cl *entitle.Claims) {
		cl.Handle, cl.Hosts = "someone-else", []string{"someone-else." + cloud.TenantZone}
	})
	if _, err := c.Refresh(t.Context()); err == nil || !strings.Contains(err.Error(), "this machine holds") {
		t.Fatalf("refresh to another handle: %v", err)
	}
	if after, _ := c.State().Entitlement(); after != before {
		t.Fatal("the foreign entitlement was stored")
	}
}

// Before a link there is nothing: no directory, no key, no request.
func TestInertUntilLinked(t *testing.T) {
	f := cloudtest.NewFake(t)
	dataDir := t.TempDir()
	if c, err := cloud.OpenLinked(dataDir); c != nil || err != nil {
		t.Fatalf("OpenLinked on a fresh machine: %v %v", c, err)
	}
	c, err := cloud.New(dataDir, f.URL, f.Keys())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"refresh": func() error { _, err := c.Refresh(ctx); return err },
		"me":      func() error { _, err := c.Me(ctx); return err },
		"billing": func() error { _, err := c.BillingPortal(ctx); return err },
		"acme":    func() error { return c.SetACMEAccount(ctx, "https://acme.example/acct/1") },
		"unlink":  func() error { return c.Unlink(ctx) },
		"delete":  func() error { return c.DeleteAccount(ctx) },
		"restore": func() error { return c.RestoreAccount(ctx) },
		"poll":    func() error { _, _, err := c.PollLink(ctx, "lk_x"); return err },
	} {
		if err := call(); !errors.Is(err, cloud.ErrNotLinked) {
			t.Errorf("%s before linking: %v, want ErrNotLinked", name, err)
		}
	}
	if _, kind := c.State().Current(time.Now()); kind != cloud.None {
		t.Errorf("state: %v, want none", kind)
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("%d requests before linking, want none", n)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "cloud")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("data/cloud exists before linking: %v", err)
	}
	if es, _, err := cloud.ReadLedger(dataDir); err != nil || len(es) != 0 {
		t.Errorf("ledger before linking: %v %v", es, err)
	}
}

// Every request is in the ledger, with sizes and paths but never a body.
func TestLedgerRecordsEveryRequestButNoBody(t *testing.T) {
	f := cloudtest.NewFake(t)
	dataDir := t.TempDir()
	c := link(t, f, dataDir)
	ctx := t.Context()
	if _, err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Me(ctx); err != nil {
		t.Fatal(err)
	}
	es, bad, err := cloud.ReadLedger(dataDir)
	if err != nil || bad != 0 {
		t.Fatal(err, bad)
	}
	reqs := f.Requests()
	if len(es) != len(reqs) {
		t.Fatalf("ledger has %d entries, the control plane saw %d requests", len(es), len(reqs))
	}
	host := strings.TrimPrefix(f.URL, "http://")
	for i, e := range es {
		if e.Method != reqs[i].Method || e.Path != reqs[i].Path || e.Status != reqs[i].Status || e.Host != host || e.At.IsZero() {
			t.Errorf("entry %d: %+v, request %+v", i, e, reqs[i])
		}
		if e.Path == "/v1/link/start" && e.ReqBytes == 0 || e.RespBytes == 0 {
			t.Errorf("entry %d has no sizes: %+v", i, e)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dataDir, "cloud", "egress.jsonl"))
	tok, _ := c.State().Entitlement()
	pub, _ := c.PublicKey()
	for _, secret := range []string{tok, entitle.EncodeKey(pub), `"entitlement":`, "checkout", "device_pub"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the ledger holds %q", secret[:min(len(secret), 20)])
		}
	}
}

// A request whose ledger cannot be written is never sent.
func TestNoLedgerNoRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relies on Unix directory modes")
	}
	f := cloudtest.NewFake(t)
	dataDir := t.TempDir()
	c := link(t, f, dataDir)
	before := len(f.Requests())
	ledger := filepath.Join(dataDir, "cloud", "egress.jsonl")
	os.Remove(ledger)
	if err := os.Mkdir(ledger, 0o700); err != nil { // a directory can't be appended to
		t.Fatal(err)
	}
	if _, err := c.Refresh(t.Context()); err == nil {
		t.Fatal("refresh succeeded without a ledger")
	}
	if n := len(f.Requests()); n != before {
		t.Fatalf("%d requests sent without a ledger", n-before)
	}
}

func TestUnlinkForgetsButKeepsTheLedger(t *testing.T) {
	f := cloudtest.NewFake(t)
	dataDir := t.TempDir()
	c := link(t, f, dataDir)
	if err := c.Unlink(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.State().Linked() {
		t.Fatal("still linked")
	}
	if _, err := c.PublicKey(); !errors.Is(err, cloud.ErrNotLinked) {
		t.Fatalf("device key survived unlink: %v", err)
	}
	checkPrivate(t, filepath.Join(dataDir, "cloud"), "egress.jsonl")
	if l, _ := cloud.OpenLinked(dataDir); l != nil {
		t.Fatal("OpenLinked after unlink")
	}
}

func TestAPIOrigin(t *testing.T) {
	for _, bad := range []string{
		"http://cloud.mirrin.app", "https://cloud.mirrin.app/v1", "https://u:p@cloud.mirrin.app",
		"https://cloud.mirrin.app?x", "https://cloud.mirrin.app#f", "ftp://cloud.mirrin.app", "cloud.mirrin.app", "",
		"http://10.0.0.1:8080",
	} {
		if _, err := cloud.New(t.TempDir(), bad, nil); err == nil {
			t.Errorf("New accepted API %q", bad)
		}
	}
	for in, want := range map[string]string{
		"https://Cloud.Mirrin.App:443": "https://cloud.mirrin.app",
		"https://cloud.mirrin.app/":    "https://cloud.mirrin.app",
		"http://127.0.0.1:8080":        "http://127.0.0.1:8080",
		"http://[::1]:9":               "http://[::1]:9",
		"http://localhost:7":           "http://localhost:7",
	} {
		c, err := cloud.New(t.TempDir(), in, nil)
		if err != nil || c.API() != want {
			t.Errorf("New(%q): %v %v, want %s", in, c, err, want)
		}
	}
}

// Answers are bounded, redirects are not followed, and bad ids and pages
// from the control plane are refused.
func TestHostileAnswers(t *testing.T) {
	type answer struct {
		status int
		body   string
		want   string // in the error
	}
	var cur answer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cur.status == http.StatusFound {
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
			return
		}
		w.WriteHeader(cur.status)
		w.Write([]byte(cur.body))
	}))
	defer srv.Close()
	for name, a := range map[string]answer{
		"huge":         {200, strings.Repeat(" ", 65<<10), "larger than"},
		"redirect":     {302, "", "HTTP 302"},
		"bad id":       {200, `{"id":"../../x","checkout_url":"http://127.0.0.1/c"}`, "bad link id"},
		"script page":  {200, `{"id":"lk_1","checkout_url":"javascript:alert(1)"}`, "not a web address"},
		"file page":    {200, `{"id":"lk_1","checkout_url":"file:///etc/passwd"}`, "not a web address"},
		"duplicate":    {200, `{"id":"lk_1","id":"lk_2","checkout_url":"http://127.0.0.1/c"}`, "malformed"},
		"trailing":     {200, `{"id":"lk_1","checkout_url":"http://127.0.0.1/c"} {}`, "malformed"},
		"error answer": {418, "\x1b[31mred", "HTTP 418"},
	} {
		t.Run(name, func(t *testing.T) {
			cur = a
			c, err := cloud.New(t.TempDir(), srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.StartLink(context.Background(), "", "")
			if err == nil || !strings.Contains(err.Error(), a.want) {
				t.Fatalf("got %v, want an error containing %q", err, a.want)
			}
			if strings.ContainsRune(err.Error(), 0x1b) {
				t.Error("control characters from the server reached the error")
			}
		})
	}
}

func TestDeviceKeyMustBePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	f := cloudtest.NewFake(t)
	dataDir := t.TempDir()
	c := link(t, f, dataDir)
	os.Chmod(filepath.Join(dataDir, "cloud", "device.key"), 0o644)
	if _, err := c.Refresh(t.Context()); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("refresh with a world-readable key: %v", err)
	}
	if _, kind := c.State().Current(time.Now()); kind != cloud.Expired {
		t.Fatalf("state with a world-readable key: %v, want expired", kind)
	}
}

func TestDeleteAndRestore(t *testing.T) {
	f := cloudtest.NewFake(t)
	c := link(t, f, t.TempDir())
	if err := c.DeleteAccount(t.Context()); err != nil {
		t.Fatal(err)
	}
	if info, _, _ := c.State().Info(); time.Until(info.DeleteAt) < 6*24*time.Hour {
		t.Fatalf("delete_at %v", info.DeleteAt)
	}
	if err := c.RestoreAccount(t.Context()); err != nil {
		t.Fatal(err)
	}
	if info, _, _ := c.State().Info(); !info.DeleteAt.IsZero() {
		t.Fatal("restore did not clear delete_at")
	}
}

func TestStartLinkChecksInputAndRefusesRelink(t *testing.T) {
	f := cloudtest.NewFake(t)
	c := link(t, f, t.TempDir())
	if _, err := c.StartLink(t.Context(), "", ""); err == nil || !strings.Contains(err.Error(), "already linked") {
		t.Fatalf("second link: %v", err)
	}
	c2, _ := cloud.New(t.TempDir(), f.URL, f.Keys())
	for _, h := range []string{"UPPER", "a", "-x-", "x--y", strings.Repeat("a", 33)} {
		if _, err := c2.StartLink(t.Context(), h, ""); err == nil {
			t.Errorf("handle %q accepted", h)
		}
	}
	for _, u := range []string{"http://acme/1", "https://acme/1;x", `https://acme/"`, "https://acme/1 2", "https://u@acme/1", "https://acme/1#f"} {
		if _, err := c2.StartLink(t.Context(), "", u); err == nil {
			t.Errorf("acme account %q accepted", u)
		}
	}
}
