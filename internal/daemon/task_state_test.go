package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

// A task the owner dropped is said to be dropped, so a chat whose history
// says the booking is under way doesn't carry on as if it were.
func TestTaskStateSaysWhatWasDropped(t *testing.T) {
	now := time.Now()
	got := taskState([]tasks.Task{
		{Title: "Bali flights early October", Status: tasks.Cancelled, Updated: now.Add(-time.Hour)},
		{Title: "Book Bali flights", Status: tasks.Done, Result: "Stopped before payment; nothing booked.", Updated: now.Add(-2 * time.Hour)},
		{Title: "Chase the refund", Status: tasks.WaitingUser, Question: "Which order number?", Updated: now},
		{Title: "Last month's errand", Status: tasks.Done, Updated: now.Add(-30 * 24 * time.Hour)},
	}, now)
	for _, want := range []string{`"Bali flights early October" was dropped by the owner`, "not happening", `"Book Bali flights" finished: Stopped before payment`, `"Chase the refund" (started`, `is waiting user, asking`, `: Which order number?`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "Last month") {
		t.Errorf("an old task is still mentioned: %q", got)
	}
	if taskState(nil, now) != "" {
		t.Error("no tasks should say nothing")
	}
}

// A task set aside is still the owner's: "how's Bali going?" gets a
// straight answer, and only one paused for the budget carries on by itself.
func TestTaskStateSaysWhatIsSetAside(t *testing.T) {
	now := time.Now()
	got := taskState([]tasks.Task{
		{Title: "Plan the Bali trip", Status: tasks.Paused, PausedBy: tasks.PausedBudget, PausedFor: "the monthly model budget was used up; it carries on when the budget allows", Updated: now.Add(-5 * 24 * time.Hour)},
		{Title: "Find a plumber", Status: tasks.Paused, PausedBy: tasks.PausedSteps, Updated: now},
	}, now)
	for _, want := range []string{
		`"Plan the Bali trip" is set aside: the monthly model budget was used up; it carries on when the budget allows;`,
		`"Find a plumber" is set aside: it ran out of steps before finishing; it carries on only when the owner says to try it again`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "none open") || strings.Count(got, "try it again") != 1 {
		t.Errorf("%q", got)
	}
}

// A booking task started on Thursday "for tomorrow" reads, on Saturday, as
// started on Thursday: the prompt can tell its flight has gone.
func TestTaskStateSaysWhenATaskStarted(t *testing.T) {
	loc := time.FixedZone("AEST", 10*3600)
	now := time.Date(2026, 10, 3, 21, 0, 0, 0, loc)
	started := time.Date(2026, 10, 1, 21, 40, 0, 0, loc)
	got := taskState([]tasks.Task{{Title: "Book Jetstar MEL-DPS tomorrow", Status: tasks.WaitingUser, Question: "Your date of birth?", Created: started, AskedAt: started.Add(30 * time.Minute), Updated: now}}, now)
	for _, want := range []string{`"Book Jetstar MEL-DPS tomorrow" (started Thu 1 Oct 21:40)`, "asking since Thu 1 Oct 22:10"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if w := when(now.Add(-2*time.Hour), now); w != "today 19:00" {
		t.Errorf("today: %q", w)
	}
	if w := when(now.Add(-24*time.Hour), now); w != "yesterday 21:00" {
		t.Errorf("yesterday: %q", w)
	}
}

// A reminder that already went off is said to have, with when.
func TestFiredRemindersAreSaidToHaveGoneOff(t *testing.T) {
	loc := time.FixedZone("AEST", 10*3600)
	now := time.Date(2026, 10, 3, 21, 3, 0, 0, loc)
	got := firedState([]memory.Reminder{{Text: "Stand up and stretch.", FiredAt: time.Date(2026, 10, 3, 21, 1, 20, 0, loc)}}, now)
	if !strings.Contains(got, `"Stand up and stretch." went off today 21:01`) || !strings.Contains(got, "don't describe them as still to come") {
		t.Fatalf("%q", got)
	}
	if firedState(nil, now) != "" {
		t.Fatal("nothing fired should say nothing")
	}
}
