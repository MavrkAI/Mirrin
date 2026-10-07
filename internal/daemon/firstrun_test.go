package daemon

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// firstRunModel answers every request (or fails with err) and counts calls.
type firstRunModel struct {
	calls atomic.Int32
	err   error
}

func (f *firstRunModel) Name() string { return "fake/model" }
func (f *firstRunModel) Complete(context.Context, llm.Request) (*llm.Response, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &llm.Response{Message: llm.Text(llm.RoleAssistant, "Hello."), StopReason: llm.StopEndTurn}, nil
}

func newFirstRunDaemon(t *testing.T, change func(*config.Config)) (*Daemon, *firstRunModel) {
	t.Helper()
	cfg := newFirstRunConfig(t, func(c *config.Config) {
		c.LLM.APIKey = "test-key"
		if change != nil {
			change(c)
		}
	})
	d := newFirstRunDaemonFrom(t, cfg, Options{Headless: true})
	fake := &firstRunModel{}
	d.agent.SetProvider(fake)
	return d, fake
}

// newFirstRunConfig saves a quiet test config in a fresh home.
func newFirstRunConfig(t *testing.T, change func(*config.Config)) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	t.Setenv("PATH", t.TempDir())                 // no desktop notifications or helper programs from tests
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir()) // and no `ant auth login` profile
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv(config.ServiceEnv, "")
	t.Setenv("XPC_SERVICE_NAME", "")
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Skills.Browser.Enabled = false
	cfg.DataDir = filepath.Join(home, "data")
	cfg.ProtocolsDir = filepath.Join(home, "protocols")
	cfg.API.Listen = ""
	if change != nil {
		change(cfg)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newFirstRunDaemonFrom(t *testing.T, cfg *config.Config, opts Options) *Daemon {
	t.Helper()
	opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// runFirstRun runs d until the test ends and returns what Run returned.
func runFirstRun(t *testing.T, d *Daemon) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done, finished := make(chan error, 1), make(chan struct{})
	go func() {
		done <- d.Run(ctx)
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
		}
	})
	return cancel, done
}

func waitFirstRun(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func firstRunResult(t *testing.T, rep health.Report, name string) health.Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %q check in %+v", name, rep.Results)
	return health.Result{}
}

func TestRunHealthRunsEachCheckOnce(t *testing.T) {
	d, fake := newFirstRunDaemon(t, nil)
	defer d.Close()
	rep := d.RunHealth(context.Background())
	if r := firstRunResult(t, rep, "model"); r.State != health.OK {
		t.Fatalf("model: %+v", r)
	}
	time.Sleep(300 * time.Millisecond) // a background round would land by now
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("model probed %d times for one doctor run", n)
	}
}

func TestDoctorWithoutARunningTwinClaimsNothing(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	d, _ := newFirstRunDaemon(t, func(c *config.Config) {
		c.API.Listen = taken.Addr().String()
		c.Channels.Telegram.Enabled = true
		c.Channels.Telegram.Token = "123:FAKE"
		c.Channels.Telegram.Owner = "42"
	})
	defer d.Close()
	rep := d.RunHealth(context.Background())
	if r := firstRunResult(t, rep, "api"); r.State != health.Fail || !strings.Contains(r.Detail, "another program") {
		t.Fatalf("api with the port held by something else: %+v", r)
	}
	if r := firstRunResult(t, rep, "channels"); r.State == health.OK {
		t.Fatalf("channels reported connected with no twin running: %+v", r)
	}

	taken.Close()
	if r := firstRunResult(t, d.RunHealth(context.Background()), "api"); r.State != health.Off || r.Detail != "not running" {
		t.Fatalf("api with nothing running: %+v", r)
	}
}

