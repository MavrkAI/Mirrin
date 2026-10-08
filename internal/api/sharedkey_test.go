package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// sharedKeyDevice moves a terminal paired with an old-style code (the
// master key, over the plain-HTTP api.remote listener) onto a key of its own.
func sharedKeyDevice(t *testing.T, e *env) devices.Device {
	t.Helper()
	w := e.do(onLegacy, req{method: "POST", path: "/pair/upgrade", body: `{"name":"Wall screen"}`, header: bearer(master)})
	var resp ClaimResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil || !resp.Device.SharedKey {
		t.Fatalf("upgrade: %d %s", w.Code, w.Body)
	}
	return resp.Device
}

// Revoking a device that came in with the master key replaces that key, so
// whoever holds it is cut off too; this computer keeps working.
func TestRevokingASharedKeyDeviceChangesTheMasterKey(t *testing.T) {
	e := newEnv(t)
	keyFile := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(keyFile, []byte(master+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.s.WithTokenFile(keyFile)
	menuLink := e.s.UIURL()
	d := sharedKeyDevice(t, e)
	// A device paired with a link of its own is revoked without touching the key.
	phone, _, _ := e.store.Add("Phone", devices.KindPWA, nil, "", "")
	w := e.do(onLoopback, req{method: "POST", path: "/devices/" + phone.ID + "/revoke", header: bearer(master)})
	var res RevokeResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.KeyChanged || !res.Device.Revoked() {
		t.Fatalf("revoke phone: %d %s", w.Code, w.Body)
	}
	if !e.s.isMaster(master, listener{kind: kindLegacy}) {
		t.Fatal("revoking a device of its own changed the master key")
	}

	w = e.do(onLoopback, req{method: "POST", path: "/devices/" + d.ID + "/revoke", header: bearer(master)})
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || !res.KeyChanged || res.KeyError != "" {
		t.Fatalf("revoke old screen: %d %s", w.Code, w.Body)
	}
	b, _ := os.ReadFile(keyFile)
	newKey := strings.TrimSpace(string(b))
	if newKey == master || len(newKey) < 32 {
		t.Fatalf("key file %q", newKey)
	}
	if fi, _ := os.Stat(keyFile); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode())
	}
	// The old key no longer works from another computer.
	if w := e.do(onLegacy, req{path: "/status", header: bearer(master)}); w.Code != 401 {
		t.Fatalf("old key over api.remote: %d", w.Code)
	}
	if w := e.do(onLegacy, req{path: "/status", header: bearer(newKey)}); w.Code != 200 {
		t.Fatalf("new key over api.remote: %d", w.Code)
	}
	// This computer: terminals read the new key, and the menu's links, made
	// with the old one, still open until the twin restarts.
	if w := e.do(onLoopback, req{path: "/status", header: bearer(newKey)}); w.Code != 200 {
		t.Fatalf("new key on loopback: %d", w.Code)
	}
	if !strings.Contains(e.s.UIURL(), newKey) {
		t.Fatalf("menu link %s", e.s.UIURL())
	}
	w = e.do(onLoopback, req{path: strings.TrimPrefix(menuLink, "http://127.0.0.1:7742")})
	if c := cookieFrom(w, cookieDev); w.Code != http.StatusFound || c == nil {
		t.Fatalf("the menu's old link: %d %v", w.Code, w.Result().Cookies())
	}
	// Revoking it again changes nothing more.
	if res, err := e.s.RevokeDevice(d.ID); err != nil || res.KeyChanged {
		t.Fatalf("second revoke: %+v %v", res, err)
	}
	if b2, _ := os.ReadFile(keyFile); string(b2) != string(b) {
		t.Fatal("a second revoke changed the key again")
	}
}

// A terminal paired with an old-style code, moved onto its own key, is
// marked the same way.
func TestUpgradedTerminalIsMarkedAsFromTheSharedKey(t *testing.T) {
	e := newEnv(t)
	w := e.do(onLegacy, req{method: "POST", path: "/pair/upgrade", body: `{"name":"Laptop"}`, header: bearer(master)})
	var resp ClaimResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil || !resp.Device.SharedKey {
		t.Fatalf("upgrade: %d %s", w.Code, w.Body)
	}
	res, err := e.s.RevokeDevice(resp.Device.ID)
	if err != nil || !res.KeyChanged {
		t.Fatalf("revoke: %+v %v", res, err)
	}
	if w := e.do(onLegacy, req{method: "POST", path: "/pair/upgrade", header: bearer(master)}); w.Code != 401 {
		t.Fatalf("the old key minted another device: %d", w.Code)
	}
}

