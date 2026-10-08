package watch

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// New mail something else handles itself (a bill) doesn't also reach the
// agent, so the owner hears about it once; what it leaves still does.
func TestClaimedItemsSkipTheAgent(t *testing.T) {
	src := &inboxSource{fakeSource{name: "gmail", items: map[string]string{"1": "unread from Sarah: Lunch?"}}}
	h := newHarness(t, src)
	var seen []string
	h.w.SetClaim(func(_ context.Context, source string, added []Item) []Item {
		if source != "gmail" {
			t.Errorf("source = %q", source)
		}
		var rest []Item
		for _, a := range added {
			seen = append(seen, a.Key+" "+a.Line)
			if !strings.Contains(a.Line, "bill") {
				rest = append(rest, a)
			}
		}
		return rest
	})
	ctx := context.Background()
	h.w.Poll(ctx) // baseline: nothing is offered to the claimer
	if len(seen) != 0 {
		t.Fatalf("the baseline was claimed: %q", seen)
	}
	src.set("2", "unread from EDF: Your bill is ready")
	h.w.Poll(ctx)
	if h.taskCount() != 0 {
		t.Fatalf("a claimed item reached the agent: %v", h.tasks)
	}
	src.set("3", "unread from Landlord: About the lease")
	src.set("4", "unread from Water Co: Your bill for October")
	h.w.Poll(ctx)
	if h.taskCount() != 1 || !strings.Contains(h.tasks[0], "Landlord") || strings.Contains(h.tasks[0], "Water Co") {
		t.Fatalf("what was left didn't reach the agent alone: %v", h.tasks)
	}
	want := []string{"2 unread from EDF: Your bill is ready", "3 unread from Landlord: About the lease", "4 unread from Water Co: Your bill for October"}
	if !slices.Equal(seen, want) {
		t.Fatalf("claimer saw %q, want %q", seen, want)
	}
	h.w.Poll(ctx)
	if len(seen) != 3 || h.taskCount() != 1 {
		t.Fatalf("an item was offered twice: %q %v", seen, h.tasks)
	}
}

// Two new emails with the same line (this month's bill and last month's,
// say) each come with a key of their own, so the claimer can tell them apart.
func TestClaimedItemsKeepTheirOwnKeys(t *testing.T) {
	prev := map[string]string{"1": "unread from EDF: Your bill is ready"}
	cur := map[string]string{
		"1": "unread from EDF: Your bill is ready",
		"2": "unread from Sarah: Lunch?",
		"3": "unread from EDF: Your bill is ready",
		"4": "unread from EDF: Your bill is ready",
	}
	got := addedItems(prev, cur, Diff(prev, cur).Added)
	want := []Item{{"2", "unread from Sarah: Lunch?"}, {"3", "unread from EDF: Your bill is ready"}, {"4", "unread from EDF: Your bill is ready"}}
	if !slices.Equal(got, want) {
		t.Fatalf("items = %q, want %q", got, want)
	}
}
