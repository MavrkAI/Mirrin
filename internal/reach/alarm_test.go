package reach

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/health"
)

// pairedDevices writes a registry with two phones: "recent", paired
// through the relay and about to be used there, with a passkey enrolled
// inside the alarm window; and "idle", paired and last used long before,
// with an old passkey. It returns the store (re-opened from disk, as the
// daemon would) and both tokens.
func pairedDevices(t *testing.T, dir string, window time.Time) (*devices.Store, string, string) {
	t.Helper()
	path := devices.Path(dir)
	s, err := devices.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := window.Add(-48 * time.Hour)
	s.SetClock(func() time.Time { return old })
	recent, recentTok, err := s.Add("Recent iPhone", devices.KindPWA, devices.DefaultScopes(devices.KindPWA), "relay", "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	idle, idleTok, err := s.Add("Idle iPad", devices.KindPWA, devices.DefaultScopes(devices.KindPWA), "relay", "198.51.100.21")
	if err != nil {
		t.Fatal(err)
	}
	// Enrol passkeys (WP-06 will do this through step-up).
	b, _ := os.ReadFile(path)
	var f map[string]any
	json.Unmarshal(b, &f)
	for _, d := range f["devices"].([]any) {
		m := d.(map[string]any)
		switch m["id"] {
		case recent.ID:
			m["passkeys"] = []map[string]any{{"id": "pk-new", "public_key": "x", "created": window.Add(30 * time.Minute)}}
		case idle.ID:
			m["passkeys"] = []map[string]any{{"id": "pk-old", "public_key": "y", "created": old}}
		}
	}
	b, _ = json.Marshal(f)
	os.WriteFile(path, b, 0o600)
	s, err = devices.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, recentTok, idleTok
}

func passkeys(t *testing.T, dir, id string) []any {
	b, _ := os.ReadFile(devices.Path(dir))
	var f struct {
		Devices []map[string]any `json:"devices"`
	}
	json.Unmarshal(b, &f)
	for _, d := range f.Devices {
		if d["id"] == id {
			pk, _ := d["passkeys"].([]any)
			return pk
		}
	}
	t.Fatalf("no device %s", id)
	return nil
}

func TestPlaybookAfterSyntheticFinding(t *testing.T) {
	window := time.Now()
	var recentTok, idleTok string
	r := startRig(t, func(r *rig) { r.store, recentTok, idleTok = pairedDevices(t, r.dir, window) })
	recent, _ := r.store.Authenticate(recentTok)
	idle, _ := r.store.Authenticate(idleTok)

	// Before the alarm: the recent phone uses the public name; approvals
	// from other devices work.
	if res := r.do("GET", "/screen", recentTok, ""); res.StatusCode != 200 {
		t.Fatalf("status before %d %s", res.StatusCode, body(res))
	} else {
		body(res)
	}
	if res := r.do("POST", "/approvals/1/approve", recentTok, ""); res.StatusCode != 200 {
		t.Fatalf("approval before %d %s", res.StatusCode, body(res))
	} else {
		body(res)
	}
	if code, reply := r.message(recentTok, "yes 1"); code != 200 || reply != "free" {
		t.Fatalf("chat before %d %q", code, reply)
	}
	sw := r.do("GET", "/sw.js", "", "")
	swBefore := body(sw)

	// CT shows a wildcard certificate for a key this machine never had.
	// It is valid from just after this machine's checkpoint, which is
	// before the recent phone's last use.
	_, checkpoint := r.e.ACME.Known()
	nb := checkpoint.Add(time.Millisecond)
	if lu, _ := r.store.Get(recent.ID); lu.LastSeen.Before(nb) {
		t.Fatalf("setup: last use %v before %v", lu.LastSeen, nb)
	}
	rogue := certwatch.Issuance{Source: "fake CT", ID: "rogue", DNSNames: []string{"*.mirrin.test"}, SPKI: "cm9ndWUta2V5LXBpbi1yb2d1ZS1rZXktcGluLXJvZ3VlLWs", NotBefore: nb, NotAfter: nb.Add(90 * 24 * time.Hour), Issuer: "Rogue DV CA"}
	r.ct.add(rogue)
	// This poll reports it, unless the watcher's own poll (it runs in the
	// background) got there first; either way it is reported once, and only
	// a critical finding starts the playbook.
	fs := r.e.Watcher.Check(context.Background())
	if len(fs) > 1 || len(fs) == 1 && (fs[0].Severity != certwatch.Critical || fs[0].Issuance == nil || fs[0].Issuance.ID != "rogue") {
		t.Fatalf("findings %+v", fs)
	}
	waitFor(t, 10*time.Second, "the playbook", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.notes) > 0
	})
	if fs := r.e.Watcher.Check(context.Background()); len(fs) != 0 {
		t.Fatalf("reported again: %+v", fs)
	}
	// Whichever poll saw it, the playbook ran once, for the rogue key.
	if st := r.e.Alarm.State(); !st.Active || len(st.Handled) != 1 || !strings.HasSuffix(st.Handled[0], ":"+rogue.SPKI) || st.Epoch != 1 {
		t.Fatalf("alarm %+v", st)
	}

	// 1. Remote approvals: 423, and the page says what to do.
	res := r.do("POST", "/approvals/1/approve", idleTok, "")
	msg := body(res)
	if res.StatusCode != http.StatusLocked || !strings.Contains(msg, "approvals_paused") || !strings.Contains(msg, "mirrin reach alarm clear") {
		t.Fatalf("approval during alarm: %d %s", res.StatusCode, msg)
	}
	// That was the iPad's first contact since the alarm (step 4).
	if res.Header.Get("Clear-Site-Data") != `"cache", "storage"` {
		t.Fatalf("idle first contact: %q", res.Header.Get("Clear-Site-Data"))
	}
	// A yes in chat from that device reaches the daemon marked as held, so
	// it decides nothing either (daemon/reachrelay.go has the refusal).
	if code, reply := r.message(idleTok, "yes 1"); code != 200 || reply != "held" {
		t.Fatalf("chat during alarm %d %q", code, reply)
	}
	// 2. The phone that used the name in the window: 401 and a re-pair page.
	res = r.do("GET", "/ui", recentTok, "text/html")
	page := body(res)
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "<html") || !strings.Contains(page, "mirrin pair") {
		t.Fatalf("rotated device: %d %s", res.StatusCode, page)
	}
	if d, ok := r.store.Get(recent.ID); !ok || !d.Revoked() {
		t.Fatal("recent phone still has its key")
	}
	if d, _ := r.store.Get(idle.ID); d.Revoked() {
		t.Fatal("idle iPad was cut off though it wasn't used in the window")
	}
	// 3. The passkey enrolled in the window is gone; the old one stays.
	if pk := passkeys(t, r.dir, recent.ID); len(pk) != 0 {
		t.Fatalf("window passkey kept: %v", pk)
	}
	if pk := passkeys(t, r.dir, idle.ID); len(pk) != 1 {
		t.Fatalf("old passkey: %v", pk)
	}
	// 4. Clear-Site-Data on each device's next contact, once (above); a
	// new service worker.
	res = r.do("GET", "/screen", idleTok, "")
	if res.StatusCode != 200 || res.Header.Get("Clear-Site-Data") != "" {
		t.Fatal("Clear-Site-Data sent twice")
	}
	body(res)
	req, _ := http.NewRequest("GET", "https://"+tenant+"/screen", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-mirrin", Value: recentTok})
	res, err := r.phone().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Header.Get("Clear-Site-Data") == "" {
		t.Fatal("a rotated device's stale cookie got no Clear-Site-Data")
	}
	body(res)
	if swAfter := body(r.do("GET", "/sw.js", "", "")); swAfter == swBefore {
		t.Fatal("service worker version unchanged")
	}
	// 5. Push, banner, owner message, health Fail.
	r.mu.Lock()
	pushes, banners, notes := len(r.pushes), len(r.banners), r.notes[0]
	r.mu.Unlock()
	if pushes != 1 || banners != 1 || !strings.Contains(notes, "paused approvals") || !strings.Contains(notes, "signed out 1 device") {
		t.Fatalf("push %d banner %d note %q", pushes, banners, notes)
	}
	rep := r.health.Run(context.Background())
	var alarm health.Result
	for _, x := range rep.Results {
		if x.Name == "alarm" {
			alarm = x
		}
	}
	if alarm.State != health.Fail {
		t.Fatalf("health %+v", rep.Results)
	}

	// The same finding again does nothing more.
	if err := Playbook(context.Background(), fs[0], r.deps); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	again := len(r.pushes)
	r.mu.Unlock()
	if again != 1 {
		t.Fatal("playbook ran twice for one finding")
	}

	// The owner clears it on loopback; remote approvals work again.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.api.Serve(ctx, ln, api.LoopbackOnly, "loopback")
	creq, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/reach/alarm/clear", nil)
	creq.Header.Set("Authorization", "Bearer rig-master-token")
	cres, err := http.DefaultClient.Do(creq)
	if err != nil {
		t.Fatal(err)
	}
	if cres.StatusCode != 200 {
		t.Fatalf("clear %d %s", cres.StatusCode, body(cres))
	}
	body(cres)
	if res := r.do("POST", "/approvals/1/approve", idleTok, ""); res.StatusCode != 200 {
		t.Fatalf("approval after clearing %d", res.StatusCode)
	} else {
		body(res)
	}
	if code, reply := r.message(idleTok, "yes 1"); code != 200 || reply != "free" {
		t.Fatalf("chat after clearing %d %q", code, reply)
	}
	// The rogue certificate stays in CT for its life; once cleared, the
	// certificate watch warns about it instead of failing.
	r.e.Watcher.Check(context.Background())
	for _, x := range r.health.Run(context.Background()).Results {
		if x.Name == "certwatch" && x.State != health.Warn || x.Name == "alarm" && x.State != health.OK {
			t.Fatalf("after clearing, %s is %v: %s", x.Name, x.State, x.Detail)
		}
	}
	// The alarm routes are not on the public name.
	if res := r.do("POST", "/reach/alarm/clear", idleTok, ""); res.StatusCode != 404 {
		t.Fatalf("remote clear %d", res.StatusCode)
	} else {
		body(res)
	}
}

