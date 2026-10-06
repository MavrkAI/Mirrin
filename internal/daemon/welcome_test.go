package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/skills/calendar"
)

type welcomeTransport func(*http.Request) (*http.Response, error)

func (f welcomeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func welcomeHTTP(t *testing.T, fn func(*http.Request) (int, string, error)) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = welcomeTransport(func(r *http.Request) (*http.Response, error) {
		code, body, err := fn(r)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
}
func TestWelcomeOllamaDetection(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	welcomeHTTP(t, func(r *http.Request) (int, string, error) {
		switch r.URL.Path {
		case "/api/tags":
			return 200, `{"models":[{"name":"llama3.1:latest"}]}`, nil
		case "/api/show":
			return 200, `{"capabilities":["completion","tools"]}`, nil
		}
		t.Fatalf("unexpected request %s", r.URL)
		return 0, "", nil
	})
	for _, b := range d.DetectBrains(context.Background()) {
		if b.Provider == "ollama" {
			if !b.Ready || b.Model != "llama3.1" {
				t.Fatal(b)
			}
			return
		}
	}
	t.Fatal("Ollama wasn't detected")
}
func TestWelcomeOllamaHTTPServer(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			io.WriteString(w, `{"models":[{"name":"llama3.1"}]}`)
		} else {
			io.WriteString(w, `{"capabilities":["completion"]}`)
		}
	}))
	defer server.Close()
	d.cfg.LLM.Providers["ollama"] = config.ProviderConfig{BaseURL: server.URL + "/v1"}
	for _, b := range d.DetectBrains(context.Background()) {
		if b.Provider == "ollama" && b.Ready && b.Model == "llama3.1" {
			return
		}
	}
	t.Fatal("Ollama wasn't detected")
}
func TestWelcomeUseBrainErrors(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		body     string
		err      error
		sentence string
	}{
		{"unauthorized", 401, `{"error":{"message":"bad key"}}`, nil, "That key wasn't accepted."},
		{"forbidden", 403, `{"error":{"message":"denied"}}`, nil, "That key doesn't have access to this model."},
		{"billing", 429, `{"error":{"message":"insufficient_quota"}}`, nil, "Your model account is out of credit."},
		{"rate", 429, `{"error":{"message":"rate limit"}}`, nil, "Your model is busy right now."},
		{"dns", 0, "", &net.DNSError{Err: "no such host", Name: "model.invalid"}, "I couldn't find that model service."},
		{"timeout", 0, "", context.DeadlineExceeded, "That connection took too long."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := newFirstRunDaemon(t, nil)
			defer d.Close()
			welcomeHTTP(t, func(*http.Request) (int, string, error) { return tt.status, tt.body, tt.err })
			err := d.UseBrain(context.Background(), "openai", "test-key", "gpt-4.1")
			var human *api.HumanError
			if !errors.As(err, &human) || human.Sentence != tt.sentence || human.Fix == "" {
				t.Fatalf("got %v want %q", err, tt.sentence)
			}
			if config.Secret("OPENAI_API_KEY") == "test-key" {
				t.Fatal("rejected key saved")
			}
		})
	}
}
func TestWelcomeStoresKeyAndStreamsNamedHello(t *testing.T) {
	for _, provider := range []string{"openai", "openai-compatible"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", "")
			d, _ := newFirstRunDaemon(t, func(c *config.Config) {
				off := false
				c.UI.Weather = &off // only the model answers here
				if provider == "openai-compatible" {
					c.LLM.Providers[provider] = config.ProviderConfig{BaseURL: "https://model.invalid/v1"}
				}
			})
			defer d.Close()
			welcomeHTTP(t, func(r *http.Request) (int, string, error) {
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), `"stream":true`) {
					return 200, `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`, nil
				}
				if !strings.Contains(string(body), "Mina") {
					t.Fatal("name missing from prompt")
				}
				return 200, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello, Mina.\"}}]}\n\ndata: [DONE]\n\n", nil
			})
			if err := d.UseBrain(context.Background(), provider, "welcome-test-key", "gpt-4.1"); err != nil {
				t.Fatal(err)
			}
			if err := d.SetNames(context.Background(), "Mina", "Ember", "pickoo", ""); err != nil {
				t.Fatal(err)
			}
			var delta string
			reply, err := d.Hello(context.Background(), func(s string) { delta += s }, nil)
			if err != nil || delta != "Hello, Mina." || reply.Text != delta {
				t.Fatalf("%q %q %v", reply.Text, delta, err)
			}
			srv := api.New("127.0.0.1:9999", "welcome-test-token", d).WithWelcome(d)
			req := httptest.NewRequest("POST", "http://127.0.0.1:9999/welcome/hello", nil)
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Authorization", "Bearer welcome-test-token")
			serverConn, clientConn := net.Pipe()
			listener := &welcomePipeListener{conn: serverConn, closed: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer clientConn.Close()
			go srv.Serve(ctx, listener, api.LoopbackOnly, "test")
			_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			start := time.Now()
			if err := req.Write(clientConn); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(clientConn), req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || time.Since(start) > 5*time.Second || response.StatusCode != 200 || !strings.Contains(string(body), "event: status") || !strings.Contains(string(body), "event: delta") || !strings.Contains(string(body), "event: done") || !strings.Contains(string(body), "Mina") {
				t.Fatalf("stream: %d %s %v", response.StatusCode, body, err)
			}

			if d.NeedsFirstLook(context.Background()) {
				t.Fatal("introduction not recorded")
			}
			env := llm.DefaultKeyEnv(provider)
			if env == "" {
				env = "MIRRIN_CUSTOM_API_KEY"
			}
			if config.Secret(env) != "welcome-test-key" {
				t.Fatal("key not saved")
			}
			data, err := os.ReadFile(config.Path())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "welcome-test-key") {
				t.Fatal("key leaked into YAML")
			}

		})
	}
}

