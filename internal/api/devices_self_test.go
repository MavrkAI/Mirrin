package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// A paired terminal can cut off its own key (what `mirrin disconnect` asks
// for), over the listeners other devices reach; after that its key is
// refused.
func TestDeviceRevokesItself(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	e := newEnv(t)
	offer := e.offer("cli")
	src := remoteTestSource(t)
	tr := remotePipe(t, e.s, src, RemoteOptions{Via: "files"})
	old := http.DefaultTransport
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = old })
	claimed, err := Claim(context.Background(), "https://twin.test", src.SPKIs(), offer.Offer.ID, offer.Offer.Secret, "terminal", "cli")
	if err != nil {
		t.Fatal("Claim:", err)
	}
	target := Target{Address: "https://twin.test", Token: claimed.Token, Pins: src.SPKIs()}
	if err := RevokeSelf(context.Background(), target); err != nil {
		t.Fatal("RevokeSelf:", err)
	}
	if d, _ := e.store.Get(claimed.Device.ID); !d.Revoked() {
		t.Fatal("the device wasn't revoked")
	}
	c := newClient(target)
	t.Cleanup(c.http.CloseIdleConnections)
	_, err = c.Status(context.Background())
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("the revoked key still works: %v", err)
	}
}

// The master key isn't a device: there is no key of its own to cut off.
func TestMasterKeyCantRevokeItself(t *testing.T) {
	e := newEnv(t)
	w := e.do(onLoopback, req{method: "POST", path: "/devices/self/revoke", header: bearer(master)})
	var body apiError
	if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error != "not_a_device" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
