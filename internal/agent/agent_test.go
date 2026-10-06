package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// fakeProvider replays scripted responses and records requests.
type fakeProvider struct {
	script []llm.Response
	reqs   []llm.Request
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.reqs = append(f.reqs, req)
	if len(f.script) == 0 {
		return &llm.Response{Message: llm.Text(llm.RoleAssistant, "(end)"), StopReason: llm.StopEndTurn}, nil
	}
	r := f.script[0]
	f.script = f.script[1:]
	return &r, nil
}

func toolUse(id, name, input string) llm.Response {
	return llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: id, ToolName: name, Input: json.RawMessage(input)}}},
		StopReason: llm.StopToolUse,
	}
}

func text(s string) llm.Response {
	return llm.Response{Message: llm.Text(llm.RoleAssistant, s), StopReason: llm.StopEndTurn}
}

func setup(t *testing.T, fp *fakeProvider, autonomy config.Autonomy) (*Agent, *memory.Store, *[]string) {
	t.Helper()
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := config.Default()
	cfg.User.Name = "Tony"
	cfg.Autonomy = autonomy
	ran := &[]string{}
	reg := tools.NewRegistry()
	reg.Register(
		tools.New("echo", "echo", tools.Schema(map[string]tools.Prop{"s": {Type: "string"}}), tools.RiskRead,
			func(_ context.Context, c tools.Call) (string, error) {
				var in struct{ S string }
				_ = tools.Decode(c, &in)
				*ran = append(*ran, "echo:"+in.S)
				return "echoed " + in.S, nil
			}),
		tools.New("send", "send", tools.Schema(map[string]tools.Prop{"to": {Type: "string"}}), tools.RiskWrite,
			func(_ context.Context, c tools.Call) (string, error) {
				*ran = append(*ran, "send")
				return "sent", nil
			}),
	)
	a := New(cfg, fp, store, reg, approvals.New(cfg.Autonomy), nil)
	a.SetPersona("Mirrin", "Mirrin", "A dry, loyal butler.", "sir", nil)
	return a, store, ran
}

func TestToolLoopRunsReadToolsAutomatically(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{toolUse("t1", "echo", `{"s":"hi"}`), text("It said hi.")}}
	a, store, ran := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	out, err := a.Handle(context.Background(), "test:1", "say hi")
	if err != nil {
		t.Fatal(err)
	}
	if out != "It said hi." {
		t.Fatalf("got %q", out)
	}
	if len(*ran) != 1 || (*ran)[0] != "echo:hi" {
		t.Fatalf("tool not run: %v", *ran)
	}
	// Second request must carry the tool result back to the model.
	last := fp.reqs[1].Messages[len(fp.reqs[1].Messages)-1]
	if last.Role != llm.RoleUser || last.Blocks[0].Type != llm.BlockToolResult || last.Blocks[0].Text != "echoed hi" {
		t.Fatalf("tool result not fed back: %+v", last)
	}
	if !strings.Contains(fp.reqs[0].System, "Tony") {
		t.Fatal("persona should mention the user")
	}
	h, _ := store.History(context.Background(), "test:1", 50)
	if len(h) != 4 {
		t.Fatalf("expected 4 stored messages, got %d", len(h))
	}
}

func TestWriteToolsWaitForApproval(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{
		toolUse("t1", "send", `{"to":"priya"}`),
		text("I'd like to send that. Reply yes 1."),
		text("Sent, sir."),
	}}
	a, store, ran := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	ctx := context.Background()
	if _, err := a.Handle(ctx, "test:1", "send it"); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 0 {
		t.Fatalf("write tool ran without approval: %v", *ran)
	}
	pending, _ := store.PendingApprovals(ctx, "test:1")
	if len(pending) != 1 || pending[0].Tool != "send" {
		t.Fatalf("expected one pending approval, got %+v", pending)
	}
	res := fp.reqs[1].Messages[len(fp.reqs[1].Messages)-1].Blocks[0]
	if !strings.Contains(res.Text, "PENDING_APPROVAL id=1") {
		t.Fatalf("model not told about pending approval: %q", res.Text)
	}
	out, err := a.ResolveApproval(ctx, "test:1", pending[0].ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if out != "Sent, sir." || len(*ran) != 1 || (*ran)[0] != "send" {
		t.Fatalf("approval did not execute tool: out=%q ran=%v", out, *ran)
	}
	if _, err := a.ResolveApproval(ctx, "test:1", pending[0].ID, true); err == nil {
		t.Fatal("re-approving should fail")
	}
}

