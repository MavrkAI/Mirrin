package devices

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func open(t *testing.T) (*Store, string, *clock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "devices.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)}
	s.SetClock(c.now)
	return s, path, c
}

func TestTokensAreHashedAtRestAndFileIsPrivate(t *testing.T) {
	s, path, _ := open(t)
	o, err := s.NewOffer(KindPWA, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	d, tok, err := s.Claim(o.ID, o.Secret, "Akshay's iPhone", "", "tailscale", "100.64.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "abt1_"+d.ID+"_") || len(tok) != len("abt1_")+16+1+43 {
		t.Fatalf("token shape %q", tok)
	}
	_, tok2, err := s.Add("Laptop", KindCLI, nil, "lan", "10.0.0.3")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The secret part may itself hold '_' (base64url), so split only twice:
	// splitting on every '_' could leave "" here, which every file contains.
	for _, secret := range []string{tok, tok2, o.Secret, strings.SplitN(tok, "_", 3)[2], strings.SplitN(tok2, "_", 3)[2]} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("devices.json holds a raw credential %q:\n%s", secret, b)
		}
	}
	if !strings.Contains(string(b), HashToken(tok)) {
		t.Fatal("the token's hash isn't stored")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("devices.json mode %v", fi.Mode().Perm())
		}
	}
	// It all comes back after a restart.
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Authenticate(tok)
	if !ok || got.Name != "Akshay's iPhone" || !got.Has(Approve) || got.Has(Admin) || got.Via != "tailscale" {
		t.Fatalf("after reopen: %+v %v", got, ok)
	}
	if got.TokenHash != "" {
		t.Fatal("Authenticate leaked the stored hash")
	}
}