func TestAlarmState(t *testing.T) {
	dir := t.TempDir()
	a, err := OpenAlarm(dir)
	if err != nil || a.Active() || a.Epoch() != "" || a.ClearSite("") {
		t.Fatal("new alarm not quiet")
	}
	f := certwatch.Finding{Kind: certwatch.CAAMismatch, Severity: certwatch.Critical, Host: tenant, Detail: "x", CAA: []certwatch.CAA{{Tag: "issue", Value: "ca.example; accounturi=other"}}}
	if fresh, err := a.raise(f, time.Now()); !fresh || err != nil {
		t.Fatal(err)
	}
	if fresh, _ := a.raise(f, time.Now()); fresh {
		t.Fatal("raised twice")
	}
	a.owe([]string{"dev1"}, nil)
	b, _ := OpenAlarm(dir) // survives a restart
	if !b.Active() || b.Epoch() != "alarm-1" || !b.ClearSite("dev1") || b.ClearSite("dev1") || !b.ClearSite("") {
		t.Fatalf("reloaded %+v", b.State())
	}
	if err := b.Clear("test"); err != nil || b.Active() {
		t.Fatal("clear")
	}
	if st, _, _ := b.Health(context.Background()); st != health.OK {
		t.Fatal(st)
	}
	// A damaged file keeps approvals paused rather than forgetting.
	os.WriteFile(filepath.Join(dir, "reach", "alarm.json"), []byte("{nope"), 0o600)
	c, err := OpenAlarm(dir)
	if err != nil || !c.Active() {
		t.Fatal("damaged alarm file opened quiet")
	}
}

