package agent

import (
	"context"
	"strings"
	"testing"
)

// With a page open, a short ask ("make the booking") gets room to step
// through the site; without one, the butler's usual discretion holds.
func TestBrowsingGetsRoomToFinish(t *testing.T) {
	a, _, _ := trustSetup(t, &fakeProvider{})
	ctx := context.Background()
	if got := a.toolBudget(ctx, "voice:local", "make the booking."); got != DefaultBudgets.Request {
		t.Fatalf("no browser: %d", got)
	}
	page := "open at https://booking.jetstar.com/select (\"Flight Select\")."
	a.Browser = func(context.Context) string { return page }
	if got := a.toolBudget(ctx, "voice:local", "make the booking."); got != browsingBudget {
		t.Fatalf("request while browsing: %d", got)
	}
	if got := a.toolBudget(ctx, "voice:local", "which flights are available?"); got != browsingBudget {
		t.Fatalf("question while browsing: %d", got)
	}
	if got := a.toolBudget(ctx, "voice:local", "how's it going"); got != DefaultBudgets.CheckIn {
		t.Fatalf("check-in while browsing: %d", got)
	}
	// Every chat hears what the browser has open, and that it can look.
	if v := a.volatile(ctx); !strings.Contains(v, page) || !strings.Contains(v, "browse_page and no url") {
		t.Fatalf("prompt:\n%s", v)
	}
	// Closed: the turn hears so, and doesn't carry on from an old page.
	page = ""
	if v := a.volatile(ctx); !strings.Contains(v, "closed, with no page open") {
		t.Fatalf("prompt with the browser closed:\n%s", v)
	}
	page = "open at https://booking.jetstar.com/select."
	if v := a.volatile(ForStranger(ctx, "Bob")); strings.Contains(v, "jetstar") {
		t.Fatalf("a stranger's turn heard about the owner's browser:\n%s", v)
	}
}

// Small talk and the owner's own questions aren't a task's answer.
func TestNotAnAnswer(t *testing.T) {
	for _, s := range []string{"how you doing?", "Are you online?", "what's up?", "hey", "thanks", "Hey, so what's happening?", "which flights are there?"} {
		if !NotAnAnswer(s) {
			t.Errorf("%q taken as an answer", s)
		}
	}
	for _, s := range []string{"Yes", "go with the Jetstar one", "2 October", "the 7am", "Raise the limit."} {
		if NotAnAnswer(s) {
			t.Errorf("%q not taken as an answer", s)
		}
	}
}
