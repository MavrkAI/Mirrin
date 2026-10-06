package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// scriptedModel answers from a script, one response per call.
type scriptedModel struct {
	mu     sync.Mutex
	script []llm.Response
}

func (m *scriptedModel) Name() string { return "scripted" }
func (m *scriptedModel) Complete(context.Context, llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.script) == 0 {
		return &llm.Response{Message: llm.Text(llm.RoleAssistant, "(end)"), StopReason: llm.StopEndTurn}, nil
	}
	r := m.script[0]
	m.script = m.script[1:]
	return &r, nil
}

// sentChat is a channel that records what it sends, as "chatID|text".
type sentChat struct {
	owner string
	mu    sync.Mutex
	sent  []string
}

func (c *sentChat) Name() string                                  { return "telegram" }
func (c *sentChat) Start(context.Context, channels.Handler) error { return nil }
func (c *sentChat) OwnerChatID() string                           { return c.owner }
func (c *sentChat) Send(_ context.Context, chatID, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, chatID+"|"+text)
	return nil
}

func (c *sentChat) to(chatID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.sent {
		if id, text, _ := strings.Cut(s, "|"); id == chatID {
			out = append(out, text)
		}
	}
	return out
}

// A stranger's request waits on the owner, who hears about it in their own
// chat and can answer there; the stranger then hears how it went.
func TestOwnerAnswersStrangerRequestsFromTheirOwnChat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Skills.Browser.Enabled = false
	cfg.DataDir = filepath.Join(home, "data")
	cfg.ProtocolsDir = filepath.Join(home, "protocols")
	cfg.LLM.APIKey = "test-key"
	cfg.User.Name = "Tony"
	d, err := New(cfg, Options{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.store.Close()
	peeks := 0
	d.agent.Tools().Register(tools.New("peek_calendar", "peek", nil, tools.RiskRead, func(context.Context, tools.Call) (string, error) {
		peeks++
		return "free after 3pm", nil
	}))
	d.agent.SetProvider(&scriptedModel{script: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "t1", ToolName: "peek_calendar", Input: json.RawMessage(`{"day":"friday"}`)}}}, StopReason: llm.StopToolUse},
		{Message: llm.Text(llm.RoleAssistant, "I've asked Tony."), StopReason: llm.StopEndTurn},
		{Message: llm.Text(llm.RoleAssistant, "Tony is free after 3pm on Friday."), StopReason: llm.StopEndTurn},
	}})
	chat := &sentChat{owner: "111"}
	d.channels["telegram"] = chat
	ctx := context.Background()

	if _, err := d.Message(ctx, channels.Inbound{Channel: "telegram", ChatID: "999", Sender: "Bob", Text: "is Tony free friday?"}); err != nil {
		t.Fatal(err)
	}
	if peeks != 0 {
		t.Fatal("the tool ran for a stranger without the owner's yes")
	}
	pending, _ := d.store.PendingApprovals(ctx, "telegram:999")
	if len(pending) != 1 {
		t.Fatalf("want one request waiting in Bob's chat, got %+v", pending)
	}
	id := pending[0].ID
	told := chat.to("111")
	want := fmt.Sprintf("\"Bob\" on Telegram (not you) is asking me to peek_calendar(day=friday)\nReply \"yes %d\" to let me, or \"no %d\".", id, id)
	if len(told) != 1 || told[0] != want {
		t.Fatalf("the owner should be told in their chat:\n got %q\nwant %q", told, want)
	}
	if n := d.Status(ctx).Pending; n != 1 {
		t.Fatalf("status should count the stranger's request, got %d", n)
	}

	reply, err := d.Message(ctx, channels.Inbound{Channel: "telegram", ChatID: "111", Sender: "Tony", Text: fmt.Sprintf("yes %d", id), IsOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	if peeks != 1 {
		t.Fatalf("the approved call should run once, ran %d", peeks)
	}
	if reply != "Done. I told Bob: Tony is free after 3pm on Friday." {
		t.Fatalf("owner's reply %q", reply)
	}
	if got := chat.to("999"); len(got) != 1 || got[0] != "Tony is free after 3pm on Friday." {
		t.Fatalf("Bob should hear the outcome, got %q", got)
	}
	// Deciding again says so rather than failing.
	again, _ := d.Message(ctx, channels.Inbound{Channel: "telegram", ChatID: "111", Sender: "Tony", Text: fmt.Sprintf("no %d", id), IsOwner: true})
	if !strings.Contains(again, "already approved") {
		t.Fatalf("second decision: %q", again)
	}
	// Only the owner decides: a "yes" from Bob is just a message to the twin.
	if _, err := d.Message(ctx, channels.Inbound{Channel: "telegram", ChatID: "999", Sender: "Bob", Text: fmt.Sprintf("yes %d", id+1)}); err != nil {
		t.Fatal(err)
	}
	if peeks != 1 {
		t.Fatal("a stranger's yes must decide nothing")
	}
}

func TestChannelWarningsReachTheChannelsPage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Skills.Browser.Enabled = false
	cfg.DataDir = filepath.Join(home, "data")
	cfg.ProtocolsDir = filepath.Join(home, "protocols")
	cfg.LLM.APIKey = "test-key"
	cfg.Channels.IRC.Enabled = true
	d, err := New(cfg, Options{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.store.Close()
	d.channels["irc"] = &warnChat{}
	for _, st := range d.ConnectorStates(context.Background()) {
		if st.Name == "irc" {
			if st.Notice != "set your owner" {
				t.Fatalf("notice %q", st.Notice)
			}
			return
		}
	}
	t.Fatal("no irc connector listed")
}

type warnChat struct{ sentChat }

func (*warnChat) Name() string    { return "irc" }
func (*warnChat) Warning() string { return "set your owner" }
