package cloud_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/cloud/cloudtest"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/relay"
)

// Zero egress: a twin on the default config lives through two days and
// sends nothing to Mirrin Cloud or its relays, not even a lookup of their
// names. Together with the boundary test this is the no-crippling
// guarantee: a twin that never linked has nothing to ask and nobody to ask
// it of.
//
// It lives them twice. A busy morning in real time, with the local API on a
// loopback port: start-up, the introduction, a conversation, each job run
// by hand, the presence screen. Then two days of fake time in a
// testing/synctest bubble, where every timer the daemon sets fires as it
// would: the hourly self-checks, the reminder tick, each scheduled job, and
// anything else on a timer, such as a future deny-list poll. The API is off
// in the bubble, because a listening socket would keep its clock from
// moving; it makes no requests of its own.
func TestDefaultDaemonSendsNothingToCloud(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config func(*config.Config)
	}{
		{"default config", func(*config.Config) {}},
		{"cloud api configured but never linked", func(c *config.Config) { c.Cloud.API = cloud.DefaultAPI }},
	} {
		for _, day := range []struct {
			name string
			live func(*testing.T, *config.Config, *fakeModel)
		}{
			{"a busy morning", liveAMorning},
			{"two days of fake time", liveTwoDays},
		} {
			t.Run(tc.name+", "+day.name, func(t *testing.T) {
				cfg, model := hermeticDefaultConfig(t)
				tc.config(cfg)
				g := watchEgress(t)
				day.live(t, cfg, model)
				g.check(t)
				if _, err := os.Stat(filepath.Join(cfg.DataDir, "cloud")); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the default config left data/cloud behind: %v", err)
				}
				if es, _, _ := cloud.ReadLedger(cfg.DataDir); len(es) != 0 {
					t.Errorf("the egress ledger has %d entries", len(es))
				}
			})
		}
	}
}

// The positive controls below aim at names under .invalid (RFC 6761),
// which never resolve, so a guard that failed open would still send
// nothing anywhere. So does the fake model, which the guard answers.
const (
	testAPI   = "https://cloud.mirrin.invalid"
	testRelay = "r1.relay.mirrin.invalid"
	modelHost = "model.invalid"
)

