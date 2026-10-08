package stepup_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/stepup"
	"github.com/MavrkAI/Mirrin/internal/stepup/stepuptest"
)

var rp = stepup.RP{ID: "twin.example.ts.net", Origin: "https://twin.example.ts.net"}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	t     *testing.T
	store *devices.Store
	v     *stepup.Verifier
	clock *clock
	heard []stepup.Enrolment
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := devices.Open(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{now: time.Now()}
	store.SetClock(c.Now)
	v := stepup.New(store)
	v.SetClock(c.Now)
	v.SetName("Mirrin")
	f := &fixture{t: t, store: store, v: v, clock: c}
	v.OnEnrol(func(e stepup.Enrolment) { f.heard = append(f.heard, e) })
	return f
}

// claimed pairs a phone through a pairing offer, as "Add your phone" does.
func (f *fixture) claimed(name string) devices.Device {
	f.t.Helper()
	o, err := f.store.NewOffer(devices.KindPWA, nil, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	d, _, err := f.store.Claim(o.ID, o.Secret, name, devices.KindPWA, "tailscale", "100.64.0.9")
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *fixture) enrol(dev devices.Device, g stepup.EnrolGrant, a *stepuptest.Authenticator) (devices.Passkey, error) {
	f.t.Helper()
	opts, sid, err := f.v.BeginRegistration(rp, dev, g)
	if err != nil {
		return devices.Passkey{}, err
	}
	cred, err := a.Create(opts)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.v.FinishRegistration(dev, sid, bytes.NewReader(cred))
}

// enrolled is a claimed phone with a passkey from the pairing window.
func (f *fixture) enrolled(name string) (devices.Device, *stepuptest.Authenticator) {
	f.t.Helper()
	dev := f.claimed(name)
	a := stepuptest.New(rp.Origin)
	g, err := f.v.Grant(dev, rp)
	if err != nil || g.Kind != stepup.GrantPairing {
		f.t.Fatalf("grant %+v %v", g, err)
	}
	if _, err := f.enrol(dev, g, a); err != nil {
		f.t.Fatal(err)
	}
	return dev, a
}

func (f *fixture) approve(dev devices.Device, a *stepuptest.Authenticator, id int64, decision string, input []byte) (string, []byte) {
	f.t.Helper()
	opts, sid, err := f.v.BeginApproval(rp, dev, id, decision, input)
	if err != nil {
		f.t.Fatal(err)
	}
	assertion, err := a.Get(opts)
	if err != nil {
		f.t.Fatal(err)
	}
	return sid, assertion
}

func TestApprovalChallengeIsBoundToEverything(t *testing.T) {
	var nonce [32]byte
	nonce[0] = 7
	input := []byte(`{"amount":"42.10"}`)
	// Written out by hand from the design (docs/cloud-design.md §8).
	h := sha256.New()
	h.Write([]byte("mirrin-approval-v1\x00"))
	h.Write(binary.BigEndian.AppendUint64(nil, 12))
	h.Write([]byte("approve"))
	in := sha256.Sum256(input)
	h.Write(in[:])
	h.Write(nonce[:])
	want := h.Sum(nil)
	if got := stepup.ApprovalChallenge(12, "approve", input, nonce); !bytes.Equal(got, want) {
		t.Fatalf("challenge %x, want %x", got, want)
	}
	base := stepup.ApprovalChallenge(12, "approve", input, nonce)
	other := nonce
	other[31] = 1
	for name, c := range map[string][]byte{
		"id":       stepup.ApprovalChallenge(13, "approve", input, nonce),
		"decision": stepup.ApprovalChallenge(12, "deny", input, nonce),
		"input":    stepup.ApprovalChallenge(12, "approve", []byte(`{"amount":"4210"}`), nonce),
		"nonce":    stepup.ApprovalChallenge(12, "approve", input, other),
	} {
		if bytes.Equal(c, base) {
			t.Errorf("changing the %s left the challenge the same", name)
		}
	}
}

func TestWhatNeedsAPasskey(t *testing.T) {
	for _, s := range []string{"", "off", "none", "dangerous", "DANGEROUS "} {
		if got := stepup.ParseLevel(s); got != stepup.Dangerous {
			t.Errorf("step_up %q is %q, want dangerous (never less)", s, got)
		}
	}
	for _, c := range []struct {
		level          stepup.Level
		risk, decision string
		want           bool
	}{
		{stepup.Dangerous, "dangerous", "approve", true},
		{stepup.Dangerous, "", "approve", true}, // unknown counts as dangerous
		{stepup.Dangerous, "write", "approve", false},
		{stepup.Dangerous, "read", "approve", false},
		{stepup.Dangerous, "dangerous", "deny", false},
		{stepup.Write, "write", "approve", true},
		{stepup.Write, "read", "approve", false},
		{stepup.Write, "write", "deny", false},
		{stepup.All, "read", "deny", true},
	} {
		if got := stepup.Required(c.level, c.risk, c.decision); got != c.want {
			t.Errorf("%s %s %s: %v", c.level, c.risk, c.decision, got)
		}
	}
}

func TestEnrolmentIsGated(t *testing.T) {
	f := newFixture(t)
	// A device that didn't come through a pairing offer gets no grant.
	plain, _, err := f.store.Add("Old screen", devices.KindPWA, nil, "lan", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.v.Grant(plain, rp); !errors.Is(err, stepup.ErrNoGrant) {
		t.Fatalf("no grant: %v", err)
	}
	for _, g := range []stepup.EnrolGrant{{Kind: stepup.GrantPairing}, {Kind: stepup.GrantOwner}, {Kind: stepup.GrantPasskey}, {}} {
		if _, _, err := f.v.BeginRegistration(rp, plain, g); !errors.Is(err, stepup.ErrNoGrant) {
			t.Fatalf("made-up %q grant: %v", g.Kind, err)
		}
	}

	// Within 15 minutes of claiming, once.
	phone := f.claimed("Akshay's iPhone")
	f.clock.Add(14 * time.Minute)
	g, err := f.v.Grant(phone, rp)
	if err != nil || g.Kind != stepup.GrantPairing {
		t.Fatalf("pairing grant: %+v %v", g, err)
	}
	a := stepuptest.New(rp.Origin)
	pk, err := f.enrol(phone, g, a)
	if err != nil {
		t.Fatal(err)
	}
	if pk.RPID != rp.ID || pk.Grant != "pairing" || pk.Credential != nil {
		t.Fatalf("enrolled %+v", pk)
	}
	if len(f.heard) != 1 || f.heard[0].Device.ID != phone.ID || f.heard[0].Grant != stepup.GrantPairing {
		t.Fatalf("heard %+v", f.heard)
	}
	if _, err := f.enrol(phone, g, stepuptest.New(rp.Origin)); !errors.Is(err, stepup.ErrNoGrant) {
		t.Fatalf("the pairing grant worked twice: %v", err)
	}
	// Now it has a passkey, a second one needs that passkey.
	if _, err := f.v.Grant(phone, rp); !errors.Is(err, stepup.ErrNeedProof) {
		t.Fatalf("second enrolment: %v", err)
	}

	// Past the window, a fresh pairing's grant is gone.
	late := f.claimed("Tablet")
	f.clock.Add(16 * time.Minute)
	if _, err := f.v.Grant(late, rp); !errors.Is(err, stepup.ErrNoGrant) {
		t.Fatalf("after 16 minutes: %v", err)
	}
	// A registration begun in the window but finished after it fails.
	slow := f.claimed("Slow phone")
	opts, sid, err := f.v.BeginRegistration(rp, slow, stepup.EnrolGrant{Kind: stepup.GrantPairing})
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := stepuptest.New(rp.Origin).Create(opts)
	f.clock.Add(15*time.Minute + time.Second)
	if _, err := f.v.FinishRegistration(slow, sid, bytes.NewReader(cred)); err == nil {
		t.Fatal("finished after the window")
	}
}

func TestOwnerAndPasskeyGrants(t *testing.T) {
	f := newFixture(t)
	plain, _, _ := f.store.Add("Old screen", devices.KindPWA, nil, "lan", "")
	if err := f.v.AllowEnrolment(plain.ID); err != nil {
		t.Fatal(err)
	}
	g, err := f.v.Grant(plain, rp)
	if err != nil || g.Kind != stepup.GrantOwner {
		t.Fatalf("owner grant: %+v %v", g, err)
	}
	a := stepuptest.New(rp.Origin)
	if _, err := f.enrol(plain, g, a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enrol(plain, g, stepuptest.New(rp.Origin)); !errors.Is(err, stepup.ErrNoGrant) {
		t.Fatalf("the owner's grant worked twice: %v", err)
	}
	// The owner's grant lapses.
	other, _, _ := f.store.Add("Other", devices.KindPWA, nil, "lan", "")
	_ = f.v.AllowEnrolment(other.ID)
	f.clock.Add(stepup.OwnerGrantTTL + time.Second)
	if _, err := f.v.Grant(other, rp); !errors.Is(err, stepup.ErrNoGrant) {
		t.Fatalf("lapsed owner grant: %v", err)
	}

	// The passkey it has lets it add another (a new phone restored from
	// the old one's backup, say).
	opts, sid, err := f.v.BeginEnrolProof(rp, plain)
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := a.Get(opts)
	pg, err := f.v.FinishEnrolProof(plain, sid, bytes.NewReader(proof))
	if err != nil || pg.Kind != stepup.GrantPasskey {
		t.Fatalf("proof: %+v %v", pg, err)
	}
	if _, err := f.enrol(plain, pg, stepuptest.New(rp.Origin)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enrol(plain, pg, stepuptest.New(rp.Origin)); !errors.Is(err, stepup.ErrNoGrant) {
		t.Fatalf("a passkey's grant worked twice: %v", err)
	}
	if n := len(f.store.Passkeys(plain.ID)); n != 2 {
		t.Fatalf("%d passkeys", n)
	}
	// Another device's proof is no use.
	phone, _ := f.enrolled("Phone")
	if _, err := f.v.FinishEnrolProof(phone, sid, bytes.NewReader(proof)); !errors.Is(err, stepup.ErrSessionUnknown) {
		t.Fatalf("someone else's proof: %v", err)
	}
}

func TestApprovalAssertion(t *testing.T) {
	f := newFixture(t)
	dev, a := f.enrolled("Akshay's iPhone")
	input := []byte(`{"to":"cab","amount":"42.10"}`)

	sid, assertion := f.approve(dev, a, 12, "approve", input)
	if err := f.v.FinishApproval(dev, sid, 12, "approve", input, bytes.NewReader(assertion)); err != nil {
		t.Fatal(err)
	}
	// Single use.
	if err := f.v.FinishApproval(dev, sid, 12, "approve", input, bytes.NewReader(assertion)); !errors.Is(err, stepup.ErrSessionUsed) {
		t.Fatalf("replay: %v", err)
	}
	// Bound to the approval, the decision and the stored input.
	for name, c := range map[string]struct {
		id       int64
		decision string
		input    []byte
	}{
		"#13":           {13, "approve", input},
		"deny":          {12, "deny", input},
		"changed input": {12, "approve", []byte(`{"to":"cab","amount":"421.00"}`)},
	} {
		sid, assertion := f.approve(dev, a, 12, "approve", input)
		if err := f.v.FinishApproval(dev, sid, c.id, c.decision, c.input, bytes.NewReader(assertion)); !errors.Is(err, stepup.ErrMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An assertion made for #12 in its own session is no use in #13's.
	_, for12 := f.approve(dev, a, 12, "approve", input)
	sid13, _ := f.approve(dev, a, 13, "approve", input)
	if err := f.v.FinishApproval(dev, sid13, 13, "approve", input, bytes.NewReader(for12)); !errors.Is(err, stepup.ErrAssertion) {
		t.Fatalf("#12's assertion on #13: %v", err)
	}
	// Two minutes.
	sid, assertion = f.approve(dev, a, 12, "approve", input)
	f.clock.Add(stepup.SessionTTL + time.Second)
	if err := f.v.FinishApproval(dev, sid, 12, "approve", input, bytes.NewReader(assertion)); !errors.Is(err, stepup.ErrSessionExpired) {
		t.Fatalf("late: %v", err)
	}
	// Another device can't finish this device's check.
	other, oa := f.enrolled("Tablet")
	sid, assertion = f.approve(dev, a, 12, "approve", input)
	forged, _ := oa.Get([]byte(`{"challenge":"AAAA","rpId":"twin.example.ts.net"}`))
	if err := f.v.FinishApproval(other, sid, 12, "approve", input, bytes.NewReader(forged)); !errors.Is(err, stepup.ErrSessionUnknown) {
		t.Fatalf("other device: %v", err)
	}
	// ... nor spend it: the device's own check still goes through.
	if err := f.v.FinishApproval(dev, sid, 12, "approve", input, bytes.NewReader(assertion)); err != nil {
		t.Fatalf("own check after another device tried it: %v", err)
	}
	if _, _, err := f.v.BeginApproval(stepup.RP{ID: "evil.test", Origin: "https://evil.test"}, dev, 12, "approve", input); !errors.Is(err, stepup.ErrNoPasskey) {
		t.Fatalf("a passkey for another host: %v", err)
	}
}

func TestUserVerificationAndClonesAreRefused(t *testing.T) {
	f := newFixture(t)
	dev, a := f.enrolled("Phone")
	input := []byte(`{}`)
	a.NoUV = true
	sid, assertion := f.approve(dev, a, 1, "approve", input)
	if err := f.v.FinishApproval(dev, sid, 1, "approve", input, bytes.NewReader(assertion)); !errors.Is(err, stepup.ErrAssertion) {
		t.Fatalf("no user verification: %v", err)
	}
	a.NoUV = false
	sid, assertion = f.approve(dev, a, 1, "approve", input)
	if err := f.v.FinishApproval(dev, sid, 1, "approve", input, bytes.NewReader(assertion)); err != nil {
		t.Fatal(err)
	}
	// Its counter was saved: one that doesn't move on is a copy.
	a.StuckCounter = true
	sid, assertion = f.approve(dev, a, 1, "approve", input)
	if err := f.v.FinishApproval(dev, sid, 1, "approve", input, bytes.NewReader(assertion)); !errors.Is(err, stepup.ErrAssertion) {
		t.Fatalf("stuck counter: %v", err)
	}
	var rec struct {
		Authenticator struct{ SignCount uint32 }
	}
	pks := f.store.Passkeys(dev.ID)
	if len(pks) != 1 || json.Unmarshal(pks[0].Credential, &rec) != nil || rec.Authenticator.SignCount == 0 || pks[0].LastUsed.IsZero() {
		t.Fatalf("use not recorded: %+v %s", rec, pks)
	}
}

func TestPasskeysNeedASecureOrigin(t *testing.T) {
	f := newFixture(t)
	dev := f.claimed("Phone")
	g := stepup.EnrolGrant{Kind: stepup.GrantPairing}
	for _, bad := range []stepup.RP{
		{ID: "100.64.0.1", Origin: "https://100.64.0.1"},            // an address isn't a name
		{ID: "twin.lan", Origin: "http://twin.lan"},                 // plain HTTP
		{ID: "twin.example.ts.net", Origin: "https://evil.example"}, // origin elsewhere
	} {
		if _, _, err := f.v.BeginRegistration(bad, dev, g); !errors.Is(err, stepup.ErrHost) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if got := stepup.RPFor("localhost:8443", false); got != (stepup.RP{ID: "localhost", Origin: "http://localhost:8443"}) {
		t.Fatal(got)
	}
	f.v.AllowHosts(func(h string) bool { return h == "twin.example.ts.net" })
	if _, _, err := f.v.BeginRegistration(stepup.RP{ID: "other.ts.net", Origin: "https://other.ts.net"}, dev, g); !errors.Is(err, stepup.ErrHost) {
		t.Fatalf("host outside the allowlist: %v", err)
	}
}

func TestRevokingADeviceRemovesItsPasskeys(t *testing.T) {
	f := newFixture(t)
	dev, a := f.enrolled("Phone")
	if got, _ := f.store.Get(dev.ID); got.PasskeyCount != 1 {
		t.Fatalf("count %d", got.PasskeyCount)
	}
	sid, assertion := f.approve(dev, a, 1, "approve", []byte(`{}`))
	if _, err := f.store.Revoke(dev.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(f.store.Passkeys(dev.ID)); n != 0 {
		t.Fatalf("%d passkeys left", n)
	}
	reopened, err := devices.Open(f.store.File())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range reopened.List() {
		if d.ID == dev.ID && d.PasskeyCount != 0 {
			t.Fatal("passkeys still on disk")
		}
	}
	if err := f.v.FinishApproval(dev, sid, 1, "approve", []byte(`{}`), bytes.NewReader(assertion)); !errors.Is(err, stepup.ErrSessionUnknown) {
		t.Fatalf("a revoked device's check: %v", err)
	}
	if _, err := f.v.Grant(dev, rp); !errors.Is(err, stepup.ErrRevoked) {
		t.Fatalf("grant for a revoked device: %v", err)
	}
}

func TestRevokePasskeysEnrolledInAWindow(t *testing.T) {
	f := newFixture(t)
	before, _ := f.enrolled("Before")
	f.clock.Add(time.Hour)
	from := f.clock.Now()
	during, da := f.enrolled("During")
	f.clock.Add(time.Minute)
	to := f.clock.Now()
	f.clock.Add(time.Hour)
	after, _ := f.enrolled("After")
	sid, assertion := f.approve(during, da, 1, "approve", []byte(`{}`))

	var r stepup.PasskeyRevoker = f.v
	removed, err := r.RevokePasskeysEnrolled(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Device.ID != during.ID {
		t.Fatalf("removed %+v", removed)
	}
	for _, c := range []struct {
		d    devices.Device
		want int
	}{{before, 1}, {during, 0}, {after, 1}} {
		if n := len(f.store.Passkeys(c.d.ID)); n != c.want {
			t.Errorf("%s: %d passkeys", c.d.Name, n)
		}
		if d, _ := f.store.Get(c.d.ID); d.Revoked() {
			t.Errorf("%s was unpaired", c.d.Name)
		}
	}
	if err := f.v.FinishApproval(during, sid, 1, "approve", []byte(`{}`), bytes.NewReader(assertion)); err == nil {
		t.Fatal("a check begun with a removed passkey still worked")
	}
}

// Looping begin and a failed finish can't grow the sessions kept in memory
// without bound: used ones count too, until they are forgotten.
func TestFinishedChecksCountTowardsTheCap(t *testing.T) {
	f := newFixture(t)
	dev, _ := f.enrolled("Akshay's iPhone")
	input := []byte(`{"amount":"42.10"}`)
	refused := -1
	for i := range 200 {
		_, sid, err := f.v.BeginApproval(rp, dev, 12, "approve", input)
		if err != nil {
			refused = i
			break
		}
		if err := f.v.FinishApproval(dev, sid, 12, "approve", input, bytes.NewReader([]byte("{}"))); err == nil {
			t.Fatal("a garbage assertion passed")
		}
	}
	if refused < 16 || refused > 64 {
		t.Fatalf("refused after %d checks", refused)
	}
	// Once the used ones are forgotten, checks can start again.
	f.clock.Add(16 * time.Minute)
	if _, _, err := f.v.BeginApproval(rp, dev, 12, "approve", input); err != nil {
		t.Fatalf("after the used checks were forgotten: %v", err)
	}
}
