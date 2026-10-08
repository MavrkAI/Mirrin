package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// fakeLLM answers each request with brain(last user text, request). It is
// safe for concurrent conversations.
type fakeLLM struct {
	mu    sync.Mutex
	brain func(last string, req llm.Request) llm.Response
	seen  []string
}

func (f *fakeLLM) Name() string { return "fake" }
func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	last := lastUserText(req)
	f.mu.Lock()
	f.seen = append(f.seen, last)
	brain := f.brain
	f.mu.Unlock()
	r := brain(last, req)
	return &r, nil
}

func (f *fakeLLM) heard() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// lastUserText is the text of the request's last message: typed text or tool results.
func lastUserText(req llm.Request) string {
	if len(req.Messages) == 0 {
		return ""
	}
	var parts []string
	for _, b := range req.Messages[len(req.Messages)-1].Blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func say(s string) llm.Response {
	return llm.Response{Message: llm.Text(llm.RoleAssistant, s), StopReason: llm.StopEndTurn}
}

func call(id, tool, input string) llm.Response {
	return llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: id, ToolName: tool, Input: json.RawMessage(input)}}},
		StopReason: llm.StopToolUse,
	}
}

// fakeChannel is a messaging channel that records what it is asked to send.
type fakeChannel struct {
	name, owner string
	mu          sync.Mutex
	sent        []string
	out         chan string
}

func (f *fakeChannel) Name() string                                        { return f.name }
func (f *fakeChannel) Start(ctx context.Context, _ channels.Handler) error { <-ctx.Done(); return nil }
func (f *fakeChannel) OwnerChatID() string                                 { return f.owner }
func (f *fakeChannel) Send(_ context.Context, chatID, text string) error {
	f.mu.Lock()
	f.sent = append(f.sent, chatID+": "+text)
	f.mu.Unlock()
	select {
	case f.out <- text:
	default:
	}
	return nil
}

func (f *fakeChannel) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// next waits for the channel's next outgoing message.
func (f *fakeChannel) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-f.out:
		return s
	case <-time.After(10 * time.Second):
		t.Fatalf("nothing was sent; so far: %q", f.messages())
		return ""
	}
}

// testDaemon is a headless daemon with a scripted model, a Telegram-like
// owner channel and a "send" tool that needs approval.
type testDaemon struct {
	*Daemon
	llm  *fakeLLM
	ch   *fakeChannel
	mu   sync.Mutex
	sent []string // what the send tool actually sent
	// modelCalls counts requests that reached the stand-in model server: a
	// model rebuilt from the config (healModel) goes there, never to a real
	// provider.
	modelCalls *atomic.Int64
}

// standInModel is a local server in place of the provider's API, refusing
// every request the way a bad key is refused. Tests never reach the real
// one, even when a failed model is rebuilt from the config.
func standInModel(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, calls
}

const ownerKey = "telegram:owner"

func newTestDaemon(t *testing.T, brain func(last string, req llm.Request) llm.Response) *testDaemon {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Skills.Browser.Enabled = false
	cfg.DataDir = filepath.Join(home, "data")
	cfg.ProtocolsDir = filepath.Join(home, "protocols")
	cfg.LLM.APIKey = "test-key"
	modelURL, modelCalls := standInModel(t)
	if os.Getenv("ANTHROPIC_BASE_URL") == "" { // a test with a server of its own keeps it
		cfg.LLM.BaseURL = modelURL
	}
	cfg.Autonomy = config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"}
	// Nothing is held for quiet hours (held.go) unless a test asks for them:
	// a run at night sees what a run by day does.
	cfg.User.QuietHours = "off"
	d, err := New(cfg, Options{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.store.Close() })
	// The first hello on a new channel has tests of its own (firstchannel_test.go).
	_ = d.store.Set(context.Background(), firstHelloKey, "already")
	// So does the first look at the inbox (inbox_first_test.go).
	_ = d.store.Set(context.Background(), inboxFirstKey, "already")
	run, stop := context.WithCancel(context.Background()) // as Run sets it
	d.runCtx = run
	t.Cleanup(stop)
	td := &testDaemon{Daemon: d, modelCalls: modelCalls, llm: &fakeLLM{brain: brain}, ch: &fakeChannel{name: "telegram", owner: "owner", out: make(chan string, 64)}}
	d.agent.SetProvider(td.llm)
	d.channels["telegram"] = td.ch
	d.agent.Tools().Register(tools.New("send", "send a message", tools.Schema(map[string]tools.Prop{"to": {Type: "string"}}), tools.RiskWrite,
		func(_ context.Context, c tools.Call) (string, error) {
			var in struct{ To string }
			_ = tools.Decode(c, &in)
			td.mu.Lock()
			td.sent = append(td.sent, in.To)
			td.mu.Unlock()
			return "sent to " + in.To, nil
		}))
	return td
}

// ran lists who the send tool actually sent to.
func (td *testDaemon) ran() []string {
	td.mu.Lock()
	defer td.mu.Unlock()
	return append([]string(nil), td.sent...)
}

// owner sends a message as the owner and waits for the reply.
func (td *testDaemon) owner(t *testing.T, text string) string {
	t.Helper()
	reply, err := td.message(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: text, IsOwner: true}, agent.Events{})
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	return reply
}

// listen collects the bus's events until the test ends.
func listen(t *testing.T, bus *events.Bus) func() []events.Event {
	ch, stop := bus.Subscribe()
	var mu sync.Mutex
	var got []events.Event
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case ev := <-ch:
				mu.Lock()
				got = append(got, ev)
				mu.Unlock()
			case <-quit:
				return
			}
		}
	}()
	t.Cleanup(func() { stop(); close(quit) })
	return func() []events.Event {
		time.Sleep(50 * time.Millisecond) // let the publisher's sends land
		mu.Lock()
		defer mu.Unlock()
		return append([]events.Event(nil), got...)
	}
}

// eventually polls cond for up to ten seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