func TestPlaybookIgnoresWarnings(t *testing.T) {
	a, _ := OpenAlarm(t.TempDir())
	if err := Playbook(context.Background(), certwatch.Finding{Kind: certwatch.SourceDown, Severity: certwatch.Warning}, Deps{Alarm: a}); err != nil || a.Active() {
		t.Fatal("a warning ran the playbook")
	}
	if alarmText(certwatch.Finding{Kind: certwatch.Superseded, Host: tenant}) == "" {
		t.Fatal("text")
	}
}

type countingPasskeys struct{ since []time.Time }

func (c *countingPasskeys) RevokePasskeysSince(_ context.Context, t time.Time) (int, error) {
	c.since = append(c.since, t)
	return 0, nil
}

func TestPlaybookUsesPasskeyRevoker(t *testing.T) {
	a, _ := OpenAlarm(t.TempDir())
	pk := &countingPasskeys{}
	nb := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	now := nb.Add(5 * time.Hour)
	err := Playbook(context.Background(), certwatch.Finding{Kind: certwatch.UnknownIssuance, Severity: certwatch.Critical, Host: tenant, Issuance: &certwatch.Issuance{SPKI: "z"}, NotBefore: nb},
		Deps{Alarm: a, Passkeys: pk, Now: func() time.Time { return now }})
	if err != nil || len(pk.since) != 1 || !pk.since[0].Equal(nb) || !a.Active() {
		t.Fatalf("%v %v", err, pk.since)
	}
	var none PasskeyRevoker = StorePasskeys(nil)
	if n, err := none.RevokePasskeysSince(context.Background(), nb); n != 0 || err != nil {
		t.Fatal("StorePasskeys(nil)")
	}
}

