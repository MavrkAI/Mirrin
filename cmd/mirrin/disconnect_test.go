package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
)

// `mirrin disconnect` cuts off this computer's key on the twin it was paired
// with, so the key isn't left working there.
func TestDisconnectRevokesItsOwnKey(t *testing.T) {
	cfg, srv, _ := runningTwin(t, "127.0.0.1:7742")
	d, tok, err := srv.Devices().Add("laptop", devices.KindCLI, []devices.Scope{devices.View, devices.Chat}, "loopback", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveRemote(remoteFile{Address: cfg.API.Listen, Token: tok, Name: "Mirrin", Device: d.ID}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := disconnect(&out); err != nil || !strings.Contains(out.String(), "Disconnected") {
		t.Fatalf("err %v, out %q", err, out.String())
	}
	if _, err := os.Stat(config.RemotePath()); !os.IsNotExist(err) {
		t.Fatalf("remote.yaml still there: %v", err)
	}
	_, err = api.DialErr(api.Target{Address: cfg.API.Listen, Token: tok})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("the disconnected key still works: %v", err)
	}
}
