package reach

import (
	"crypto/tls"
	"errors"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"context"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"sync/atomic"
	"testing"
	"time"
)

func TestModesAndPorts(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	for _, os := range []string{"darwin", "windows", "linux"} {
		want := "100.64.0.1:443"
		if os == "linux" {
			want = "100.64.0.1:7743"
		}
		if got := DefaultListen(os, "100.64.0.1"); got != want {
			t.Fatal(got)
		}
	}
	for _, mode := range []string{"", "off", "relay", "cloud"} {
		err := Serve(context.Background(), nil, Config{Mode: mode})
		if (mode == "relay" || mode == "cloud") != (err != nil) {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}
func TestLegacyHealth(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	m := health.New()
	Start(context.Background(), api.New("", "", nil), Config{}, true, m)
	r := m.Run(context.Background())
	if len(r.Results) != 1 || r.Results[0].State != health.Warn || r.Results[0].Fix != "Run `mirrin reach use tailscale` for HTTPS" {
		t.Fatalf("%+v", r)
	}
}

func TestKeepAwakeOnPowerOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	started := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)
	var ac atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- stayAwake(ctx, ac.Load, func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			stopped <- struct{}{}
			return nil
		}, ticks)
	}()
	ticks <- time.Now()
	select {
	case <-started:
		t.Fatal("inhibited on battery")
	default:
	}
	ac.Store(true)
	ticks <- time.Now()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("did not inhibit on AC")
	}
	ac.Store(false)
	ticks <- time.Now()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("did not release on battery")
	}
	ac.Store(true)
	ticks <- time.Now()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("did not stop")
	}
	<-stopped
}

func TestBindCapability(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{{"CapEff:\t0000000000000400\n", true}, {"CapEff:\t0000000000000000\n", false}, {"CapEff: bad input", false}, {"", false}} {
		if got := bindCapability([]byte(tc.status)); got != tc.want {
			t.Fatalf("%q: %v", tc.status, got)
		}
	}
	if got := DefaultListen("linux", "100.64.0.1", true); got != "100.64.0.1:443" {
		t.Fatal(got)
	}
}

func TestReachConfigRoundTrip(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	cfg := config.Default()
	cfg.Reach = Config{Mode: "files", Listen: ":7743", CertFile: "cert.pem", KeyFile: "key.pem", StepUp: "dangerous", AdminRemote: true, StayAwake: true}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Reach != cfg.Reach {
		t.Fatalf("%+v", loaded.Reach)
	}
	if config.Default().Reach.Mode != "off" || config.Default().Reach.StepUp != "dangerous" {
		t.Fatal("unsafe defaults")
	}
}

func TestPowerSupplyOnAC(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"desktop", nil, true}, {"battery", map[string]string{"BAT0/type": "Battery"}, false},
		{"plugged", map[string]string{"BAT0/type": "Battery", "AC/type": "Mains", "AC/online": "1\n"}, true},
		{"unplugged", map[string]string{"BAT0/type": "Battery", "AC/type": "Mains", "AC/online": "0\n"}, false},
		{"unknown mains", map[string]string{"AC/type": "Mains"}, false},
		{"usb power", map[string]string{"USB/type": "USB_C", "USB/online": "1"}, true},
		{"unrelated", map[string]string{"device/type": "Other"}, true},
		{"desktop with wireless mouse", map[string]string{"hidpp_battery_0/type": "Battery", "hidpp_battery_0/scope": "Device\n"}, true},
		{"laptop with wireless mouse", map[string]string{"BAT0/type": "Battery", "hidpp_battery_0/type": "Battery", "hidpp_battery_0/scope": "Device"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, value := range tc.files {
				p := filepath.Join(root, name)
				os.MkdirAll(filepath.Dir(p), 0700)
				os.WriteFile(p, []byte(value), 0600)
			}
			if got := powerSupplyOnAC(root); got != tc.want {
				t.Fatalf("%v", got)
			}
		})
	}
	if powerSupplyOnAC(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("unreadable state treated as mains")
	}
}

type retrySource struct {
	active  atomic.Int32
	warning error
}

func (s *retrySource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return nil, errors.New("unused handshake")
}
func (s *retrySource) Hostnames() []string { return []string{"twin.test"} }
func (s *retrySource) SPKIs() []string     { return nil }
func (s *retrySource) Run(ctx context.Context) error {
	s.active.Add(1)
	defer s.active.Add(-1)
	<-ctx.Done()
	return nil
}
func (s *retrySource) Warning() error { return s.warning }

type idleListener struct {
	done chan struct{}
	once sync.Once
}

func (l *idleListener) Accept() (net.Conn, error) { <-l.done; return nil, net.ErrClosed }
func (l *idleListener) Close() error              { l.once.Do(func() { close(l.done) }); return nil }
func (l *idleListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(100, 64, 0, 1), Port: 7743} }
func TestReachRetriesStartupAndListen(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &retrySource{}
	attempts, listens := 0, 0
	var delays []time.Duration
	var states []snapshot
	d := dependencies{prepare: func(context.Context, Config) (tlsmgr.Source, string, error) {
		attempts++
		switch attempts {
		case 1:
			return nil, "", &tailscale.Problem{Message: "Tailscale is still starting"}
		case 2:
			return nil, "", errors.New("no TailscaleIPs")
		}
		return src, "100.64.0.1:7743", nil
	}, listen: func(string, string) (net.Listener, error) {
		listens++
		if listens == 1 {
			return nil, errors.New("bind: permission denied")
		}
		return &idleListener{done: make(chan struct{})}, nil
	}, wait: func(_ context.Context, d time.Duration) bool { delays = append(delays, d); return true }}
	err := supervise(ctx, api.New("", "", nil), Config{Mode: "tailscale"}, func(v snapshot) {
		states = append(states, v)
		if v.state == health.OK {
			cancel()
		}
	}, d)
	if err != nil || attempts != 4 || listens != 2 || src.active.Load() != 0 {
		t.Fatalf("%v attempts %d listens %d active %d", err, attempts, listens, src.active.Load())
	}
	if !reflect.DeepEqual(delays, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Fatal(delays)
	}
	last := states[len(states)-1]
	if last.state != health.OK || !strings.Contains(last.message, "https://twin.test:7743") {
		t.Fatalf("%+v", last)
	}
	for _, v := range states {
		if strings.Contains(v.message, "bind:") {
			t.Fatal("raw error in health", v)
		}
	}
}
func TestReachCancelsBackoffAndReportsRenewal(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	err := supervise(ctx, nil, Config{Mode: "files"}, nil, dependencies{prepare: func(context.Context, Config) (tlsmgr.Source, string, error) {
		attempts++
		return nil, "", errors.New("missing file")
	}, wait: func(ctx context.Context, d time.Duration) bool { cancel(); return pause(ctx, time.Hour) }})
	if err != nil || attempts != 1 {
		t.Fatal(err, attempts)
	}
	src := &retrySource{warning: errors.New("renewal will retry")}
	v := snapshot{state: health.OK, message: "https://twin.test", source: src}
	if state, msg, _ := healthSnapshot(v, false); state != health.Warn || msg != "renewal will retry" {
		t.Fatal(state, msg)
	}
	src.warning = nil
	if state, _, _ := healthSnapshot(v, false); state != health.OK {
		t.Fatal(state)
	}
}
