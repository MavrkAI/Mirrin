package agent

import (
	"context"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// A run reading a bill that just arrived may read mail and set reminders,
// and nothing else: a crafted email can't make it browse, follow its link,
// pay or send, even for a tool the owner always allows.
func TestBillsRunOnlyReadsMailAndSetsReminders(t *testing.T) {
	noop := func(context.Context, tools.Call) (string, error) { return "", nil }
	extra := []tools.Tool{
		tools.New("read_email", "read", nil, tools.RiskRead, noop),
		tools.New("set_reminder", "remind", nil, tools.RiskRead, noop),
		tools.New("fetch_url", "fetch", nil, tools.RiskRead, noop),
		tools.New("send_email", "send", nil, tools.RiskWrite, noop),
		tools.New("browser_act", "click", nil, tools.RiskWrite, noop),
	}
	a, _, _ := trustSetup(t, &fakeProvider{}, extra...)
	auto := a.cfg.Autonomy
	auto.AlwaysAllow = []string{"send_email", "fetch_url"}
	a.SetPolicy(approvals.New(auto))
	ctx := context.Background()
	key := "telegram:1" + BillsRunMarker + "20261007-090000-1"
	want := map[string]approvals.Decision{
		"read_email": approvals.Allow, "set_reminder": approvals.Allow,
		"fetch_url": approvals.Deny, "send_email": approvals.Deny, "browser_act": approvals.Deny, "read_notes": approvals.Deny,
	}
	for name, d := range want {
		tool, _ := a.tools.Get(name)
		if got := a.decide(ctx, key, tool, tool.Risk()); got != d {
			t.Errorf("%s in a bills run: got %v, want %v", name, got, d)
		}
		// Anywhere else the owner's policy is untouched.
		if got := a.decide(ctx, "telegram:1", tool, tool.Risk()); got == approvals.Deny {
			t.Errorf("%s denied outside a bills run", name)
		}
	}
}
