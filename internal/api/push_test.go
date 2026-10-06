package api

import (
	"encoding/json"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/push"
)

func TestPushRoutesScopesSSRFAndRevoke(t *testing.T) {
	e := newEnv(t)
	v, err := push.LoadOrCreateVAPID(filepath.Join(t.TempDir(), "vapid.pem"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := push.Open("")
	dispatcher := push.NewDispatcher(store, nil)
	private := false
	resolve := func(string) ([]netip.Addr, error) {
		ip := "8.8.8.8"
		if private {
			ip = "10.0.0.1"
		}
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
	e.s.WithPush(v, store, dispatcher, resolve)
	dev, tok, _ := e.store.Add("Phone", devices.KindPWA, []devices.Scope{devices.Approve}, "", "")
	_, view, _ := e.store.Add("Tablet", devices.KindPWA, []devices.Scope{devices.View}, "", "")
	_, chat, _ := e.store.Add("Chat", devices.KindPWA, []devices.Scope{devices.Chat}, "", "")
	for _, test := range []struct {
		token string
		want  int
	}{{"", 401}, {chat, 403}, {tok, 200}, {view, 200}} {
		if w := e.do(onRemote, req{path: "/push/vapid", header: bearer(test.token)}); w.Code != test.want {
			t.Fatal("vapid", w.Code, w.Body.String())
		}
	}
	sub := push.Subscription{DeviceID: "forged"}
	sub.Keys.P256DH = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	sub.Keys.Auth = "BTBZMqHH6r4Tts7J_aSIgg"
	headers := bearer(tok)
	headers["Origin"] = "https://twin.example.ts.net"
	headers["Sec-Fetch-Site"] = "same-origin"
	post := func(endpoint string) int {
		sub.Endpoint = endpoint
		b, _ := json.Marshal(sub)
		return e.do(onRemote, req{method: "POST", path: "/push/subscribe", header: headers, body: string(b)}).Code
	}
	for _, endpoint := range []string{"http://169.254.169.254/", "https://example.com/"} {
		if code := post(endpoint); code != 400 {
			t.Fatal("unsafe endpoint", code)
		}
	}
	private = true
	if code := post("https://fcm.googleapis.com/a"); code != 400 {
		t.Fatal("private endpoint", code)
	}
	private = false
	if code := post("https://fcm.googleapis.com/a"); code != 200 {
		t.Fatal("subscribe", code)
	}
	if subs := store.List(); len(subs) != 1 || subs[0].DeviceID != dev.ID {
		t.Fatal("forged ownership", subs)
	}
	headers["Origin"] = "https://evil.test"
	if code := post("https://fcm.googleapis.com/b"); code != 403 {
		t.Fatal("cross-origin subscribe", code)
	}
	headers["Origin"] = "https://twin.example.ts.net"
	if w := e.do(onRemote, req{method: "POST", path: "/push/test", header: headers}); w.Code != 200 {
		t.Fatal("test push", w.Code)
	}
	e.store.Revoke(dev.ID)
	if len(store.List()) != 0 {
		t.Fatal("revoke retained subscription")
	}
}