// The guard is not blind: the cloud client's requests go through it.
func TestGuardSeesTheCloudClient(t *testing.T) {
	g := watchEgress(t, "cloud.mirrin.invalid")
	dataDir := t.TempDir()
	c, err := cloud.New(dataDir, testAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Transport != nil || http.DefaultTransport != http.RoundTripper(guard) {
		t.Fatal("the cloud client does not send through the guarded default transport")
	}
	if _, err := c.StartLink(t.Context(), "", ""); !errors.Is(err, errRefusedByGuard) {
		t.Fatalf("StartLink under the guard: %v", err)
	}
	if !g.saw("POST " + testAPI + "/v1/link/start") {
		t.Fatal("the guard did not see the cloud client's request")
	}
	// Refused before it left, and still in the ledger.
	if es, _, _ := cloud.ReadLedger(dataDir); len(es) != 1 || es[0].Status != 0 {
		t.Fatalf("ledger: %+v", es)
	}
}

// Nor is it blind to a relay tunnel, which dials with its own transport:
// the name lookup gives it away.
func TestGuardSeesARelayTunnel(t *testing.T) {
	g := watchEgress(t, testRelay)
	if !g.dnsWatched {
		t.Skip("DNS lookups bypass the guard on this platform")
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	tunnel := "wss://" + testRelay + "/v1/tunnel"
	l, err := relay.Listen(t.Context(), relay.ClientConfig{Relays: []relay.RelayRef{{ID: "r1", URL: tunnel}}, Key: key, StatusKey: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	waitFor(t, 10*time.Second, "the relay lookup", func() bool { return g.saw("DNS " + testRelay) })
}

// The guard counts the real names as Cloud, however they are written.
func TestGuardKnowsTheCloudHosts(t *testing.T) {
	g := &egressGuard{}
	for host, want := range map[string]bool{
		"cloud.mirrin.app":                 true,
		"CLOUD.mirrin.app:443":             true,
		"r1.relay.mirrin.app.":             true,
		"r1.relay.mirrin.app.corp.example": true, // a search domain appended
		"ember-otter-42.mirrin.link":       true,
		"mirrin.app":                       true,
		"example.com":                      false,
		"mirrin.application.example":       false,
		"notmirrin.app":                    false,
		"127.0.0.1:7742":                   false,
		"model.invalid":                    false,
	} {
		if got := g.isCloud(host); got != want {
			t.Errorf("isCloud(%q) = %v, want %v", host, got, want)
		}
	}
}

// And a daemon on a linked machine is caught: its daily refresh is due, so
// it contacts the control plane at start, and the guard sees it. This is
// what the zero-egress test would report if the daemon ever talked to
// Cloud without a link.
func TestGuardSeesALinkedDaemon(t *testing.T) {
	cfg, _ := hermeticDefaultConfig(t)
	f := cloudtest.NewFake(t)
	twoDaysAgo := func() time.Time { return time.Now().Add(-48 * time.Hour) }
	f.SetClock(twoDaysAgo) // linked two days ago, so a refresh is due
	c, err := cloud.New(cfg.DataDir, f.URL, f.Keys())
	if err != nil {
		t.Fatal(err)
	}
	c.Now = twoDaysAgo
	ls, err := c.StartLink(t.Context(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.Pay(ls.ID)
	if st, _, err := c.PollLink(t.Context(), ls.ID); err != nil || st != cloud.LinkActive {
		t.Fatalf("link: %q %v", st, err)
	}
	f.SetClock(time.Now)

	g := watchEgress(t, strings.TrimPrefix(f.URL, "http://"))
	ctx, cancel := context.WithCancel(t.Context())
	_, done := startDaemon(t, ctx, cfg)
	waitFor(t, 10*time.Second, "the linked daemon's refresh", func() bool { return g.saw("/v1/entitlement/refresh") })
	cancel()
	<-done
}

// The two days of fake time are real days to the daemon's timers: a
// machine linked at the start refreshes on its own a day or so later, and
// again after that, and the guard sees each one. The zero-egress run would
// see a timer-driven request to Cloud the same way.
func TestGuardSeesALinkedDaemonThroughTwoDays(t *testing.T) {
	cfg, model := hermeticDefaultConfig(t)
	cfg.API.Listen = ""
	f := cloudtest.NewFake(t)
	host := strings.TrimPrefix(f.URL, "http://")
	guard.serve(t, host, f.Handler())
	synctest.Test(t, func(t *testing.T) {
		c, err := cloud.New(cfg.DataDir, f.URL, f.Keys())
		if err != nil {
			t.Fatal(err)
		}
		ls, err := c.StartLink(t.Context(), "", "")
		if err != nil {
			t.Fatal(err)
		}
		f.Pay(ls.ID)
		if st, _, err := c.PollLink(t.Context(), ls.ID); err != nil || st != cloud.LinkActive {
			t.Fatalf("link: %q %v", st, err)
		}
		linked := time.Now()
		g := watchEgress(t, host)
		ctx, cancel := context.WithCancel(t.Context())
		_, done := startDaemon(t, ctx, cfg)
		time.Sleep(2*(20*time.Hour+8*time.Hour) + time.Hour) // two refresh windows
		cancel()
		<-done
		at := g.sawAt("POST " + f.URL + "/v1/entitlement/refresh")
		var after []string
		for _, a := range at {
			after = append(after, a.Sub(linked).Round(time.Minute).String())
		}
		t.Logf("refreshes after linking: %s", strings.Join(after, ", "))
		if len(at) < 2 {
			t.Fatalf("%d refreshes seen in %v of fake time, want two or more", len(at), time.Since(linked))
		}
		if first := at[0].Sub(linked); first < 20*time.Hour {
			t.Errorf("the first refresh came %v after linking, want a day or so", first)
		}
		if n := model.listed.Load(); n < 48 {
			t.Errorf("the hourly self-check ran %d times, want 48 or more", n)
		}
	})
}

// hermeticDefaultConfig is config.Default() with only what a test must
// change to stay on this machine, none of it about Cloud. It returns the
// fake model it points at, which the guard answers in memory.
func hermeticDefaultConfig(t *testing.T) (*config.Config, *fakeModel) {
	t.Helper()
	t.Setenv("MIRRIN_HOME", t.TempDir())
	model := &fakeModel{}
	guard.serve(t, modelHost, model)
	cfg := config.Default()
	cfg.API.Listen = "127.0.0.1:0"        // never the real port
	cfg.Channels.WhatsApp.Enabled = false // as on first run; it would dial WhatsApp
	cfg.Skills.Browser.Enabled = false    // no Chrome and no keychain in tests
	cfg.LLM.Provider, cfg.LLM.Model = "ollama", "fake"
	cfg.LLM.Providers["ollama"] = config.ProviderConfig{BaseURL: "http://" + modelHost + "/v1", Model: "fake"}
	if cfg.Cloud != config.Default().Cloud || cfg.Cloud.API != "" {
		t.Fatalf("the default config names a Cloud API: %+v", cfg.Cloud)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(config.Path()); strings.Contains(string(b), "cloud:") {
		t.Fatal("a saved default config has a cloud section")
	}
	return cfg, model
}

// startDaemon builds a headless daemon from cfg and runs it until ctx ends.
// The returned channel closes when Run has returned.
func startDaemon(t *testing.T, ctx context.Context, cfg *config.Config) (*daemon.Daemon, <-chan struct{}) {
	t.Helper()
	d, err := daemon.New(cfg, daemon.Options{Headless: true, Version: "test", Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := d.Run(ctx); err != nil {
			t.Errorf("daemon: %v", err)
		}
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	return d, done
}

// liveAMorning runs a daemon in real time and puts it through a busy
// morning: start-up and its self-checks, the first-run introduction, a
// conversation, every job the schedule runs (the first-week nudge, pattern
// spotting, the portrait) run by hand, and the presence screen, then
// shutdown.
func liveAMorning(t *testing.T, cfg *config.Config, model *fakeModel) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	d, done := startDaemon(t, ctx, cfg)
	converse(t, ctx, d)
	for _, job := range []string{"nudge", "patterns", "portrait"} {
		if err := d.RunJob(ctx, job); err != nil {
			t.Fatalf("job %s: %v", job, err)
		}
	}
	d.Screen(ctx)
	waitFor(t, 10*time.Second, "the start-up self-checks", func() bool { return model.listed.Load() > 0 })
	cancel()
	<-done
	if model.chats.Load() == 0 {
		t.Fatal("the morning made no model calls; the test is not exercising the twin")
	}
}

// liveTwoDays runs a daemon for two days and an hour of fake time in a
// synctest bubble, which starts at midnight UTC on Saturday 1 January 2000:
// long enough for every daily job (the tidy at 3:15, the nudge at 9:30, the
// patterns at 17:00) to come round twice and Sunday's portrait once, in any
// time zone. The introduction and a conversation come first.
func liveTwoDays(t *testing.T, cfg *config.Config, model *fakeModel) {
	t.Helper()
	cfg.API.Listen = ""
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		d, done := startDaemon(t, ctx, cfg)
		converse(t, ctx, d)
		start := time.Now()
		time.Sleep(49 * time.Hour)
		d.Screen(ctx)
		cancel()
		<-done
		if n := model.listed.Load(); n < 48 {
			t.Fatalf("the hourly self-check ran %d times in %v of fake time, want 48 or more; the clock did not move", n, time.Since(start))
		}
		if model.chats.Load() == 0 {
			t.Fatal("the days made no model calls; the test is not exercising the twin")
		}
	})
}

// converse has the twin introduce itself, then the owner talk to it.
func converse(t *testing.T, ctx context.Context, d *daemon.Daemon) {
	t.Helper()
	if _, err := d.FirstLook(ctx); err != nil {
		t.Fatalf("first look: %v", err)
	}
	for _, text := range []string{"Good morning. What's on today?", "Remind me to call Sam at five.", "Thanks, goodnight."} {
		if _, err := d.Message(ctx, channels.Inbound{Channel: "cli", ChatID: "terminal", Sender: "owner", Text: text, IsOwner: true}); err != nil {
			t.Fatalf("message %q: %v", text, err)
		}
	}
}

// fakeReply is long enough for every job, the portrait included.
const fakeReply = "Noted. The user likes an early start, keeps Fridays light, and would rather be asked than guessed about."

// fakeModel is an OpenAI-compatible model that answers every chat with the
// same few sentences. The guard serves it in memory.
type fakeModel struct {
	chats, listed atomic.Int64
}

func (m *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		m.listed.Add(1)
		w.Write([]byte(`{"data":[{"id":"fake"}]}`))
	case "/v1/chat/completions":
		m.chats.Add(1)
		var req struct {
			Stream bool `json:"stream"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + fakeReply + "\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"` + fakeReply + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	default:
		http.NotFound(w, r)
	}
}