func TestTakenAPIPortDoesNotStopTheTwin(t *testing.T) {
	defer func(d time.Duration) { apiRetry = d }(apiRetry)
	apiRetry = 50 * time.Millisecond
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	d, _ := newFirstRunDaemon(t, func(c *config.Config) { c.API.Listen = taken.Addr().String() })
	cancel, done := runFirstRun(t, d)
	select {
	case err := <-done:
		t.Fatalf("Run stopped because the API port was taken: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	st, detail, fix := d.apiHealth(context.Background())
	if st != health.Fail || !strings.Contains(detail, "another program is using it") || !strings.Contains(fix, "try again") {
		t.Fatalf("api health = %s %q %q", st, detail, fix)
	}

	// Once the other program lets go, the API comes up without a restart.
	taken.Close()
	waitFirstRun(t, "the local API to come back", func() bool { st, _, _ := d.apiHealth(context.Background()); return st == health.OK })
	waitFirstRun(t, "the health page to catch up", func() bool {
		for _, r := range d.Health().Results {
			if r.Name == "api" {
				return r.State == health.OK
			}
		}
		return false
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestAPISettingProblemIsNotBlamedOnAnotherProgram(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	_, inUse := net.Listen("tcp", taken.Addr().String())
	cases := []struct {
		err       error
		inUse     bool
		fixSays   string
		fixDoesnt string
	}{
		{inUse, true, "quit the other program using", "check api.listen"},
		{errors.New("api.listen must be a loopback address unless api.remote is true"), false, "check api.listen", "quit the other program"},
		{errors.New(`api listen "nope": missing port in address`), false, "check api.listen", "quit the other program"},
	}
	for _, c := range cases {
		if addrInUse(c.err) != c.inUse {
			t.Errorf("addrInUse(%v) = %v", c.err, !c.inUse)
		}
		d, _ := newFirstRunDaemon(t, func(cfg *config.Config) { cfg.API.Listen = "127.0.0.1:1" })
		d.apiFailed(c.err)
		_, detail, fix := d.apiHealth(context.Background())
		if !strings.Contains(fix, c.fixSays) || strings.Contains(fix, c.fixDoesnt) {
			t.Errorf("%v: fix %q (detail %q)", c.err, fix, detail)
		}
		d.Close()
	}
}

func TestOneTwinPerHome(t *testing.T) {
	defer func(g time.Duration) { claimGrace = g }(claimGrace)
	claimGrace = 100 * time.Millisecond
	first, _ := newFirstRunDaemon(t, nil)
	stopFirst, firstDone := runFirstRun(t, first)
	waitFirstRun(t, "the first twin to claim its home", func() bool {
		first.instMu.Lock()
		defer first.instMu.Unlock()
		return first.instance != nil
	})

	// A second copy (double-clicking the app, `mirrin run` in a terminal) says so and stops.
	cfg := first.Config()
	second := newFirstRunDaemonFrom(t, &cfg, Options{Headless: true})
	_, secondDone := runFirstRun(t, second)
	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("second copy: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a second twin started from the same home")
	}

	// The service manager's copy waits for the other one to quit, then takes over.
	t.Setenv(config.ServiceEnv, "1")
	cfg = first.Config()
	service := newFirstRunDaemonFrom(t, &cfg, Options{Headless: true})
	_, serviceDone := runFirstRun(t, service)
	select {
	case err := <-serviceDone:
		t.Fatalf("the service's copy gave up instead of waiting: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	stopFirst()
	<-firstDone
	waitFirstRun(t, "the service's copy to take over", func() bool {
		service.instMu.Lock()
		defer service.instMu.Unlock()
		return service.instance != nil
	})
}

// The healed model runs with the settings on disk, not only the key: the
// agent keeps its own copy of the config (turn.go), so healing hands it the
// new one as UpdateConfig does.
func TestHealedModelUsesTheSettingsOnDisk(t *testing.T) {
	var mu sync.Mutex
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = string(b)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello."},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	t.Setenv("OPENAI_API_KEY", "")
	cfg := newFirstRunConfig(t, func(c *config.Config) {
		c.LLM.Provider, c.LLM.Model = "openai", "gpt-4.1"
		c.LLM.Providers["openai"] = config.ProviderConfig{APIKeyEnv: "OPENAI_API_KEY", BaseURL: srv.URL}
	})
	d := newFirstRunDaemonFrom(t, cfg, Options{Headless: true})
	defer d.Close()
	disk, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	pc := disk.LLM.Providers["openai"]
	pc.APIKey = "sk-good"
	disk.LLM.Providers["openai"] = pc
	disk.LLM.Model, disk.LLM.MaxTokens = "gpt-4.1-mini", 4321
	if err := disk.Save(); err != nil {
		t.Fatal(err)
	}
	if reply, err := d.Message(context.Background(), channels.Inbound{Channel: "cli", ChatID: "t", Text: "hello", IsOwner: true}); err != nil || reply != "Hello." {
		t.Fatalf("after adding the key: %q %v", reply, err)
	}
	if got := d.agent.Config().LLM; got.Model != "gpt-4.1-mini" || got.MaxTokens != 4321 {
		t.Fatalf("the agent still thinks with %s/%d", got.Model, got.MaxTokens)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(last, "4321") || !strings.Contains(last, "gpt-4.1-mini") {
		t.Fatalf("the healed model's request used the old settings: %s", last)
	}
}

func TestModelHealsWhenAKeyIsAdded(t *testing.T) {
	// The twin started without a key (the tray said "needs a key"); the
	// person then adds one with `mirrin init` or `mirrin chat`.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-good" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4.1"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello."},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	t.Setenv("OPENAI_API_KEY", "")
	cfg := newFirstRunConfig(t, func(c *config.Config) {
		c.LLM.Provider, c.LLM.Model = "openai", "gpt-4.1"
		c.LLM.Providers["openai"] = config.ProviderConfig{APIKeyEnv: "OPENAI_API_KEY", BaseURL: srv.URL}
	})
	d := newFirstRunDaemonFrom(t, cfg, Options{Headless: true})
	defer d.Close()
	say := func() (string, error) {
		return d.Message(context.Background(), channels.Inbound{Channel: "cli", ChatID: "t", Text: "hello", IsOwner: true})
	}
	setKey := func(key string) {
		disk, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		pc := disk.LLM.Providers["openai"]
		pc.APIKey = key
		disk.LLM.Providers["openai"] = pc
		if err := disk.Save(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := say(); err == nil || !strings.Contains(err.Error(), "needs an API key") {
		t.Fatalf("no key yet: %v", err)
	}
	setKey("sk-typo")
	if _, err := say(); err == nil || !strings.Contains(err.Error(), "didn't accept the API key") {
		t.Fatalf("a wrong key: %v", err)
	}
	setKey("sk-good")
	if reply, err := say(); err != nil || reply != "Hello." {
		t.Fatalf("after fixing the key, without a restart: %q %v", reply, err)
	}
	if r := firstRunResult(t, d.RunHealth(context.Background()), "model"); r.State != health.OK {
		t.Fatalf("model check: %+v", r)
	}
}

func TestUpdateConfigRefusesAKeylessProviderBeforeSaving(t *testing.T) {
	cases := []struct {
		from, to string
		start    func(*config.Config)
	}{
		{"anthropic", "openai", nil},
		{"anthropic", "gemini", nil},
		// New builds an Anthropic client without a key, so it needs its own check.
		{"openai", "anthropic", func(c *config.Config) {
			c.LLM.APIKey = ""
			c.LLM.Provider, c.LLM.Model = "openai", "gpt-4.1"
			c.LLM.Providers["openai"] = config.ProviderConfig{APIKey: "sk-test"}
		}},
	}
	for _, c := range cases {
		t.Run(c.from+" to "+c.to, func(t *testing.T) {
			for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"} {
				t.Setenv(k, "")
			}
			d, _ := newFirstRunDaemon(t, c.start)
			defer d.Close()
			before, _ := os.ReadFile(config.Path())
			err := d.SetProvider(c.to)
			var km *llm.KeyMissingError
			if !errors.As(err, &km) {
				t.Fatalf("switching to %s without a key: %v", c.to, err)
			}
			if after, _ := os.ReadFile(config.Path()); string(after) != string(before) {
				t.Fatal("the keyless provider was saved, so the next start would fail")
			}
			if d.Config().LLM.Provider != c.from {
				t.Fatal("live provider changed")
			}
		})
	}

	// A rejected change must not leak into the live config through shared maps.
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	err := d.UpdateConfig(func(c *config.Config) {
		c.LLM.Providers["openai"] = config.ProviderConfig{Model: "leaked"}
		c.Autonomy.Read = "sometimes"
	})
	if err == nil || d.Config().LLM.Providers["openai"].Model == "leaked" {
		t.Fatalf("rejected change leaked (err %v)", err)
	}
}

func TestUpdateConfigKeepsEditsMadeWhileRunning(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	// `mirrin voice setup` (or a hand edit) changes the file behind the running twin.
	disk, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "ggml-small.en.bin")
	disk.Channels.Voice.WhisperModel = model
	if err := disk.Save(); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateConfig(func(c *config.Config) { c.Autonomy.Write = "auto" }); err != nil {
		t.Fatal(err)
	}
	after, _ := config.Load()
	if after.Channels.Voice.WhisperModel != model || d.Config().Channels.Voice.WhisperModel != model {
		t.Fatalf("a settings change undid the edit: file %q, live %q", after.Channels.Voice.WhisperModel, d.Config().Channels.Voice.WhisperModel)
	}
	if after.Autonomy.Write != "auto" {
		t.Fatal("the change itself wasn't saved")
	}

	// A file that no longer parses is left alone.
	if err := os.WriteFile(config.Path(), []byte("autonomy: [oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateConfig(func(c *config.Config) { c.LLM.Effort = "low" }); err == nil {
		t.Fatal("expected an error while the file is broken")
	}
	if b, _ := os.ReadFile(config.Path()); string(b) != "autonomy: [oops" {
		t.Fatal("a broken config file was overwritten")
	}
}

func TestNewStartsWithoutAModelAndSaysWhy(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Skills.Browser.Enabled = false
	cfg.DataDir = filepath.Join(home, "data")
	cfg.API.Listen = ""
	cfg.LLM.Provider, cfg.LLM.Model = "openai", "gpt-4.1"
	d, err := New(cfg, Options{Headless: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("a missing key stopped the daemon (the service would crash-loop): %v", err)
	}
	defer d.Close()
	if r := firstRunResult(t, d.RunHealth(context.Background()), "model"); r.State != health.Fail || !strings.Contains(r.Detail+r.Fix, "platform.openai.com") {
		t.Fatalf("model: %+v", r)
	}
}

func TestFirstWeekTourStartsWithoutInit(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	ctx := context.Background()
	if !d.NeedsFirstLook(ctx) {
		t.Fatal("a brand new twin should introduce itself")
	}
	d.scheduleJobs() // what the first `mirrin chat` or `mirrin run` does
	if v, _ := d.store.Get(ctx, "installed_at"); v == "" {
		t.Fatal("installed_at not stamped, so the first-week tour never starts")
	}
	if _, err := d.FirstLook(ctx); err != nil {
		t.Fatal(err)
	}
	if d.NeedsFirstLook(ctx) {
		t.Fatal("the first look should only happen once")
	}
}

func TestModelErrorsReachThePersonInPlainWords(t *testing.T) {
	d, fake := newFirstRunDaemon(t, nil)
	defer d.Close()
	fake.err = errors.New(`anthropic 401: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	_, err := d.Message(context.Background(), channels.Inbound{Channel: "cli", ChatID: "t", Text: "hello", IsOwner: true})
	if err == nil || !strings.HasPrefix(err.Error(), "Anthropic didn't accept the API key") || strings.Contains(err.Error(), "{") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpgradedTwinGetsNoTourOrIntroduction(t *testing.T) {
	// Months of conversations from before installs were stamped.
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	ctx := context.Background()
	if err := d.store.AppendMessage(ctx, "whatsapp:61400000000", llm.Text(llm.RoleUser, "remind me to call mum")); err != nil {
		t.Fatal(err)
	}
	since := time.Now().AddDate(0, -3, 0).UTC()
	db, err := sql.Open("sqlite", filepath.Join(d.Config().DataDir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE messages SET created_at=?`, since.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	if d.NeedsFirstLook(ctx) {
		t.Fatal("a twin that has been talking for months introduced itself as new")
	}
	d.scheduleJobs()
	v, _ := d.store.Get(ctx, "installed_at")
	if at, err := time.Parse(time.RFC3339, v); err != nil || at.After(since.Add(time.Minute)) {
		t.Fatalf("installed_at = %q, want the first conversation (%s), so no first-week tour", v, since)
	}
}

func TestVoiceSessionThatCantStartSaysWhy(t *testing.T) {
	cfg := newFirstRunConfig(t, func(c *config.Config) {
		c.LLM.APIKey = "test-key"
		c.Channels.Voice.WhisperBin = "no-such-whisper-cli"
		c.Channels.Voice.TTSCommand = "true"
	})
	d := newFirstRunDaemonFrom(t, cfg, Options{Voice: true})
	_, done := runFirstRun(t, d)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no-such-whisper-cli not found") {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("`mirrin voice` sat there silently when its microphone pipeline couldn't start")
	}
}
