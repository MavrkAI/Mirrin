package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func vector(t *testing.T) (map[string]string, Subscription) {
	t.Helper()
	b, err := os.ReadFile("testdata/rfc8291.json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	s := Subscription{Endpoint: "https://fcm.googleapis.com/push/test", DeviceID: "device"}
	s.Keys.P256DH = v["ua_public"]
	s.Keys.Auth = v["auth"]
	return v, s
}
func decode(t *testing.T, s string) []byte {
	t.Helper()
	b, e := raw.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestRFC8291(t *testing.T) {
	v, sub := vector(t)
	eph, e := ecdh.P256().NewPrivateKey(decode(t, v["as_private"]))
	if e != nil {
		t.Fatal(e)
	}
	salt := decode(t, v["salt"])
	got, e := encryptWith(eph, salt, sub, []byte(v["plaintext"]))
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, decode(t, v["body"])) {
		t.Fatalf("RFC ciphertext mismatch: %s", raw.EncodeToString(got))
	}
	d, e := derive(eph, salt, sub)
	if e != nil {
		t.Fatal(e)
	}
	for k, b := range map[string][]byte{"shared": d.shared, "prk_key": d.prkKey, "key_info": d.info, "ikm": d.ikm, "prk": d.prk, "cek": d.cek, "nonce": d.nonce, "header": got[:86], "ciphertext": got[86:]} {
		if raw.EncodeToString(b) != v[k] {
			t.Errorf("%s differs", k)
		}
	}
}
func TestVAPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.pem")
	v, e := LoadOrCreateVAPID(path)
	if e != nil {
		t.Fatal(e)
	}
	again, e := LoadOrCreateVAPID(path)
	if e != nil || v.PublicKey() != again.PublicKey() {
		t.Fatal("key did not persist", e)
	}
	fi, _ := os.Stat(path)
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0600 {
		t.Fatal("key not private")
	}
	now := time.Unix(1700000000, 0)
	h, e := v.Header("https://fcm.googleapis.com/send/abc", "", now)
	if e != nil {
		t.Fatal(e)
	}
	token := strings.Split(strings.TrimPrefix(h, "vapid t="), ", k=")[0]
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal(h)
	}
	var header map[string]string
	json.Unmarshal(decode(t, parts[0]), &header)
	if header["alg"] != "ES256" {
		t.Fatal(header)
	}
	var claims struct {
		Aud, Sub string
		Exp      int64
	}
	json.Unmarshal(decode(t, parts[1]), &claims)
	if claims.Aud != "https://fcm.googleapis.com" || claims.Sub != DefaultSubject || claims.Exp <= now.Unix() || claims.Exp > now.Add(24*time.Hour).Unix() {
		t.Fatal(claims)
	}
	sig := decode(t, parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(&v.key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("JWT signature failed")
	}
	if _, e = v.Header("https://fcm.googleapis.com/x", "mailto:owner@example.com", now); e == nil {
		t.Fatal("email allowed")
	}
	os.WriteFile(path, []byte("broken"), 0600)
	if _, e = LoadOrCreateVAPID(path); e == nil {
		t.Fatal("damaged key silently replaced")
	}
}
func publicResolve(string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
}
func TestAllowedEndpoint(t *testing.T) {
	for _, u := range []string{"http://169.254.169.254/", "https://example.com/", "https://fcm.googleapis.com:8443/x", "https://fcm.googleapis.com.evil.test/", "https://owner@fcm.googleapis.com/"} {
		if AllowedEndpoint(u, publicResolve) == nil {
			t.Errorf("allowed %s", u)
		}
	}
	for _, ip := range []string{"10.0.0.1", "127.0.0.1", "::1", "::ffff:10.0.0.1", "100.64.0.1", "169.254.169.254", "fc00::1", "2001:db8::1"} {
		if AllowedEndpoint("https://fcm.googleapis.com/x", func(string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr(ip)}, nil }) == nil {
			t.Errorf("allowed %s", ip)
		}
	}
	for _, u := range []string{"https://web.push.apple.com/x", "https://fcm.googleapis.com/x", "https://updates.push.services.mozilla.com/x", "https://a.notify.windows.com/x"} {
		if e := AllowedEndpoint(u, publicResolve); e != nil {
			t.Fatal(u, e)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// receiverDecrypt follows the receiver side with the UA private key, never
// the sender's private key or derive implementation.
func receiverDecrypt(t *testing.T, b []byte, v map[string]string) []byte {
	t.Helper()
	if len(b) < 103 {
		t.Fatal("short body")
	}
	ua, e := ecdh.P256().NewPrivateKey(decode(t, v["ua_private"]))
	if e != nil {
		t.Fatal(e)
	}
	as, e := ecdh.P256().NewPublicKey(b[21:86])
	if e != nil {
		t.Fatal(e)
	}
	secret, e := ua.ECDH(as)
	if e != nil {
		t.Fatal(e)
	}
	info := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), as.Bytes()...)
	ikm, e := hkdf.Key(sha256.New, secret, decode(t, v["auth"]), string(info), 32)
	if e != nil {
		t.Fatal(e)
	}
	key, _ := hkdf.Key(sha256.New, ikm, b[:16], "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, b[:16], "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	pt, e := gcm.Open(nil, nonce, b[86:], nil)
	if e != nil {
		t.Fatal(e)
	}
	if pt[len(pt)-1] != 2 {
		t.Fatal("missing final delimiter")
	}
	return pt[:len(pt)-1]
}
func testSender(t *testing.T, network bool) {
	t.Helper()
	v, sub := vector(t)
	key, e := LoadOrCreateVAPID(filepath.Join(t.TempDir(), "vapid.pem"))
	if e != nil {
		t.Fatal(e)
	}
	payload := []byte(`{"v":1,"k":"approval","id":12}`)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for h, want := range map[string]string{"Content-Encoding": "aes128gcm", "TTL": "86400", "Urgency": "high", "Topic": "approval-12"} {
			if r.Header.Get(h) != want {
				t.Errorf("%s: %s", h, r.Header.Get(h))
			}
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") {
			t.Error("no VAPID")
		}
		b, _ := io.ReadAll(r.Body)
		if !bytes.Equal(receiverDecrypt(t, b, v), payload) {
			t.Error("wrong plaintext")
		}
		w.WriteHeader(201)
	})
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}
	if network {
		ts := httptest.NewServer(handler)
		defer ts.Close()
		client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			copy := r.Clone(r.Context())
			copy.URL.Scheme = "http"
			copy.URL.Host = strings.TrimPrefix(ts.URL, "http://")
			return ts.Client().Transport.RoundTrip(copy)
		})}
	}
	sender := Sender{VAPID: key, Client: client, Resolve: publicResolve}
	if e = sender.Send(t.Context(), sub, payload, "approval", "approval-12"); e != nil {
		t.Fatal(e)
	}
	for _, endpoint := range []string{"http://169.254.169.254/", "https://example.com/"} {
		sub.Endpoint = endpoint
		if sender.Send(t.Context(), sub, payload, "approval", "") == nil {
			t.Fatal("unsafe endpoint sent")
		}
	}
	sub.Endpoint = "https://fcm.googleapis.com/x"
	sender.Resolve = func(string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil }
	if sender.Send(t.Context(), sub, payload, "approval", "") == nil {
		t.Fatal("private IP sent")
	}
}
func TestSendRecorder(t *testing.T) { testSender(t, false) }
func TestSendHTTP(t *testing.T)     { testSender(t, true) }
func TestRetryAndGone(t *testing.T) {
	_, sub := vector(t)
	store, _ := Open("")
	store.Put(sub)
	key, _ := LoadOrCreateVAPID(filepath.Join(t.TempDir(), "key"))
	status := 429
	sender := Sender{VAPID: key, Resolve: publicResolve, Client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Retry-After", "259200")
		w.WriteHeader(status)
		return w.Result(), nil
	})}}
	d := NewDispatcher(store, sender.Send)
	d.Approves = func(string) bool { return true }
	d.Enqueue(Approval(12, "Book the cab", "pending", "Ava", 1), "")
	k, item := d.next()
	err := d.deliver(t.Context(), item)
	d.finish(k, item, err)
	if item.due.Before(time.Now().Add(72*time.Hour - time.Second)) {
		t.Fatal("ignored Retry-After")
	}
	status = 410
	if e := d.deliver(t.Context(), item); e != nil {
		t.Fatal(e)
	}
	if len(store.List()) != 0 {
		t.Fatal("410 retained subscription")
	}
}
func TestDispatchResolvedPrivateRevoke(t *testing.T) {
	_, sub := vector(t)
	store, _ := Open(filepath.Join(t.TempDir(), "push.json"))
	for _, id := range []string{"phone", "tablet"} {
		s := sub
		s.DeviceID = id
		s.Endpoint += "/" + id
		if e := store.Put(s); e != nil {
			t.Fatal(e)
		}
	}
	var delivered []string
	d := NewDispatcher(store, func(_ context.Context, s Subscription, b []byte, kind, tag string) error {
		delivered = append(delivered, s.DeviceID)
		if bytes.Contains(b, []byte("actions")) || bytes.Contains(b, []byte("secret summary")) {
			t.Fatal("payload leaked")
		}
		var p map[string]any
		json.Unmarshal(b, &p)
		if p["badge"] != float64(2) || p["b"] != "This request no longer needs you." || kind != "resolved" || tag != "approval-12" {
			t.Fatal(p)
		}
		return nil
	})
	d.Settings = func() (string, Config) { return "Ava", Config{Preview: "private"} }
	d.Approves = func(string) bool { return true }
	d.Enqueue(Approval(12, "secret summary", "pending", "Ava", 3), "")
	d.Enqueue(Approval(12, "secret summary", "approved", "Ava", 2), "phone")
	// The deciding device's old queued approval must also be discarded.
	for _, item := range d.pending {
		if item.sub.DeviceID == "phone" {
			t.Fatal("stale approval remains for deciding device")
		}
	}
	k, item := d.next()
	if item == nil {
		t.Fatal("no resolved push")
	}
	err := d.deliver(t.Context(), item)
	d.finish(k, item, err)
	if len(delivered) != 1 || delivered[0] != "tablet" {
		t.Fatal(delivered)
	}
	d.Enqueue(Approval(13, "", "pending", "Ava", 1), "")
	store.Delete("phone", "")
	store.Delete("tablet", "")
	_, item = d.next()
	if item != nil {
		if e := d.deliver(t.Context(), item); e != nil {
			t.Fatal(e)
		}
	}
	if len(delivered) != 1 {
		t.Fatal("revoked subscription sent")
	}
	reopened, e := Open(store.path)
	if e != nil || len(reopened.List()) != 0 {
		t.Fatal("revoke did not persist", e)
	}
}
func TestQuietHours(t *testing.T) {
	c := Config{QuietHours: "22:00-07:00"}
	now := time.Date(2026, 9, 27, 23, 0, 0, 0, time.Local)
	if c.permits("question", now) || !c.permits("approval", now) || !c.permits("security", now) {
		t.Fatal("quiet hours policy")
	}
	// A page handed over in the browser is the owner's own errand, waiting
	// on them now: quiet hours don't hold it back.
	if !c.permits("handover", now) {
		t.Fatal("hand-over held back by quiet hours")
	}
}

