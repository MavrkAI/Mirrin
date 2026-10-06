package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// A tool's approval text may use what its check found (browser_act names
// the element it fingerprinted), so the check runs first.
func TestApprovalSummaryIsWrittenAfterTheCheck(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{toolUse("t1", "act", `{}`), text("asked")}}
	a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	seen := "nothing"
	a.tools.Register(tools.WithSummaryAndCheck(
		tools.New("act", "act", tools.Schema(map[string]tools.Prop{}), tools.RiskWrite,
			func(context.Context, tools.Call) (string, error) { return "done", nil }),
		func(tools.Call) string { return "Click " + seen },
		func(context.Context, tools.Call) error { seen = `"Pay now $49" on shop.example`; return nil }))
	if _, err := a.Handle(context.Background(), "telegram:1", "pay it"); err != nil {
		t.Fatal(err)
	}
	pending, _ := store.PendingApprovals(context.Background(), "telegram:1")
	if len(pending) != 1 || !strings.Contains(pending[0].Summary, `Click "Pay now $49" on shop.example`) {
		t.Fatalf("approval summary written before the check: %+v", pending)
	}
}