// Regression (review of the certificate alarm): passkeys enrolled in the
// window went only with the devices the playbook rotated. A device it
// didn't rotate (its last use was recorded before the window, say) kept a
// passkey whoever held the rogue certificate may have enrolled.
func TestPlaybookRevokesWindowPasskeysOnDevicesItKeeps(t *testing.T) {
	dir := t.TempDir()
	path := devices.Path(dir)
	s, err := devices.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	nb := time.Now().Add(-time.Hour)
	s.SetClock(func() time.Time { return nb.Add(-48 * time.Hour) })
	dev, _, err := s.Add("Laptop", devices.KindCLI, nil, "loopback", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var f map[string]any
	json.Unmarshal(b, &f)
	for _, d := range f["devices"].([]any) {
		d.(map[string]any)["passkeys"] = []map[string]any{
			{"id": "pk-old", "public_key": "y", "created": nb.Add(-24 * time.Hour)},
			{"id": "pk-window", "public_key": "x", "created": nb.Add(10 * time.Minute)},
		}
	}
	b, _ = json.Marshal(f)
	os.WriteFile(path, b, 0o600)
	if s, err = devices.Open(path); err != nil {
		t.Fatal(err)
	}
	a, _ := OpenAlarm(dir)
	err = Playbook(context.Background(), certwatch.Finding{Kind: certwatch.UnknownIssuance, Severity: certwatch.Critical, Host: tenant, Issuance: &certwatch.Issuance{SPKI: "z"}, NotBefore: nb},
		Deps{Alarm: a, Devices: func() *devices.Store { return s }})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(dev.ID); d.Revoked() {
		t.Fatal("the laptop was rotated; this test needs one that isn't")
	}
	pk := passkeys(t, dir, dev.ID)
	if len(pk) != 1 || pk[0].(map[string]any)["id"] != "pk-old" {
		t.Fatalf("passkeys after the playbook: %v", pk)
	}
}