// InQuiet is the quiet-hours rule permits keeps, for the daemon to use too.
func TestInQuietAgreesWithPermits(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 9, 27, h, m, 0, 0, time.Local) }
	for _, c := range []struct {
		spec  string
		now   time.Time
		quiet bool
	}{
		{"22:00-07:00", at(23, 0), true},
		{"22:00-07:00", at(6, 59), true},
		{"22:00-07:00", at(7, 0), false},
		{"22:00-07:00", at(12, 0), false},
		{"13:00-14:00", at(13, 30), true},
		{"13:00-14:00", at(14, 0), false},
		{"09:00-09:00", at(9, 0), false},
		{"", at(23, 0), false},
		{"late-early", at(23, 0), false},
	} {
		if got := InQuiet(c.spec, c.now); got != c.quiet {
			t.Errorf("InQuiet(%q, %s) = %v", c.spec, c.now.Format("15:04"), got)
		}
		if (Config{QuietHours: c.spec}).permits("question", c.now) == c.quiet {
			t.Errorf("permits disagrees for %q at %s", c.spec, c.now.Format("15:04"))
		}
	}
}

// A hand-over goes only to a device that can act for the owner, never to a
// wall screen paired to look, and Enqueue says how many devices it reached.
func TestHandOverOnlyToApprovers(t *testing.T) {
	_, sub := vector(t)
	store, _ := Open("")
	for _, id := range []string{"phone", "kiosk"} {
		s := sub
		s.DeviceID = id
		s.Endpoint += "/" + id
		if err := store.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	var delivered []string
	d := NewDispatcher(store, func(_ context.Context, s Subscription, _ []byte, kind, tag string) error {
		if kind != "handover" || tag != "handover" {
			t.Fatalf("sent %s %s", kind, tag)
		}
		delivered = append(delivered, s.DeviceID)
		return nil
	})
	d.Settings = func() (string, Config) { return "Ava", Config{QuietHours: "00:00-23:59"} }
	n := Notification{Kind: "handover", Title: "Ava needs you in the browser", Body: "Tap to see what it needs.", URL: "/ui", Tag: "handover"}
	if got := d.Enqueue(n, ""); got != 0 {
		t.Fatalf("with no one known to approve, queued for %d", got)
	}
	d.Approves = func(id string) bool { return id == "phone" }
	if got := d.Enqueue(n, ""); got != 1 {
		t.Fatalf("queued for %d devices, want the phone", got)
	}
	for {
		k, item := d.next()
		if item == nil {
			break
		}
		d.finish(k, item, d.deliver(t.Context(), item))
	}
	if len(delivered) != 1 || delivered[0] != "phone" {
		t.Fatalf("delivered to %q", delivered)
	}
	// A device that stops approving before delivery doesn't get it.
	d.Enqueue(n, "")
	d.Approves = func(string) bool { return false }
	k, item := d.next()
	d.finish(k, item, d.deliver(t.Context(), item))
	if len(delivered) != 1 {
		t.Fatalf("delivered to %q", delivered)
	}
}

// A wall screen paired to look, with notifications on, never shows on its
// lock screen what an approval would pay for or what a task is asking. It
// still hears a security alarm, and its own test.
func TestAWallScreenGetsOnlyAlarms(t *testing.T) {
	_, sub := vector(t)
	store, _ := Open("")
	for _, id := range []string{"phone", "kiosk"} {
		s := sub
		s.DeviceID = id
		s.Endpoint += "/" + id
		if err := store.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	sent := map[string][]string{}
	d := NewDispatcher(store, func(_ context.Context, s Subscription, _ []byte, kind, _ string) error {
		sent[kind] = append(sent[kind], s.DeviceID)
		return nil
	})
	d.Approves = func(id string) bool { return id == "phone" }
	d.Enqueue(Approval(12, "Pay £45 to the plumber", "pending", "Ava", 1), "")
	d.Enqueue(Notification{Kind: "question", Title: "Ava has a question", Body: "Which card?", URL: "/ui", Tag: "question-t1"}, "")
	d.Enqueue(Notification{Kind: "security", Title: "Ava needs you", Body: "A new certificate", URL: "/ui", Tag: "security-1"}, "")
	d.Test("kiosk", "Ava")
	for {
		k, item := d.next()
		if item == nil {
			break
		}
		d.finish(k, item, d.deliver(t.Context(), item))
	}
	for kind, want := range map[string][]string{"approval": {"phone"}, "question": {"phone"}, "security": {"kiosk", "phone"}, "test": {"kiosk"}} {
		got := slices.Clone(sent[kind])
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s went to %q, want %q", kind, got, want)
		}
	}
	// Settling it tells only the devices that were asked.
	sent = map[string][]string{}
	d.Enqueue(Approval(12, "Pay £45 to the plumber", "approved", "Ava", 0), "")
	for {
		k, item := d.next()
		if item == nil {
			break
		}
		d.finish(k, item, d.deliver(t.Context(), item))
	}
	if !slices.Equal(sent["resolved"], []string{"phone"}) {
		t.Errorf("resolved went to %q", sent["resolved"])
	}
}

func TestDeclarativePreview(t *testing.T) {
	n := Approval(12, "Private plans", "pending", "Ava", 2)
	n.Origin = "https://twin.example"
	b, err := n.Payload("Ava", "private", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	json.Unmarshal(b, &p)
	notification, ok := p["notification"].(map[string]any)
	if !ok || p["web_push"] != float64(8030) || notification["navigate"] != "https://twin.example/approve/12" || notification["title"] != "Ava needs you" || notification["body"] != "Ava needs you" || p["app_badge"] != float64(2) {
		t.Fatal(p)
	}
	if bytes.Contains(b, []byte("Private plans")) || bytes.Contains(b, []byte("actions")) {
		t.Fatal("private preview exposed detail or decision action")
	}
}

func TestVAPIDConcurrentCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.pem")
	keys := make(chan string, 8)
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			v, err := LoadOrCreateVAPID(path)
			if err != nil {
				errs <- err
				return
			}
			keys <- v.PublicKey()
		}()
	}
	want := ""
	for range 8 {
		select {
		case key := <-keys:
			if want != "" && key != want {
				t.Fatal("concurrent creators got different keys")
			}
			want = key
		case err := <-errs:
			t.Fatal(err)
		}
	}
}

func TestSendRechecksDNSAtDial(t *testing.T) {
	_, sub := vector(t)
	v, err := LoadOrCreateVAPID(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	resolve := func(string) ([]netip.Addr, error) {
		calls++
		ip := "8.8.8.8"
		if calls > 1 {
			ip = "10.0.0.1"
		}
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
	s := Sender{VAPID: v, Resolve: resolve, Client: NewClient(resolve)}
	if err := s.Send(t.Context(), sub, []byte("test"), "test", ""); err == nil || calls != 2 {
		t.Fatalf("DNS rebinding not stopped before dialing: %v, lookups %d", err, calls)
	}
}
