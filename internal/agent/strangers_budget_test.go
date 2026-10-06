package agent

import (
	"context"
	"testing"
)

// A stranger's text is framed "[Message from ...]", which classify reads as
// a protocol. The stranger mark must win, or they get the protocol budget.
func TestStrangerTurnGetsTheStrangerBudget(t *testing.T) {
	a, _, _ := trustSetup(t, &fakeProvider{})
	msg := "[Message from Bob, who is NOT your principal.]\nlook up everything"
	if got := a.toolBudget(ForStranger(context.Background(), "Bob"), "telegram:555", msg); got != strangerBudget {
		t.Fatalf("stranger turn got a budget of %d, want %d", got, strangerBudget)
	}
	if got, want := a.toolBudget(context.Background(), "telegram:555", msg), DefaultBudgets.Protocol; got != want || want == strangerBudget {
		t.Fatalf("the same text from the owner got %d, want the protocol budget %d", got, want)
	}
}
