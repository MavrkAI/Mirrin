package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/skills/spend"
	"github.com/MavrkAI/Mirrin/internal/skills/system"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func TestSafetyFloorStoresDangerousAndNeverRunsBeforeYes(t *testing.T) {
	for _, name := range []string{"check_spend", "run_shell", "read_file", "write_file", "list_dir", "browser_act"} {
		t.Run(name, func(t *testing.T) {
			a, store, _ := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "auto", Dangerous: "auto", AlwaysAllow: []string{name}})
			for _, tool := range system.Tools(config.System{Enabled: true, AllowShell: true}) {
				a.Tools().Register(floorProbe{Tool: tool, t: t})
			}
			for _, tool := range spend.New(store, func() config.Spending { return config.Spending{} }).Tools() {
				a.Tools().Register(floorProbe{Tool: tool, t: t})
			}
			a.Tools().Register(riskyTool{tools.New("browser_act", "", nil, tools.RiskWrite, func(context.Context, tools.Call) (string, error) { t.Fatal("payment ran"); return "", nil })})
			input := json.RawMessage(`{"path":"~/.ssh/id_rsa","content":"x","command":"echo unsafe","label":"Pay now","amount":1}`)
			result, failed := a.execute(context.Background(), "test:1", llm.Block{ToolName: name, Input: input})
			if failed || !strings.Contains(result, "PENDING_APPROVAL") {
				t.Fatalf("%s, failed=%v", result, failed)
			}
			pending, err := store.PendingApprovals(context.Background(), "test:1")
			if err != nil || len(pending) != 1 || pending[0].Risk != tools.RiskDangerous {
				t.Fatalf("pending %+v: %v", pending, err)
			}
		})
	}
}

// Keep real classification but never execute a file, shell or payment call,
// even when this regression test is run against a broken policy.
type floorProbe struct {
	tools.Tool
	t *testing.T
}

func (p floorProbe) RiskFor(ctx context.Context, call tools.Call) tools.Risk {
	if cr, ok := p.Tool.(tools.CallRisker); ok {
		return cr.RiskFor(ctx, call)
	}
	return p.Tool.Risk()
}
func (p floorProbe) Run(context.Context, tools.Call) (string, error) {
	p.t.Fatal("protected tool ran before approval")
	return "", nil
}

func TestSafetyFloorShellDisabledByPolicy(t *testing.T) {
	a, store, _ := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "auto", Dangerous: "never"})
	a.Tools().Register(floorProbe{Tool: tools.New("run_shell", "", nil, tools.RiskDangerous, nil), t: t})
	for _, ctx := range []context.Context{context.Background(), ForStranger(context.Background(), "a visitor")} {
		result, failed := a.execute(ctx, "test:1", llm.Block{ToolName: "run_shell", Input: json.RawMessage(`{"command":"echo blocked"}`)})
		if !failed || !strings.Contains(result, "disabled by the user's autonomy policy") {
			t.Fatalf("result %q, failed %v", result, failed)
		}
	}
	if pending, err := store.AllPendingApprovals(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("pending %+v: %v", pending, err)
	}
}

func TestRecordSpendUpdatesLimitsWithoutAnotherApproval(t *testing.T) {
	a, store, _ := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	ledger := spend.New(store, func() config.Spending { return config.Spending{MonthlyLimit: 100, Currency: "AUD"} })
	for _, tool := range ledger.Tools() {
		a.Tools().Register(tool)
	}
	ctx := context.Background()
	result, failed := a.execute(ctx, "test:1", llm.Block{ToolName: "record_spend", Input: json.RawMessage(`{"amount":90,"merchant":"Shop","purpose":"completed purchase"}`)})
	if failed || !strings.HasPrefix(result, "recorded.") {
		t.Fatalf("result %q, failed %v", result, failed)
	}
	if ok, _, why := ledger.Check(ctx, 20); ok || !strings.Contains(why, "BLOCKED") {
		t.Fatalf("later payment was not blocked: %s", why)
	}
	if pending, err := store.AllPendingApprovals(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending %+v: %v", pending, err)
	}
}

func TestApprovalPromptDistinguishesStopFromNo(t *testing.T) {
	a, _, _ := setup(t, &fakeProvider{}, config.Autonomy{})
	prompt := a.systemFor(context.Background())
	for _, want := range []string{"yes N or no N", `"stop" halts whatever is running`, "unless policy forbids them outright"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("approval guidance missing %q", want)
		}
	}
}