// Regression: any device with chat could name the owner's WhatsApp chat (or
// any other) and so post into its history and answer the requests waiting
// there.
func TestPairedDevicesTalkOnlyInTheirOwnChat(t *testing.T) {
	e := newEnv(t)
	_, tok, _ := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.Chat}, "", "")
	for _, path := range []string{"/message", "/message/stream"} {
		for _, c := range []struct {
			body     string
			want     int
			key      string
			loopback bool
			tok      string
		}{
			{`{"text":"hi"}`, 200, "api:local", false, tok},
			{`{"channel":"screen","chat_id":"local","text":"hi"}`, 200, "screen:local", false, tok},
			{`{"channel":"cli","text":"hi"}`, 200, "cli:terminal", false, tok},
			{`{"channel":"voice","chat_id":"local","text":"hi"}`, 200, "voice:local", false, tok},
			{`{"channel":"whatsapp","chat_id":"61400000000@s.whatsapp.net","text":"yes 3"}`, 400, "", false, tok},
			{`{"channel":"telegram","chat_id":"42","text":"hi"}`, 400, "", false, tok},
			{`{"channel":"voice","chat_id":"phone","text":"hi"}`, 400, "", false, tok},
			{`{"channel":"cli","chat_id":"terminal#task-1","text":"hi"}`, 400, "", false, tok},
			{`{"channel":"screen","chat_id":"other","text":"hi"}`, 400, "", false, tok},
			// An old-style code's master key over api.remote is held to the same.
			{`{"channel":"whatsapp","chat_id":"61400000000@s.whatsapp.net","text":"hi"}`, 400, "", false, master},
			// This computer's master key names any chat.
			{`{"channel":"telegram","chat_id":"42","text":"hi"}`, 200, "telegram:42", true, master},
		} {
			at := onLegacy
			if c.loopback {
				at = onLoopback
			}
			w := e.do(at, req{method: "POST", path: path, body: c.body, header: bearer(c.tok)})
			if w.Code != c.want {
				t.Errorf("%s %s: %d, want %d (%s)", path, c.body, w.Code, c.want, w.Body)
				continue
			}
			if c.want == 400 {
				var body apiError
				if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error != "wrong_chat" || !strings.Contains(body.Message, "its own chat") {
					t.Errorf("%s %s: body %s", path, c.body, w.Body)
				}
				continue
			}
			if got := e.f.lastInbound().Key(); got != c.key {
				t.Errorf("%s %s: talked in %s, want %s", path, c.body, got, c.key)
			}
		}
	}
}

// Regression: with api.listen on a specific address, a program already
// holding 127.0.0.1 on the same port was silently left there, and the menu
// and terminals sent it the master key.
func TestStartReportsATakenLoopbackPort(t *testing.T) {
	ip := nonLoopbackIP(t)
	lo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lo.Close()
	_, port, _ := net.SplitHostPort(lo.Addr().String())
	e := newEnv(t)
	e.s.addr = net.JoinHostPort(ip, port)
	e.s.remote = true
	err = e.s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:"+port) || !strings.Contains(err.Error(), "another program") {
		t.Fatalf("Start: %v", err)
	}
	// It let go of the address it did get.
	if ln, err := net.Listen("tcp", e.s.addr); err != nil {
		t.Fatalf("%s still held: %v", e.s.addr, err)
	} else {
		ln.Close()
	}
}

// nonLoopbackIP is an IPv4 address of this computer's that isn't loopback.
func nonLoopbackIP(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			if ln, err := net.Listen("tcp", net.JoinHostPort(ipn.IP.String(), "0")); err == nil {
				ln.Close()
				return ipn.IP.String()
			}
		}
	}
	t.Skip("no non-loopback address to listen on")
	return ""
}

// Regression: when this browser's key couldn't be saved, a menu link failed
// with a raw 500 naming a Go error.
func TestMenuLinkSaysWhenItCantSaveTheKey(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Dir(e.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil { // running as root
		f.Close()
		os.Remove(f.Name())
		t.Skip("the folder stays writable")
	}
	w := e.do(onLoopback, req{path: "/ui?token=" + master, header: map[string]string{"Accept": "text/html"}})
	body := w.Body.String()
	if w.Code != 500 || !strings.Contains(w.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "couldn&#39;t save this browser&#39;s key in "+dir) || !strings.Contains(body, "writable") || strings.Contains(body, "permission denied") {
		t.Fatalf("%d %s", w.Code, body)
	}
}

func TestErrorPagesShowCommandsAsCode(t *testing.T) {
	e := newEnv(t)
	e.s.WithName(`Mirrin <script>alert(1)</script>`)
	w := e.do(onLegacy, req{path: "/ui", header: map[string]string{"Accept": "text/html"}})
	body := w.Body.String()
	if !strings.Contains(body, "<code>mirrin pair --screen</code>") || strings.Contains(body, "`") {
		t.Fatalf("fix text: %s", body)
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatalf("the name wasn't escaped: %s", body)
	}
	if got := codeSpans("a `b` c `d"); got != "a <code>b</code> c `d" {
		t.Fatalf("codeSpans: %s", got)
	}
}

func TestNotAllowedSuggestsTheRightPairing(t *testing.T) {
	e := newEnv(t)
	_, cli, _ := e.store.Add("Laptop", devices.KindCLI, []devices.Scope{devices.View}, "", "")
	_, phone, _ := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.View}, "", "")
	for tok, want := range map[string]string{cli: "`mirrin pair --scopes view,chat,approve`", phone: "`mirrin pair --screen --scopes view,chat,approve`"} {
		w := e.do(onLegacy, req{method: "POST", path: "/message", body: `{"text":"hi"}`, header: bearer(tok)})
		var body apiError
		if w.Code != 403 || json.Unmarshal(w.Body.Bytes(), &body) != nil || !strings.Contains(body.Fix, want) || !strings.Contains(body.Message, "see the screen only") {
			t.Errorf("%d %+v, want %s", w.Code, body, want)
		}
	}
}
