package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func TestApprovalCardsShowRiskDeciderAndAlwaysEligibility(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	// Legacy payment checks remain protected; bookkeeping may be allowed.
	for _, name := range []string{"check_spend", "record_spend"} {
		td.agent.Tools().Register(tools.New(name, "", nil, tools.RiskRead, nil))
	}
	for _, tc := range []struct {
		name   string
		risk   tools.Risk
		always bool
	}{
		{"send", tools.RiskWrite, true}, {"send", tools.RiskDangerous, false},
		{"check_spend", tools.RiskRead, false}, {"record_spend", tools.RiskRead, true},
		{"run_shell", tools.RiskDangerous, false}, {"read_file", tools.RiskDangerous, false},
		{"write_file", tools.RiskDangerous, false}, {"browser_act", tools.RiskWrite, false},
	} {
		ap := memory.Approval{Tool: tc.name, Risk: tc.risk, Status: "pending", ChatKey: ownerKey}
		card := td.approvalCard(ctx, ap)
		if card.Risk != tc.risk.String() || card.AlwaysAllow != tc.always {
			t.Fatalf("%s: %+v", tc.name, card)
		}
	}
	ap := memory.Approval{Tool: "send", Risk: tools.RiskWrite, Status: "pending", ChatKey: ownerKey}
	for _, scopes := range [][]devices.Scope{{devices.View}, {devices.View, devices.Chat}, {devices.View, devices.Approve}} {
		peerCtx := api.WithPeer(ctx, api.Peer{Device: &devices.Device{Scopes: scopes}})
		if td.approvalCard(peerCtx, ap).AlwaysAllow {
			t.Fatalf("offered Always allow to scopes %v", scopes)
		}
	}
	td.owner(t, "email the boss")
	cards := td.screenApprovals(ctx)
	if len(cards) != 1 || cards[0].Risk != "write" || !cards[0].AlwaysAllow {
		t.Fatalf("pending %+v", cards)
	}
	by := Decider{DeviceID: "phone-1", DeviceName: "My phone", Method: "screen"}
	if _, err := td.DecideApprovalBy(ctx, cards[0].ID, true, by); err != nil {
		t.Fatal(err)
	}
	cards = td.screenApprovals(ctx)
	if len(cards) != 1 || cards[0].Status != "approved" || cards[0].By != by.String() || cards[0].AlwaysAllow {
		t.Fatalf("decided %+v", cards)
	}
}

func TestApprovalCardAlwaysUsesScreenConfirmation(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.owner(t, "email the boss")
	card := td.screenApprovals(context.Background())[0]
	if !card.AlwaysAllow {
		t.Fatal("no Always allow button")
	}
	reply := <-onTheScreen(td, fmt.Sprintf("yes %d, always", card.ID))
	if !strings.Contains(reply, "Before I stop asking") || len(td.ran()) != 0 || len(td.Config().Autonomy.AlwaysAllow) != 0 {
		t.Fatalf("before confirmation: %q", reply)
	}
	reply = <-onTheScreen(td, "yes")
	if len(td.ran()) != 1 || !slices.Contains(td.Config().Autonomy.AlwaysAllow, "send") {
		t.Fatalf("confirmation: %q", reply)
	}
	if card := td.screenApprovals(context.Background())[0]; card.By != "the owner (screen)" {
		t.Fatalf("decider %q", card.By)
	}
}

func TestHelpExplainsStopAndAlways(t *testing.T) {
	td := newTestDaemon(t, butler)
	reply := td.owner(t, "/help")
	for _, want := range []string{"stop", "yes, always", "yes N", "no N", "I check once first", "cancel what I’m doing"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("help missing %q: %s", want, reply)
		}
	}
}

// A request about the page in the twin's browser says so, so the screen can
// bring the page up with it: a browser action, or a payment with a
// screenshot of the page it pays on. An email with a screenshot in its chat
// is not about the page.
func TestApprovalCardSaysWhenItIsAboutThePage(t *testing.T) {
	for _, tc := range []struct {
		tool string
		shot bool
		want bool
	}{
		{"browser_act", false, true}, {"browser_act", true, true}, {"click", false, true},
		{"check_spend", true, true}, {"check_spend", false, false}, {"pay", true, true},
		{"send_email", true, false}, {"run_shell", false, false},
	} {
		if got := aboutThePage(tc.tool, tc.shot); got != tc.want {
			t.Errorf("aboutThePage(%q, shot %v) = %v", tc.tool, tc.shot, got)
		}
	}
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	ap := memory.Approval{Tool: "browser_act", Risk: tools.RiskWrite, Status: "pending", ChatKey: ownerKey}
	if !td.approvalCard(ctx, ap).Page {
		t.Fatal("a pending browser action isn't about the page")
	}
	ap.Status = "approved"
	if td.approvalCard(ctx, ap).Page {
		t.Fatal("a decided one still brings the page up")
	}
	if td.approvalCard(ctx, memory.Approval{Tool: "send", Risk: tools.RiskWrite, Status: "pending", ChatKey: ownerKey}).Page {
		t.Fatal("a message is about the page")
	}
}