func TestRevokeIsInstantAndLeavesATombstone(t *testing.T) {
	s, path, _ := open(t)
	d, tok, err := s.Add("Phone", KindPWA, nil, "lan", "10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	var heard []Change
	s.OnChange(func(c Change) { heard = append(heard, c) })
	tick := s.NewTicket(d.ID, tok)
	if _, ok := s.Authenticate(tok); !ok {
		t.Fatal("fresh token refused")
	}
	if _, err := s.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Authenticate(tok); ok {
		t.Fatal("a revoked device still authenticates")
	}
	if _, _, err := s.RedeemTicket(tick); !errors.Is(err, ErrTicketGone) {
		t.Fatalf("a revoked device's ticket still works: %v", err)
	}
	if len(heard) != 1 || heard[0].Kind != Revoked || heard[0].Device.ID != d.ID {
		t.Fatalf("hooks heard %+v", heard)
	}
	again, _ := Open(path)
	list := again.List()
	if len(list) != 1 || !list[0].Revoked() {
		t.Fatalf("tombstone missing: %+v", list)
	}
	if _, ok := again.Authenticate(tok); ok {
		t.Fatal("revocation didn't survive a restart")
	}
	if _, err := s.Revoke(d.ID); err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
	if _, err := s.Revoke("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestAuthenticateRejectsLookalikes(t *testing.T) {
	s, _, _ := open(t)
	d, tok, _ := s.Add("Phone", KindPWA, nil, "", "")
	bad := []string{
		"",
		"abt1_",
		tok[:len(tok)-1],
		tok + "x",
		strings.Replace(tok, d.ID, "0000000000000000", 1),
		"abt1_" + d.ID + "_" + strings.Repeat("A", 43),
		strings.ToUpper(tok),
	}
	for _, b := range bad {
		if _, ok := s.Authenticate(b); ok {
			t.Errorf("authenticated %q", b)
		}
	}
}

func TestOfferIsSingleUseAndExpires(t *testing.T) {
	s, _, c := open(t)
	o, _ := s.NewOffer(KindCLI, nil, 0)
	if !strings.HasPrefix(o.ID, "of_") || len(o.ID) != 13 || len(o.Secret) != 43 {
		t.Fatalf("offer shape %+v", o)
	}
	if _, _, err := s.Claim(o.ID, o.Secret, "Laptop", KindCLI, "lan", "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Claim(o.ID, o.Secret, "Laptop", KindCLI, "lan", "10.0.0.2"); !errors.Is(err, ErrOfferUsed) {
		t.Fatalf("second claim: %v", err)
	}
	late, _ := s.NewOffer(KindPWA, nil, 0)
	c.add(OfferTTL + time.Second)
	if _, _, err := s.Claim(late.ID, late.Secret, "", "", "lan", "10.0.0.3"); !errors.Is(err, ErrOfferExpired) {
		t.Fatalf("late claim: %v", err)
	}
	if _, _, err := s.Claim("of_nothere00", late.Secret, "", "", "lan", "10.0.0.3"); !errors.Is(err, ErrOfferUnknown) {
		t.Fatalf("unknown offer: %v", err)
	}
}

// A withdrawn offer pairs nothing, even with its secret; a claimed one
// can't be withdrawn, and withdrawing touches no other offer.
func TestCancelOfferWithdrawsOnlyThatUnclaimedOffer(t *testing.T) {
	s, _, _ := open(t)
	o, _ := s.NewOffer(KindPWA, nil, 0)
	other, _ := s.NewOffer(KindPWA, nil, 0)
	if !s.CancelOffer(o.ID) || s.CancelOffer(o.ID) || s.CancelOffer("of_nothere00") {
		t.Fatal("CancelOffer's answers")
	}
	if _, _, err := s.Claim(o.ID, o.Secret, "", "", "lan", "10.0.0.2"); !errors.Is(err, ErrOfferRevoked) {
		t.Fatalf("claim of a withdrawn offer: %v", err)
	}
	if _, _, err := s.Claim(other.ID, other.Secret, "", "", "lan", "10.0.0.2"); err != nil {
		t.Fatalf("the other offer: %v", err)
	}
	if s.CancelOffer(other.ID) {
		t.Fatal("withdrew a claimed offer")
	}
}

func TestFiveBadSecretsBurnOnlyThatOffer(t *testing.T) {
	s, _, _ := open(t)
	o, _ := s.NewOffer(KindPWA, nil, 0)
	other, _ := s.NewOffer(KindPWA, nil, 0)
	for i := 0; i < 5; i++ {
		// Each guess from its own address: the burn is per offer, not per IP.
		if _, _, err := s.Claim(o.ID, "wrong", "", "", "lan", "10.0.1."+string(rune('1'+i))); !errors.Is(err, ErrBadSecret) {
			t.Fatalf("guess %d: %v", i+1, err)
		}
	}
	if _, _, err := s.Claim(o.ID, o.Secret, "", "", "lan", "10.0.0.2"); !errors.Is(err, ErrOfferBurned) {
		t.Fatalf("right secret after five wrong ones: %v", err)
	}
	// No global lockout: the owner's other offer still pairs.
	if _, _, err := s.Claim(other.ID, other.Secret, "", "", "lan", "10.0.0.2"); err != nil {
		t.Fatalf("another offer was locked out too: %v", err)
	}
}

func TestClaimsAreLimitedPerIP(t *testing.T) {
	s, _, c := open(t)
	for i := 0; i < ClaimsPerMinute; i++ {
		if _, _, err := s.Claim("of_nothere00", "x", "", "", "", "203.0.113.9"); !errors.Is(err, ErrOfferUnknown) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	o, _ := s.NewOffer(KindPWA, nil, 0)
	if _, _, err := s.Claim(o.ID, o.Secret, "", "", "", "203.0.113.9"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("11th claim in a minute: %v", err)
	}
	if s.RetryAfter("203.0.113.9") <= 0 {
		t.Fatal("no retry-after for a limited address")
	}
	// Another address isn't affected, and the offer wasn't spent.
	if _, _, err := s.Claim(o.ID, o.Secret, "", "", "", "198.51.100.7"); err != nil {
		t.Fatalf("other address: %v", err)
	}
	c.add(time.Minute)
	o2, _ := s.NewOffer(KindPWA, nil, 0)
	if _, _, err := s.Claim(o2.ID, o2.Secret, "", "", "", "203.0.113.9"); err != nil {
		t.Fatalf("a minute later: %v", err)
	}
}

func TestClaimChecksKindAndUsesTheOffersScopes(t *testing.T) {
	s, _, _ := open(t)
	o, _ := s.NewOffer(KindKiosk, nil, 0)
	if _, _, err := s.Claim(o.ID, o.Secret, "", KindCLI, "", "10.0.0.2"); !errors.Is(err, ErrWrongKind) {
		t.Fatalf("cli claiming a kiosk offer: %v", err)
	}
	d, _, err := s.Claim(o.ID, o.Secret, "  Kitchen\nwall  ", KindKiosk, "", "10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "Kitchen wall" || len(d.Scopes) != 1 || !d.Has(View) {
		t.Fatalf("kiosk device %+v", d)
	}
	if _, err := s.NewOffer(KindLocal, nil, 0); err == nil {
		t.Fatal("a pairing offer for a loopback-only browser")
	}
}

func TestTicketsWorkOnce(t *testing.T) {
	s, _, c := open(t)
	d, tok, _ := s.Add("Phone", KindPWA, nil, "", "")
	it := s.NewTicket(d.ID, tok)
	got, gotTok, err := s.RedeemTicket(it)
	if err != nil || got.ID != d.ID || gotTok != tok {
		t.Fatalf("redeem: %+v %v", got, err)
	}
	if _, _, err := s.RedeemTicket(it); !errors.Is(err, ErrTicketGone) {
		t.Fatalf("second redeem: %v", err)
	}
	it2 := s.NewTicket(d.ID, tok)
	c.add(TicketTTL + time.Second)
	if _, _, err := s.RedeemTicket(it2); !errors.Is(err, ErrTicketGone) {
		t.Fatalf("late redeem: %v", err)
	}
}

func TestLocalBrowsersArePruned(t *testing.T) {
	s, _, c := open(t)
	phone, _, _ := s.Add("Phone", KindPWA, nil, "", "")
	var first string
	for i := 0; i < maxLocal+5; i++ {
		d, _, err := s.AddLocal("Safari")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = d.ID
		}
		c.add(time.Second)
	}
	var locals int
	for _, d := range s.List() {
		if d.Kind == KindLocal {
			locals++
		}
		if d.ID == first {
			t.Fatal("the least recently used browser was kept past the cap")
		}
	}
	if locals != maxLocal {
		t.Fatalf("%d local browsers kept", locals)
	}
	c.add(localMaxAge + time.Hour)
	if _, _, err := s.AddLocal("Chrome"); err != nil {
		t.Fatal(err)
	}
	locals = 0
	for _, d := range s.List() {
		if d.Kind == KindLocal {
			locals++
		}
	}
	if locals != 1 {
		t.Fatalf("stale browsers kept: %d", locals)
	}
	if _, ok := s.Get(phone.ID); !ok {
		t.Fatal("pruning browsers removed a paired phone")
	}
}

func TestFindByPrefixSkipsRevokedAndLocal(t *testing.T) {
	s, _, _ := open(t)
	d, _, _ := s.Add("Phone", KindPWA, nil, "", "")
	if got, err := s.Find(strings.ToUpper(d.ID[:6])); err != nil || got.ID != d.ID {
		t.Fatalf("find: %+v %v", got, err)
	}
	if _, err := s.Find(d.ID[:3]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("three characters matched: %v", err)
	}
	_, _ = s.Revoke(d.ID)
	if _, err := s.Find(d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("found a revoked device: %v", err)
	}
	l, _, _ := s.AddLocal("Safari")
	if _, err := s.Find(l.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("found a local browser: %v", err)
	}
}

func TestTouchIsSavedAtMostOnceAMinute(t *testing.T) {
	s, path, c := open(t)
	d, _, _ := s.Add("Phone", KindPWA, nil, "", "10.0.0.2")
	c.add(10 * time.Second)
	s.Touch(d.ID, "10.0.0.2")
	again, _ := Open(path)
	if got, _ := again.Get(d.ID); !got.LastSeen.Equal(d.LastSeen) {
		t.Fatal("wrote last-seen within the minute")
	}
	s.Touch(d.ID, "10.0.0.9") // a new address is written at once
	again, _ = Open(path)
	if got, _ := again.Get(d.ID); got.LastIP != "10.0.0.9" {
		t.Fatalf("new address not saved: %+v", got)
	}
	c.add(2 * time.Minute)
	s.Touch(d.ID, "10.0.0.9")
	again, _ = Open(path)
	if got, _ := again.Get(d.ID); !got.LastSeen.Equal(c.now()) {
		t.Fatalf("last-seen not saved after a minute: %v", got.LastSeen)
	}
}

func TestDamagedFileIsNotReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "move it aside") {
		t.Fatalf("damaged file: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "{nope" {
		t.Fatal("the damaged file was overwritten")
	}
}

// A hand-edited or restored file may name a device with an id no token could
// carry: it's left out, not trusted and not shown.
func TestOddIDsAreLeftOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	f := `{"version":1,"devices":[{"id":"abc","name":"Odd","kind":"pwa"},{"id":"0123456789ABCDEZ","name":"Odd too","kind":"pwa"},{"id":"0123456789abcdef","name":"Fine","kind":"pwa"}]}`
	if err := os.WriteFile(path, []byte(f), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if l := s.List(); len(l) != 1 || l[0].Name != "Fine" {
		t.Fatalf("%+v", l)
	}
}

func TestSharedKeyMarkIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s, _ := Open(path)
	d, tok, err := s.AddShared("Old screen", KindPWA, "lan", "192.168.1.9")
	if err != nil || !d.SharedKey || !slices.Equal(d.Scopes, DefaultScopes(KindPWA)) {
		t.Fatalf("%+v %v", d, err)
	}
	if other, _, _ := s.Add("Phone", KindPWA, nil, "", ""); other.SharedKey {
		t.Fatal("a device paired on its own marked as from the shared key")
	}
	again, _ := Open(path)
	if got, ok := again.Authenticate(tok); !ok || !got.SharedKey {
		t.Fatalf("after reopening: %+v %v", got, ok)
	}
	if s.File() != path || NewMemory().File() != "" {
		t.Fatal("File")
	}
}

func TestParseScopes(t *testing.T) {
	got, err := ParseScopes("view, Chat,view")
	if err != nil || len(got) != 2 || got[0] != View || got[1] != Chat {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseScopes("view,root"); err == nil {
		t.Fatal("accepted an unknown scope")
	}
	if _, err := ParseScopes(" "); err == nil {
		t.Fatal("accepted no scopes")
	}
}

func TestRename(t *testing.T) {
	s, _, _ := open(t)
	d, _, _ := s.Add("Phone", KindPWA, nil, "", "")
	if got, err := s.Rename(d.ID, "  Kitchen\tiPad "); err != nil || got.Name != "Kitchen iPad" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.Rename(d.ID, "   "); err == nil {
		t.Fatal("renamed to nothing")
	}
}

// A passkey already on the device isn't enrolled again: replacing it would
// reset its sign count, and with it the check for a cloned authenticator.
func TestAPasskeyIsNotReplacedByEnrollingItAgain(t *testing.T) {
	s, _, _ := open(t)
	d, _, err := s.Add("Phone", KindPWA, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	first := Passkey{ID: "cred1", PublicKey: "pk", Credential: []byte(`{"authenticator":{"signCount":41}}`)}
	if _, err := s.AddPasskey(d.ID, first, 0); err != nil {
		t.Fatal(err)
	}
	again := Passkey{ID: "cred1", PublicKey: "pk", Credential: []byte(`{"authenticator":{"signCount":0}}`)}
	if _, err := s.AddPasskey(d.ID, again, 0); !errors.Is(err, ErrPasskeyExists) {
		t.Fatalf("enrolled again: %v", err)
	}
	if pks := s.Passkeys(d.ID); len(pks) != 1 || string(pks[0].Credential) != string(first.Credential) {
		t.Fatalf("passkeys %+v", pks)
	}
}