func TestWelcomeReplacingSameProviderKeyRebuildsClient(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	d, _ := newFirstRunDaemon(t, func(c *config.Config) {
		c.LLM.Provider = "openai"
		c.LLM.Model = "gpt-4.1"
		c.LLM.APIKey = ""
		c.LLM.APIKeyEnv = "OPENAI_API_KEY"
	})
	defer d.Close()
	welcomeHTTP(t, func(r *http.Request) (int, string, error) {
		if r.Header.Get("Authorization") != "Bearer replacement-key" {
			t.Fatal("client still has the old key")
		}
		return 200, `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`, nil
	})
	if err := d.UseBrain(context.Background(), "openai", "replacement-key", "gpt-4.1"); err != nil {
		t.Fatal(err)
	}
	if d.agent.Provider().Name() == "fake/model" {
		t.Fatal("same provider didn't rebuild")
	}
	if _, err := d.agent.Provider().Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatal(err)
	}
}

func TestWelcomeRejectsKeyShadowedByEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "exported-key")
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	welcomeHTTP(t, func(*http.Request) (int, string, error) {
		t.Fatal("conflicting key must not be sent")
		return 0, "", nil
	})
	err := d.UseBrain(context.Background(), "openai", "replacement-key", "gpt-4.1")
	var human *api.HumanError
	if !errors.As(err, &human) || human.Sentence != "A different key is set in your environment." {
		t.Fatalf("shadowed key: %v", err)
	}
	secrets, err := config.ReadSecrets()
	if err != nil || secrets["OPENAI_API_KEY"] != "" {
		t.Fatalf("rejected key saved: %v", err)
	}
}

func TestWelcomeRestoreReportIsOfferedUntilDismissed(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	ctx := context.Background()
	if err := d.store.Set(ctx, "first_look_at", "done"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.Home(), "welcome-restore.txt")
	if err := os.WriteFile(path, []byte("Review your devices."), 0o600); err != nil {
		t.Fatal(err)
	}
	if !d.NeedsWelcome(ctx) || d.WelcomeRestoreReport(ctx).Detail != "Review your devices." {
		t.Fatal("restore review wasn't offered")
	}
	if err := d.DismissWelcomeRestore(ctx); err != nil {
		t.Fatal(err)
	}
	if d.NeedsWelcome(ctx) {
		t.Fatal("dismissed restore review still offered")
	}
}

