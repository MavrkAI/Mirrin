package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

func TestFactsRemindersApprovals(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.Remember(ctx, "Family", "Priya is the user's partner.", "test")
	if err != nil {
		t.Fatal(err)
	}
	facts, _ := s.Recall(ctx, "priya", 10)
	if len(facts) != 1 || facts[0].ID != id || facts[0].Subject != "family" {
		t.Fatalf("recall failed: %+v", facts)
	}
	if err := s.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	if facts, _ = s.Recall(ctx, "priya", 10); len(facts) != 0 {
		t.Fatal("forget failed")
	}

	if _, err := s.AddReminder(ctx, "c", time.Now().Add(-time.Second), "past"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddReminder(ctx, "c", time.Now().Add(time.Hour), "future"); err != nil {
		t.Fatal(err)
	}
	due, _ := s.DueReminders(ctx, time.Now())
	if len(due) != 1 || due[0].Text != "past" {
		t.Fatalf("due reminders wrong: %+v", due)
	}
	_ = s.MarkFired(ctx, due[0].ID)
	if due, _ = s.DueReminders(ctx, time.Now()); len(due) != 0 {
		t.Fatal("fired reminder still due")
	}

	aid, _ := s.CreateApproval(ctx, "c", "send", nil, "send()")
	if err := s.ResolveApproval(ctx, aid, "approved"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveApproval(ctx, aid, "denied"); err == nil {
		t.Fatal("double resolve should fail")
	}

	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleUser, "one"))
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleAssistant, "two"))
	_ = s.AppendMessage(ctx, "c", llm.Text(llm.RoleUser, "three"))
	h, _ := s.History(ctx, "c", 2)
	if len(h) != 2 || h[0].PlainText() != "two" || h[1].PlainText() != "three" {
		t.Fatalf("history order wrong: %+v", h)
	}
}

// DeletePortrait takes the portrait, the earlier one and a proposed new one,
// and leaves the rest of the state alone.
func TestDeletePortraitRemovesEveryCopy(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetPortrait(ctx, "You are quietly looking for a new role."); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"portrait.prev", "portrait.new", "owner.chat"} {
		if err := s.Set(ctx, k, "kept for "+k); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeletePortrait(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"portrait", "portrait.prev", "portrait.new"} {
		if v, err := s.Get(ctx, k); err != nil || v != "" {
			t.Errorf("%s is still %q (%v)", k, v, err)
		}
	}
	if p, err := s.GetPortrait(ctx); err != nil || p.Text != "" {
		t.Fatalf("portrait %+v (%v)", p, err)
	}
	if v, _ := s.Get(ctx, "owner.chat"); v != "kept for owner.chat" {
		t.Fatalf("another key went too: %q", v)
	}
}

// What hears that a fact was kept gets it as stored, once it is in memory.
func TestOnRememberedHearsTheFactAsKept(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got []Fact
	s.OnRemembered(func(f Fact) {
		if _, err := s.FactByID(ctx, f.ID); err != nil {
			t.Errorf("heard of fact #%d before it was kept: %v", f.ID, err)
		}
		got = append(got, f)
	})
	id, err := s.Remember(ctx, " Preferences ", "  Akshay doesn't eat meat. ", "screen:local")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Subject != "preferences" || got[0].Content != "Akshay doesn't eat meat." || got[0].Source != "screen:local" || time.Since(got[0].CreatedAt) > time.Minute {
		t.Fatalf("heard %+v", got)
	}
	if _, err := s.FactByID(ctx, id+1); !errors.Is(err, ErrNoFact) {
		t.Fatalf("a fact that isn't there: %v", err)
	}
}
