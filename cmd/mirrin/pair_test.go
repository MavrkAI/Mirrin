package main

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
)

type twinStub struct{}

func (twinStub) Status(context.Context) api.Status { return api.Status{Name: "Mirrin"} }
func (twinStub) Message(context.Context, channels.Inbound) (string, error) {
	return "hi", nil
}
func (twinStub) MessageStreaming(context.Context, channels.Inbound, func(string)) (string, error) {
	return "hi", nil
}
func (twinStub) MessageEvents(context.Context, channels.Inbound, agent.Events) (string, error) {
	return "hi", nil
}
func (twinStub) SetPaused(bool)                            {}
func (twinStub) RunProtocol(context.Context, string) error { return nil }
func (twinStub) RunJob(context.Context, string) error      { return nil }

// runningTwin serves a twin on a loopback port whose api.listen claims
// listen (so pairing links carry that address), and returns a config that
// reaches it.
func runningTwin(t *testing.T, listen string) (*config.Config, *api.Server, string) {
	t.Helper()
	home(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	master, err := api.LoadOrCreateToken(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := devices.Open(devices.Path(cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(listen, master, twinStub{}).WithName("Mirrin").WithDevices(store)
	if listen != "127.0.0.1:7742" {
		srv.AllowRemote()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln, api.LoopbackOnly, "loopback"); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	cfg.API.Listen = ln.Addr().String()
	return cfg, srv, master
}

// The pairing code and link never carry the master key (data/api.token).
func TestPairNeverPrintsTheMasterKey(t *testing.T) {
	cfg, _, master := runningTwin(t, "100.64.0.1:7742")
	ctx := context.Background()
	for _, args := range [][]string{nil, {"--screen"}, {"--screen", "--no-qr"}, {"--kiosk"}, {"--host", "http://twin.lan:7742"}} {
		var out strings.Builder
		if err := pairCmd(ctx, cfg, args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		s := out.String()
		if strings.Contains(s, master) {
			t.Fatalf("%v printed the master key:\n%s", args, s)
		}
		for _, f := range strings.Fields(s) {
			if b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(f, api.CodePrefix)); err == nil && strings.Contains(string(b), master) {
				t.Fatalf("%v: %q hides the master key", args, f)
			}
		}
		switch {
		case len(args) == 0:
			if !strings.Contains(s, "mirrin connect ab2.") || !strings.Contains(s, "works once") || !strings.Contains(s, "plain HTTP") || !strings.Contains(s, "mirrin pair --screen") {
				t.Fatalf("terminal code output:\n%s", s)
			}
			code := strings.Fields(s[strings.Index(s, "mirrin connect ")+len("mirrin connect "):])[0]
			c, err := api.DecodeCode(code)
			if err != nil || len(c.URLs) != 1 || c.URLs[0] != "http://100.64.0.1:7742" || c.Name != "Mirrin" {
				t.Fatalf("code %+v %v", c, err)
			}
		case args[0] == "--host":
			code := strings.Fields(s[strings.Index(s, "mirrin connect ")+len("mirrin connect "):])[0]
			if c, _ := api.DecodeCode(code); len(c.URLs) != 1 || c.URLs[0] != "http://twin.lan:7742" {
				t.Fatalf("--host code %+v", c)
			}
		default:
			if !strings.Contains(s, "http://100.64.0.1:7742/pair#v=2&o=of_") {
				t.Fatalf("%v output:\n%s", args, s)
			}
			if (len(args) == 1) != strings.Contains(s, "▀") { // a QR code unless --no-qr
				t.Fatalf("%v: QR code shown %v:\n%s", args, strings.Contains(s, "▀"), s)
			}
			if args[0] == "--kiosk" && !strings.Contains(s, "wall screen will be able to see the screen.") {
				t.Fatalf("kiosk output:\n%s", s)
			}
		}
	}
}

func TestPairExplainsWhatsMissing(t *testing.T) {
	cfg, _, _ := runningTwin(t, "127.0.0.1:7742")
	var out strings.Builder
	if err := pairCmd(context.Background(), cfg, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "can't reach Mirrin yet") || !strings.Contains(out.String(), "api.remote: true") {
		t.Fatalf("loopback-only twin:\n%s", out.String())
	}
	cfg.API.Listen = "127.0.0.1:1"
	if err := pairCmd(context.Background(), cfg, nil, &out); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Fatalf("no twin running: %v", err)
	}
	cfg.API.Listen = ""
	if err := pairCmd(context.Background(), cfg, nil, &out); err == nil || !strings.Contains(err.Error(), "api.listen") {
		t.Fatalf("API off: %v", err)
	}
	if err := pairCmd(context.Background(), cfg, []string{"--scopes", "root"}, &out); err == nil || !strings.Contains(err.Error(), "isn't a scope") {
		t.Fatalf("bad scope: %v", err)
	}
}

// End to end: pair, connect with the code, talk, revoke, and the terminal
// says plainly it was cut off.
func TestConnectWithACodeThenRevoke(t *testing.T) {
	cfg, srv, master := runningTwin(t, "100.64.0.1:7742")
	ctx := context.Background()
	var out strings.Builder
	if err := pairCmd(ctx, cfg, []string{"--host", "http://" + cfg.API.Listen}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	code := strings.Fields(s[strings.Index(s, "mirrin connect ")+len("mirrin connect "):])[0]
	out.Reset()
	if err := connectCmd(ctx, []string{"--name", "Test laptop", code}, &out); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !strings.Contains(out.String(), `as "Test laptop"`) {
		t.Fatalf("connect output %q", out.String())
	}
	r, ok := loadRemote()
	if !ok || !devices.LooksLikeToken(r.Token) || r.Token == master || r.Device == "" {
		t.Fatalf("remote.yaml %+v", r)
	}
	if fi, err := os.Stat(config.RemotePath()); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("remote.yaml mode: %v %v", fi, err)
	}
	// The old loader still reads it.
	if old := config.LoadRemote(); old == nil || old.Token != r.Token {
		t.Fatalf("config.LoadRemote: %+v", old)
	}
	if c := pairedClient(); c == nil {
		t.Fatal("paired client didn't connect")
	}
	// The same code again is refused plainly.
	if err := connectCmd(ctx, []string{code}, &out); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("code twice: %v", err)
	}
	out.Reset()
	if err := devicesCmd(ctx, cfg, nil, &out); err != nil || !strings.Contains(out.String(), "Test laptop") || !strings.Contains(out.String(), r.Device[:8]) {
		t.Fatalf("devices: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := devicesCmd(ctx, cfg, []string{"rename", r.Device[:6], "Kitchen", "laptop"}, &out); err != nil || !strings.Contains(out.String(), "Kitchen laptop") {
		t.Fatalf("rename: %v %s", err, out.String())
	}
	out.Reset()
	if err := devicesCmd(ctx, cfg, []string{"revoke", r.Device[:6]}, &out); err != nil || !strings.Contains(out.String(), "can't reach Mirrin any more") {
		t.Fatalf("revoke: %v %s", err, out.String())
	}
	if _, ok := srv.Devices().Authenticate(r.Token); ok {
		t.Fatal("revoked through the CLI, but the twin still takes the key")
	}
	if c := pairedClient(); c != nil {
		t.Fatal("a revoked terminal still connected")
	}
}

func TestOldStyleCodeStillConnectsWithAWarning(t *testing.T) {
	cfg, _, master := runningTwin(t, "127.0.0.1:7742")
	code := base64.RawURLEncoding.EncodeToString([]byte(cfg.API.Listen + "|" + master + "|Mirrin"))
	var out strings.Builder
	if err := connectCmd(context.Background(), []string{code}, &out); err != nil {
		t.Fatalf("legacy code: %v", err)
	}
	if !strings.Contains(out.String(), "old-style pairing code") || !strings.Contains(out.String(), "Paired with Mirrin") {
		t.Fatalf("output %q", out.String())
	}
	if r, ok := loadRemote(); !ok || r.Address != cfg.API.Listen {
		t.Fatalf("remote %+v", r)
	}
	if err := connectCmd(context.Background(), []string{"hello"}, &out); err == nil || !strings.Contains(err.Error(), "doesn't look like a pairing code") {
		t.Fatalf("garbage: %v", err)
	}
	if err := connectCmd(context.Background(), nil, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("no code: %v", err)
	}
}

func TestDevicesWorkWithTheTwinStopped(t *testing.T) {
	home(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.API.Listen = "127.0.0.1:1" // nothing there
	store, _ := devices.Open(devices.Path(cfg.DataDir))
	d, tok, _ := store.Add("Old phone", devices.KindPWA, nil, "", "")
	_, _, _ = store.AddLocal("Safari")
	var out strings.Builder
	if err := devicesCmd(context.Background(), cfg, nil, &out); err != nil || !strings.Contains(out.String(), "Old phone") || !strings.Contains(out.String(), "1 browser(s) on this computer") {
		t.Fatalf("list: %v\n%s", err, out.String())
	}
	if err := devicesCmd(context.Background(), cfg, []string{"revoke", d.ID}, &out); err != nil {
		t.Fatal(err)
	}
	again, _ := devices.Open(devices.Path(cfg.DataDir))
	if _, ok := again.Authenticate(tok); ok {
		t.Fatal("revoke with the twin stopped didn't stick")
	}
	if err := devicesCmd(context.Background(), cfg, []string{"revoke", "zzzz"}, &out); err == nil {
		t.Fatal("revoked a device that doesn't exist")
	}
}

// A device that came in with the old shared key, revoked while the twin is
// stopped, takes that key with it: the twin starts with a new one.
func TestRevokingASharedKeyDeviceWithTheTwinStopped(t *testing.T) {
	home(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.API.Listen = "127.0.0.1:1" // nothing there
	master, _ := api.LoadOrCreateToken(cfg.DataDir)
	store, _ := devices.Open(devices.Path(cfg.DataDir))
	d, _, _ := store.AddShared("Chrome on Linux", devices.KindPWA, "lan", "192.168.1.9")
	var out strings.Builder
	if err := devicesCmd(context.Background(), cfg, []string{"revoke", d.ID[:8]}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "old shared key") || !strings.Contains(out.String(), "pairing again") {
		t.Fatalf("output %s", out.String())
	}
	if now, _ := api.LoadOrCreateToken(cfg.DataDir); now == master {
		t.Fatal("the master key wasn't changed")
	}
}

// Regression: a hand-edited devices.json with a short id made `mirrin
// devices` panic; and the listing spoke in kind codes (pwa, cli, kiosk).
func TestDeviceListIsPlainAndSurvivesHandEdits(t *testing.T) {
	home(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.API.Listen = "127.0.0.1:1"
	f := `{"version":1,"devices":[
 {"id":"abc","name":"Hand edited","kind":"pwa","scopes":["view"],"created":"2026-01-01T00:00:00Z"},
 {"id":"0123456789abcdef","name":"Kitchen screen","kind":"kiosk","scopes":["view"],"created":"2026-01-01T00:00:00Z"},
 {"id":"fedcba9876543210","name":"Work laptop","kind":"cli","scopes":["view","chat","approve"],"created":"2026-01-02T00:00:00Z"}]}`
	if err := os.WriteFile(devices.Path(cfg.DataDir), []byte(f), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := devicesCmd(context.Background(), cfg, nil, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"01234567", "wall screen", "can see the screen;", "fedcba98", "terminal", "can see the screen, talk and approve requests"} {
		if !strings.Contains(s, want) {
			t.Errorf("listing lacks %q:\n%s", want, s)
		}
	}
	for _, jargon := range []string{" pwa ", " kiosk ", " cli ", "view,chat"} {
		if strings.Contains(s, jargon) {
			t.Errorf("listing says %q:\n%s", jargon, s)
		}
	}
	// Whatever the file held, the listing never slices past an id's end.
	out.Reset()
	printDevices(&out, []devices.Device{{ID: "abc", Name: "Restored", Kind: devices.KindPWA}})
	if !strings.Contains(out.String(), "abc  Restored") {
		t.Fatalf("short id listing:\n%s", out.String())
	}
}

// Regression (devices merged with backup): a revoke with the twin stopped
// ran in this process, where no backup hook fires, so the newest encrypted
// snapshot kept the revoked device until the next 03:30 (a restore meanwhile
// would bring it back). It now asks for a snapshot as soon as the twin runs.
func TestRevokeWithTheTwinStoppedAsksForABackup(t *testing.T) {
	home(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.API.Listen = "127.0.0.1:1" // nothing there
	cfg.Backup = config.Backup{Recipient: "age1example", RecoveryPub: "pub", Target: "folder", Path: t.TempDir()}
	cfg.Channels.WhatsApp.Enabled = false
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	store, _ := devices.Open(devices.Path(cfg.DataDir))
	d, _, _ := store.Add("Old phone", devices.KindPWA, nil, "", "")
	night := &backup.Scheduler{DataDir: cfg.DataDir, Loc: time.UTC}
	_ = backup.UpdateState(cfg.DataDir, func(s *backup.State) { s.LastAttempt, s.LastGood = time.Now(), time.Now() })
	if night.Due() {
		t.Fatal("setup: a snapshot was just taken")
	}
	var out strings.Builder
	if err := devicesCmd(context.Background(), cfg, []string{"revoke", d.ID}, &out); err != nil {
		t.Fatal(err)
	}
	if !night.Due() {
		t.Fatal("a revoke with the twin stopped didn't ask for a snapshot at the next start")
	}
	_ = backup.UpdateState(cfg.DataDir, func(s *backup.State) { s.LastAttempt = time.Now().Add(time.Second) })
	if night.Due() {
		t.Fatal("once taken, it isn't asked for again")
	}
}
