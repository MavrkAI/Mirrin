package agent

import (
	"context"
	"testing"
)

// A web chore that stops at the button (the screen's "Try this" chip, or
// the owner's own words) gets a task's room, not a question's three calls
// or a request's six; a stranger's words never do.
func TestAWebChoreGetsRoomToFinish(t *testing.T) {
	a, _, _ := trustSetup(t, &fakeProvider{})
	ctx := context.Background()
	for _, ask := range []string{
		"Fill in the practice pizza order form at https://httpbin.org/forms/post for a medium margherita, and stop before submitting it.",
		"Find the cheapest train to Brighton on Friday and stop before booking",
		"Can you fill in the contact form on their site and stop before sending?",
		"Get the basket ready and stop at the final button.",
	} {
		if got := classify(ask); got != "chore" {
			t.Errorf("classify(%q) = %s, want chore", ask, got)
		}
		if got := a.toolBudget(ctx, "screen:local", ask); got != DefaultBudgets.Protocol {
			t.Errorf("budget for %q = %d, want %d", ask, got, DefaultBudgets.Protocol)
		}
		if got := a.toolBudget(ForStranger(ctx, "Bob"), "telegram:555", ask); got != strangerBudget {
			t.Errorf("a stranger's %q got %d", ask, got)
		}
	}
	for _, ask := range []string{
		"Book a table for two on Friday",
		"Don't stop before you've sent it",
		"What can you do for me?",
		"Stop telling me these",
	} {
		if got := classify(ask); got == "chore" {
			t.Errorf("classify(%q) = chore", ask)
		}
	}
}