func TestPolicyNeverBlocksTool(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{toolUse("t1", "send", `{}`), text("Not permitted.")}}
	a, _, ran := setup(t, fp, config.Autonomy{Read: "auto", Write: "never", Dangerous: "never"})
	if _, err := a.Handle(context.Background(), "test:1", "send"); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 0 {
		t.Fatal("tool ran despite never policy")
	}
	res := fp.reqs[1].Messages[len(fp.reqs[1].Messages)-1].Blocks[0]
	if !res.IsError {
		t.Fatal("expected error result")
	}
}

func TestRepairFixesTruncatedHistory(t *testing.T) {
	h := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "old", Text: "x"}}},
		llm.Text(llm.RoleAssistant, "orphan assistant"),
		llm.Text(llm.RoleUser, "hello"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "t9", ToolName: "echo", Input: json.RawMessage(`{}`)}}},
		llm.Text(llm.RoleUser, "are you there?"),
	}
	out := repair(h)
	if out[0].Role != llm.RoleUser || out[0].PlainText() != "hello" {
		t.Fatalf("leading orphans not dropped: %+v", out[0])
	}
	// tool_use must be followed by a tool_result, merged with the next user turn.
	if out[2].Role != llm.RoleUser || out[2].Blocks[0].Type != llm.BlockToolResult || out[2].Blocks[0].ToolUseID != "t9" {
		t.Fatalf("missing tool result not filled: %+v", out[2])
	}
	if len(out) != 3 || out[2].Blocks[1].Text != "are you there?" {
		t.Fatalf("same-role messages not merged: %+v", out)
	}
}

func TestRepairDropsOrphanToolResults(t *testing.T) {
	h := []llm.Message{
		llm.Text(llm.RoleUser, "run it"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "outer", ToolName: "run_protocol", Input: json.RawMessage(`{}`)}}},
		llm.Text(llm.RoleUser, "[nested task]"),
		llm.Text(llm.RoleAssistant, "nested answer"),
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "outer", Text: "done"}}},
		llm.Text(llm.RoleAssistant, "final"),
	}
	out := repair(h)
	for i := 1; i < len(out); i++ {
		if out[i].Role == out[i-1].Role {
			t.Fatalf("consecutive same-role messages at %d: %+v", i, out)
		}
		for _, b := range out[i].Blocks {
			if b.Type == llm.BlockToolResult {
				prev := out[i-1]
				ok := false
				for _, pb := range prev.Blocks {
					if pb.Type == llm.BlockToolUse && pb.ToolUseID == b.ToolUseID {
						ok = true
					}
				}
				if !ok {
					t.Fatalf("orphan tool_result %s survived at %d: %+v", b.ToolUseID, i, out)
				}
			}
		}
	}
}

func TestClassifyAndBudget(t *testing.T) {
	cases := map[string]string{
		"what's happening?": "checkin", "How's it going": "checkin", "status": "checkin", "all good?": "checkin",
		"What is the top story on the BBC?": "question", "can you email Priya the deck?": "question",
		"Set a reminder for 3pm to call the accountant": "request", "[Scheduled task] do the briefing": "protocol",
	}
	for in, want := range cases {
		if got := classify(in); got != want {
			t.Errorf("classify(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestToolBudgetRefusesExtraCalls(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{
		toolUse("t1", "echo", `{"s":"a"}`),
		toolUse("t2", "echo", `{"s":"b"}`),
		text("Here's what I have."),
	}}
	a, _, ran := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	out, err := a.Handle(context.Background(), "test:1", "what's happening?")
	if err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 1 {
		t.Fatalf("check-in should allow one tool call, ran %v", *ran)
	}
	res := fp.reqs[2].Messages[len(fp.reqs[2].Messages)-1].Blocks[0]
	if !res.IsError || !strings.Contains(res.Text, "budget") {
		t.Fatalf("second call should be refused with a budget message: %+v", res)
	}
	if out != "Here's what I have." {
		t.Fatalf("got %q", out)
	}
}

func TestCaptions(t *testing.T) {
	if c := caption("fetch_url", json.RawMessage(`{"url":"https://www.bbc.com/news"}`)); c != "Looking at bbc.com." {
		t.Fatalf("got %q", c)
	}
	if c := caption("files__read_file", nil); c != "Using files." {
		t.Fatalf("got %q", c)
	}
	if c := caption("set_reminder", nil); c != "" {
		t.Fatalf("instant tools should not narrate: %q", c)
	}
}