// restoredTwin is a twin a restore just put here: its state says when, and
// what stayed on the old Mac, and the restore's report is written.
func restoredTwin(t *testing.T, change func(*config.Config), from *backup.MovedFrom) (*Daemon, string) {
	t.Helper()
	d, _ := newFirstRunDaemon(t, func(c *config.Config) {
		c.Name, c.Persona = "Mirrin", "mirrin"
		c.User.Name, c.User.Honorific = "Akshay Kumar", "Akshay"
		if change != nil {
			change(c)
		}
	})
	t.Cleanup(func() { d.Close() })
	if err := backup.SaveState(d.Config().DataDir, backup.State{RestoredAt: time.Now().Add(-time.Minute), From: from}); err != nil {
		t.Fatal(err)
	}
	const report = "Your twin is home. Review the devices below and remove any you don't recognise from Devices.\nAkshay's iPhone (0123abcd)\nThis snapshot is 3 days old."
	if err := os.WriteFile(filepath.Join(config.Home(), "welcome-restore.txt"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	return d, report
}

// Moving in: the twin welcomes the owner back in character with what came
// along, read from the restored twin, then says what is still to do here,
// the device review first.
func TestWelcomeBackSaysWhatCameAlong(t *testing.T) {
	d, report := restoredTwin(t, func(c *config.Config) {
		c.Channels.WhatsApp.Enabled = true // on, but no link: a snapshot never takes it
		c.Skills.Browser.Enabled = true
	}, &backup.MovedFrom{Host: "Akshay's MacBook Pro", StandsBy: true, SignIns: true})
	ctx := context.Background()
	for i := 1; i <= 214; i++ {
		if _, err := d.store.Remember(ctx, "general", fmt.Sprintf("fact number %d", i), "test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []string{"call the bank", "water the plants", "renew the passport"} {
		if _, err := d.store.AddReminder(ctx, screenChat, time.Now().Add(time.Hour), r); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(d.cfg.ProtocolsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"morning-briefing.yaml": "name: morning briefing\nschedule: \"0 7 * * *\"\nprompt: Brief me.\n",
		"reply-like-me.yaml":    "name: reply like me\nprompt: Draft it.\n",
		"weekly-review.yaml":    "name: weekly review\nschedule: \"0 17 * * 5\"\nprompt: Review the week.\n",
		"parked.yaml":           "name: parked routine\nschedule: \"0 9 * * *\"\nenabled: false\nprompt: Not now.\n",
	} {
		if err := os.WriteFile(filepath.Join(d.cfg.ProtocolsDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.ReloadProtocols(); err != nil {
		t.Fatal(err)
	}

	got := d.WelcomeRestoreReport(ctx)
	if want := "Welcome back, Akshay. Same Mirrin, new Mac: 214 things I remember, your morning briefing and 2 more routines, and 3 reminders came along."; got.Line != want {
		t.Fatalf("the welcome:\n got %q\nwant %q", got.Line, want)
	}
	if got.Persona != "mirrin" || got.Facts != 214 || got.Reminders != 3 || strings.Join(got.Routines, ", ") != "morning briefing, weekly review, reply like me" {
		t.Fatalf("what came along: %+v", got)
	}
	want := []api.RestoreTodo{{Text: "Review your devices.", URL: "/restore/review"}}
	if whatsapp.Built {
		want = append(want, api.RestoreTodo{Text: "WhatsApp needs one scan.", URL: "/channels"})
	}
	want = append(want, api.RestoreTodo{Text: "Sign in to sites again in the browser; sign-ins stay on the old Mac."})
	if !slices.Equal(got.Todo, want) {
		t.Fatalf("still to do:\n got %+v\nwant %+v", got.Todo, want)
	}
	if got.StandBy != "Akshay's MacBook Pro stands by once it sees the move, and stays paused until you run `mirrin backup resume` there." {
		t.Fatalf("stand-by note: %q", got.StandBy)
	}
	if got.Detail != report {
		t.Fatalf("the restore's own report: %q", got.Detail)
	}

	// The browser's sign-ins already here (a twin set aside kept them): none
	// to redo.
	if err := os.MkdirAll(filepath.Join(d.cfg.DataDir, "chrome-profile"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, todo := range d.WelcomeRestoreReport(ctx).Todo {
		if strings.Contains(todo.Text, "Sign in to sites") {
			t.Fatal("asked to sign in again with the sign-ins already here")
		}
	}
}

// A part that is zero is left out, and nothing is said that isn't so: no
// old Mac standing by, no sign-ins it kept. A Mac without the model key is
// asked for one.
func TestWelcomeBackLeavesOutWhatIsntThere(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	d, _ := restoredTwin(t, func(c *config.Config) {
		c.Skills.Browser.Enabled = true
		c.LLM.Provider, c.LLM.APIKey, c.LLM.APIKeyEnv = "openai", "", ""
	}, nil)
	got := d.WelcomeRestoreReport(context.Background())
	if got.Line != "Welcome back, Akshay. Same Mirrin, new Mac." {
		t.Fatalf("the welcome: %q", got.Line)
	}
	want := []api.RestoreTodo{{Text: "Review your devices.", URL: "/restore/review"}, {Text: "Add this Mac's model key.", URL: "/accounts#model"}}
	if !slices.Equal(got.Todo, want) || got.StandBy != "" {
		t.Fatalf("still to do: %+v, stand-by %q", got.Todo, got.StandBy)
	}

	// A chat app that stopped for good needs signing in; one still
	// connecting may be fine in a moment.
	d.cmu.Lock()
	d.cfg.Channels.Telegram.Enabled, d.cfg.Channels.Discord.Enabled = true, true
	d.cmu.Unlock()
	d.chmu.Lock()
	d.chanErr = map[string]string{"telegram": "the bot token was rejected", "discord": "no connection"}
	d.chanCancel = map[string]context.CancelFunc{"discord": func() {}} // the supervisor tries again
	d.chmu.Unlock()
	got = d.WelcomeRestoreReport(context.Background())
	if len(got.Todo) != 3 || got.Todo[1] != (api.RestoreTodo{Text: "Telegram needs signing in again.", URL: "/channels"}) {
		t.Fatalf("a stopped chat app: %+v", got.Todo)
	}

	for _, c := range []struct {
		facts, reminders int
		routines         []string
		want             string
	}{
		{1, 0, []string{"morning briefing"}, "Welcome back, sir. Same Mirrin, new Mac: 1 thing I remember and your morning briefing came along."},
		{0, 1, []string{"morning briefing", "evening wrap"}, "Welcome back, sir. Same Mirrin, new Mac: your morning briefing and 1 more routine, and 1 reminder came along."},
		{12, 2, nil, "Welcome back, sir. Same Mirrin, new Mac: 12 things I remember and 2 reminders came along."},
		{0, 0, []string{"inbox triage"}, "Welcome back, sir. Same Mirrin, new Mac: your inbox triage came along."},
	} {
		if got := welcomeBackLine("sir", "Mirrin", c.facts, c.routines, c.reminders); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
	if got := welcomeBackLine("", "Ember", 3, nil, 0); got != "Welcome back. Same Ember, new Mac: 3 things I remember came along." {
		t.Errorf("no way to address the owner: %q", got)
	}
}

// When the restored twin can't be read, or the report is from a restore
// that didn't finish (written long after the last one), the page shows the
// restore's own report alone, as before.
func TestWelcomeBackFallsBackToTheReport(t *testing.T) {
	d, report := restoredTwin(t, nil, nil)
	ctx := context.Background()
	if got := d.WelcomeRestoreReport(ctx); got.Line == "" {
		t.Fatal("a finished restore wasn't welcomed")
	}
	if err := backup.SaveState(d.Config().DataDir, backup.State{RestoredAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := d.WelcomeRestoreReport(ctx); got.Line != "" || got.Todo != nil || got.Detail != report {
		t.Fatalf("a restore that didn't finish was welcomed: %+v", got)
	}
	if err := backup.SaveState(d.Config().DataDir, backup.State{RestoredAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	d.store.Close() // the memory can't be read
	if got := d.WelcomeRestoreReport(ctx); got.Line != "" || got.Detail != report {
		t.Fatalf("welcomed without the memory: %+v", got)
	}
}

// A real HTTP connection without a listening socket, for sandbox runs.
type welcomePipeListener struct {
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *welcomePipeListener) Accept() (net.Conn, error) {
	if l.conn != nil {
		c := l.conn
		l.conn = nil
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *welcomePipeListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (l *welcomePipeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9999}
}

func TestWelcomeURLWaitsForServerToken(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	d.cfg.API.Listen = "127.0.0.1:9999"
	if u := d.WelcomeURL(); u != "" {
		t.Fatal("welcome minted a token before the server", u)
	}
	if _, err := os.Stat(api.TokenPath(d.cfg.DataDir)); !os.IsNotExist(err) {
		t.Fatal("welcome created a token", err)
	}
	token, err := api.LoadOrCreateToken(d.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if u := d.WelcomeURL(); !strings.Contains(u, "/welcome?token="+token) {
		t.Fatal(u)
	}
}

func TestWelcomeSwitchKeepsPreviousProvider(t *testing.T) {
	d, _ := newFirstRunDaemon(t, func(c *config.Config) { c.LLM.BaseURL = "https://previous.invalid"; c.LLM.Model = "previous-model" })
	defer d.Close()
	welcomeHTTP(t, func(r *http.Request) (int, string, error) {
		if r.URL.Host == "previous.invalid" {
			t.Fatal("used previous provider's endpoint")
		}
		return 200, `{"choices":[{"message":{"role":"assistant","content":"Hello"}}]}`, nil
	})
	if err := d.UseBrain(context.Background(), "openai", "new-key", "gpt-4.1"); err != nil {
		t.Fatal(err)
	}
	c := d.Config()
	if c.ProviderKey("anthropic") != "test-key" || c.ProviderModel("anthropic") != "previous-model" || c.ProviderBaseURL("anthropic") != "https://previous.invalid" {
		t.Fatal("lost previous provider settings")
	}
	if c.ProviderBaseURL("openai") != "" {
		t.Fatal("inherited previous endpoint")
	}
}

func TestWelcomeUsesExistingInlineKey(t *testing.T) {
	d, _ := newFirstRunDaemon(t, func(c *config.Config) {
		c.LLM.Provider = "openai"
		c.LLM.Model = "gpt-4.1"
		c.LLM.APIKey = "existing-inline-key"
		c.LLM.APIKeyEnv = "OPENAI_API_KEY"
	})
	defer d.Close()
	welcomeHTTP(t, func(r *http.Request) (int, string, error) {
		if r.Header.Get("Authorization") != "Bearer existing-inline-key" {
			t.Fatal("didn't use existing key")
		}
		return 200, `{"choices":[{"message":{"role":"assistant","content":"Hello"}}]}`, nil
	})
	if err := d.UseBrain(context.Background(), "openai", "", "gpt-4.1"); err != nil {
		t.Fatal(err)
	}
	c := d.Config()
	if c.ProviderKey("openai") != "existing-inline-key" {
		t.Fatal("lost existing key")
	}
}

func TestWelcomeEmptyReplyKeepsSetupOpen(t *testing.T) {
	d, _ := newFirstRunDaemon(t, func(c *config.Config) { off := false; c.UI.Weather = &off })
	defer d.Close()
	welcomeHTTP(t, func(*http.Request) (int, string, error) {
		return 200, "data: [DONE]\n\n", nil
	})
	p, err := llm.New(llm.ProviderSettings{Provider: "openai-compatible", Model: "fake", BaseURL: "https://model.invalid/v1"})
	if err != nil {
		t.Fatal(err)
	}
	d.agent.SetProvider(p)
	_, err = d.Hello(context.Background(), func(string) {}, nil)
	var human *api.HumanError
	if !errors.As(err, &human) || human.Sentence != "I didn't get a reply from that model." {
		t.Fatalf("empty reply: %v", err)
	}
	if !d.NeedsFirstLook(context.Background()) {
		t.Fatal("empty reply completed setup")
	}
}

func TestWelcomeExplainedProviderError(t *testing.T) {
	err := llm.Explain(errors.New("openai 401: bad key"))
	var human *api.HumanError
	if !errors.As(brainError(err), &human) || human.Sentence != "That key wasn't accepted." {
		t.Fatalf("wrapped error: %v", brainError(err))
	}
}

// Step 2 of the welcome saves how the twin addresses the owner: "name" is
// the first word of their name, "sir" and "ma'am" are themselves, even
// where the persona's own way differs, and "" leaves it be.
func TestWelcomeSetsHowToAddress(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	ctx := context.Background()
	for _, c := range []struct{ persona, address, want string }{
		{"nyra", "name", "Akshaya"},
		{"mirrin", "ma'am", "ma'am"},
		{"mirrin", "sir", "sir"},
		{"nyra", "sir", "sir"}, // Mirrin's "sir", chosen again for Nyra, stays
		{"pickoo", "", "sir"},
		{"pickoo", "name", "Akshaya"},
	} {
		if err := td.SetNames(ctx, "Akshaya Kumar", "Twin", c.persona, c.address); err != nil {
			t.Fatal(err)
		}
		if h := td.Config().User.Honorific; h != c.want || savedHonorific(t) != c.want || td.address() != c.want {
			t.Fatalf("%s with %q: live %q, saved %q, address %q; want %q", c.persona, c.address, h, savedHonorific(t), td.address(), c.want)
		}
		if id := td.Persona().ID; id != c.persona {
			t.Fatalf("persona %q, want %q", id, c.persona)
		}
	}
}

// Hear Nyra says her greeting in her own voice, pitch and pace, whatever
// the twin speaks as now, and only once voice is set up.
func TestPreviewPersonaSpeaksInItsOwnVoice(t *testing.T) {
	type said struct {
		voice, name, line string
		pitch             int
		speed             float64
	}
	var got []said
	old := sayPreview
	sayPreview = func(_ context.Context, v config.Voice, name, _, line string) {
		got = append(got, said{v.Voice, name, line, v.Pitch, v.Speed})
	}
	t.Cleanup(func() { sayPreview = old })
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	ctx := context.Background()
	if td.PreviewPersona(ctx, "nyra") || len(got) != 0 {
		t.Fatalf("spoke before voice was set up: %v", got)
	}
	model := filepath.Join(t.TempDir(), "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	td.cmu.Lock()
	td.cfg.Channels.Voice.WhisperModel = model // set up, not listening
	td.cmu.Unlock()
	for _, id := range []string{"nyra", "pickoo", "mirrin", "starter/nobody"} {
		if spoke := td.PreviewPersona(ctx, id); spoke != (id != "starter/nobody") {
			t.Fatalf("%s: spoke %v", id, spoke)
		}
	}
	stock := config.Default().Channels.Voice.Speed
	want := []said{
		{"af_nova", "Nyra", "Hello. Nyra here.", 0, 1.04},
		{"am_puck", "Pickoo", "Hiya! Pickoo here.", 520, 1.06},
		{"bm_george", "Mirrin", "Good to see you, sir. What can I take off your plate?", 0, stock},
	}
	if len(got) != len(want) {
		t.Fatalf("said %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("preview %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

// helloWeather answers Open-Meteo for the first hello: 14.2°C and clear.
func helloWeather(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"current":{"temperature_2m":14.2,"weather_code":0}}`)
	}))
	old := weatherURL
	weatherURL = srv.URL
	fresh := func() {
		weatherCache.Lock()
		weatherCache.w = nil
		weatherCache.Unlock()
	}
	fresh()
	t.Cleanup(func() { srv.Close(); weatherURL = old; fresh() })
}

// The first hello is in character, with true things Go gathered for the
// model to word: the part of the day, the weather where the owner's time
// zone is, and when the routines run. It offers no tools, says what it is
// looking at before any words, starts the first week, and opens the
// screen's conversation.
func TestHelloInCharacterWithOneTrueThing(t *testing.T) {
	helloWeather(t)
	var asked []llm.Request
	td := newTestDaemon(t, func(_ string, req llm.Request) llm.Response {
		asked = append(asked, req)
		return say("Evening, Akshay. Fourteen degrees and clear in Melbourne. Type to me whenever you like.")
	})
	ctx := context.Background()
	if err := td.SetNames(ctx, "Akshay Kumar", "Nyra", "nyra", "name"); err != nil {
		t.Fatal(err)
	}
	td.cmu.Lock()
	td.cfg.User.Timezone = "Australia/Melbourne"
	td.cmu.Unlock()
	for name, body := range map[string]string{
		"morning-briefing.yaml": "name: morning briefing\nschedule: \"0 7 * * *\"\nprompt: Brief me.\n",
		"parked.yaml":           "name: parked routine\nschedule: \"0 9 * * *\"\nenabled: false\nprompt: Not now.\n",
	} {
		if err := os.MkdirAll(td.cfg.ProtocolsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(td.cfg.ProtocolsDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := td.ReloadProtocols(); err != nil {
		t.Fatal(err)
	}
	seen := listen(t, td.bus)

	var order []string
	reply, err := td.Hello(ctx, func(s string) { order = append(order, "delta: "+s) }, func(s string) { order = append(order, "status: "+s) })
	if err != nil {
		t.Fatal(err)
	}
	part := dayPart(time.Now().In(td.location()))
	if len(order) < 2 || order[0] != "status: "+lookingAt(part) || !strings.HasPrefix(order[1], "delta: ") {
		t.Fatalf("the status should come before any words: %q", order)
	}
	if len(asked) != 1 {
		t.Fatalf("%d model calls", len(asked))
	}
	req := asked[0]
	facts := lastUserText(req)
	for what, ok := range map[string]bool{
		"the persona's name":         strings.Contains(req.System, "You are Nyra"),
		"her character":              strings.Contains(req.System, strings.Fields(td.Persona().Character)[0]),
		"how she addresses him":      strings.Contains(req.System, `as "Akshay"`),
		"the ask":                    strings.Contains(req.System, fmt.Sprintf(helloAsk, "Akshay Kumar", helloEndTyping)),
		"not listening, no name":     !strings.Contains(req.System, "say your name"),
		"the part of the day":        strings.Contains(facts, "Time: "+part+", "),
		"the weather":                strings.Contains(facts, "Weather: 14°C and clear in Melbourne."),
		"the routine's time":         strings.Contains(facts, "Routines: morning briefing every day at 7:00."),
		"no parked routine":          !strings.Contains(facts, "parked"),
		"no calendar, none is there": !strings.Contains(strings.ToLower(facts), "calendar"),
		"no tools":                   len(req.Tools) == 0,
		"a short reply":              req.MaxTokens == 200,
	} {
		if !ok {
			t.Errorf("hello prompt: %s\nsystem: %s\nfacts: %s", what, req.System, facts)
		}
	}
	if reply.Text == "" || reply.Note != weatherNoteZone {
		t.Fatalf("reply %+v", reply)
	}
	for _, key := range []string{"installed_at", "first_look_at"} {
		if v, _ := td.store.Get(ctx, key); v == "" {
			t.Errorf("%s not set", key)
		}
	}
	h, err := td.store.History(ctx, screenChat, 10)
	if err != nil || len(h) == 0 || h[len(h)-1].Role != llm.RoleAssistant || h[len(h)-1].PlainText() != reply.Text {
		t.Fatalf("the screen's conversation doesn't start with the hello: %v %v", h, err)
	}
	said := false
	for _, ev := range seen() {
		if ev.Kind == "said" && ev.Text == reply.Text {
			from, _ := ev.Data.(map[string]string)
			said = from["channel"] == "screen"
		}
	}
	if !said {
		t.Fatal("the screens weren't told the hello")
	}

	// Weather turned off: none asked for, and no small print.
	off := false
	td.cmu.Lock()
	td.cfg.UI.Weather = &off
	td.cmu.Unlock()
	reply, err = td.Hello(ctx, func(string) {}, nil)
	if err != nil || reply.Note != "" || strings.Contains(lastUserText(asked[1]), "Weather") {
		t.Fatalf("weather off: %+v %v\n%s", reply, err, lastUserText(asked[1]))
	}

	// Listening for its name on this Mac, it says they can say its name.
	td.cmu.Lock()
	td.cfg.Channels.Voice.Enabled, td.cfg.Channels.Voice.Mode = true, "wake"
	td.cmu.Unlock()
	if _, err := td.Hello(ctx, func(string) {}, nil); err != nil {
		t.Fatal(err)
	}
	if sys := asked[2].System; !strings.Contains(sys, "say your name") {
		t.Fatalf("listening, but no name to say:\n%s", sys)
	}
}

// The day's next event is worded from the calendar, or that nothing more
// is on it; tomorrow's isn't today's.
func TestHelloNextOnTheCalendarToday(t *testing.T) {
	loc := time.FixedZone("AEST", 10*3600)
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, loc)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 3, h, m, 0, 0, loc) }
	for _, c := range []struct {
		evs  []calendar.Event
		want string
	}{
		{nil, "Calendar: nothing more today."},
		{[]calendar.Event{{Title: "Tomorrow's train", Start: at(8, 0).AddDate(0, 0, 1)}}, "Calendar: nothing more today."},
		{[]calendar.Event{{Title: "Under way", Start: at(17, 30), End: at(18, 30)}, {Title: `Dinner "with" Priya`, Start: at(19, 30)}}, "Next on the calendar today: “Dinner with Priya” at 7:30 pm."},
		{[]calendar.Event{{Title: "Mum's birthday", Start: at(0, 0), AllDay: true}}, "All day today on the calendar: “Mum's birthday”."},
	} {
		if got := nextToday(c.evs, now); got != c.want {
			t.Errorf("%v: %q, want %q", c.evs, got, c.want)
		}
	}
}

// Said out loud, the first hello is in the persona's own voice, and only
// once voice is set up.
func TestSayHelloInThePersonasVoice(t *testing.T) {
	type said struct{ voice, name, line string }
	var got []said
	old := sayPreview
	sayPreview = func(_ context.Context, v config.Voice, name, _, line string) {
		got = append(got, said{v.Voice, name, line})
	}
	t.Cleanup(func() { sayPreview = old })
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	ctx := context.Background()
	if err := td.SetNames(ctx, "Akshay", "Nyra", "nyra", "name"); err != nil {
		t.Fatal(err)
	}
	if td.SayHello(ctx, "Evening, Akshay.") || len(got) != 0 {
		t.Fatalf("spoke before voice was set up: %v", got)
	}
	model := filepath.Join(t.TempDir(), "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	td.cmu.Lock()
	td.cfg.Channels.Voice.WhisperModel = model
	td.cmu.Unlock()
	if !td.SayHello(ctx, "Evening, Akshay.") || len(got) != 1 || got[0] != (said{"af_nova", "Nyra", "Evening, Akshay."}) {
		t.Fatalf("said %v", got)
	}
}
