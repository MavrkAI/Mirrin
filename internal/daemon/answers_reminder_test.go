package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/channels"
)

// remindNow sets a reminder for key that is already due and sends it, as
// the heartbeat does.
func remindNow(t *testing.T, td *testDaemon, key, text string) int64 {
	t.Helper()
	id, err := td.store.AddReminder(context.Background(), key, time.Now().Add(-time.Minute), text)
	if err != nil {
		t.Fatal(err)
	}
	refire(t, td, id, key, text)
	return id
}

// refire sends reminder id again, once it falls due after being put back.
func refire(t *testing.T, td *testDaemon, id int64, key, text string) {
	t.Helper()
	ctx := context.Background()
	if err := td.Notify(ctx, key, "Reminder: "+text); err != nil {
		t.Fatal(err)
	}
	if err := td.store.MarkFired(ctx, id); err != nil {
		t.Fatal(err)
	}
}

// settled reports whether no reminder sent for key is waiting on a word:
// it was ticked off or put back.
func settled(td *testDaemon, key string) bool {
	_, ok := td.store.LastFired(context.Background(), key, time.Hour)
	return !ok
}

// "Done" to a reminder just sent ticks it off, without the model, while
// it is under half an hour old; after that it is conversation.
func TestDoneTicksOffAReminderJustSent(t *testing.T) {
	ctx := context.Background()
	for _, words := range []string{"done", "Done!", "did it", "done it", "All done.", "sorted", "ticked"} {
		t.Run(words, func(t *testing.T) {
			td := newTestDaemon(t, butler)
			remindNow(t, td, ownerKey, "call the plumber")
			withClock(t, 29*time.Minute)
			if got := td.owner(t, words); got != "Ticked." {
				t.Fatalf("reply %q", got)
			}
			if !settled(td, ownerKey) || slices.Contains(td.llm.heard(), words) {
				t.Fatalf("not ticked off by the daemon itself (model heard %q)", td.llm.heard())
			}
			// The conversation keeps it, and a second "done" is conversation.
			h, _ := td.store.History(ctx, ownerKey, 2)
			if len(h) != 2 || h[0].PlainText() != words || h[1].PlainText() != "Ticked." {
				t.Fatalf("history %+v", h)
			}
			if got := td.owner(t, "done"); got != "Heard: done" {
				t.Fatalf("second done: %q", got)
			}
		})
	}
	t.Run("after half an hour", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		remindNow(t, td, ownerKey, "call the plumber")
		withClock(t, 31*time.Minute)
		if got := td.owner(t, "done"); got != "Heard: done" {
			t.Fatalf("reply %q", got)
		}
		if settled(td, ownerKey) {
			t.Fatal("a done 31 minutes on ticked the reminder off")
		}
	})
	t.Run("more than a word", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		remindNow(t, td, ownerKey, "call the plumber")
		if got := td.owner(t, "done, he's coming at four"); got != "Heard: done, he's coming at four" || settled(td, ownerKey) {
			t.Fatalf("reply %q", got)
		}
	})
	t.Run("once the twin has said something since", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		remindNow(t, td, ownerKey, "call the plumber")
		_ = td.Notify(ctx, ownerKey, "Your 3pm moved to 4pm.")
		if got := td.owner(t, "done"); got != "Heard: done" || settled(td, ownerKey) {
			t.Fatalf("reply %q", got)
		}
	})
}

// A reminder set by voice reaches the owner on their phone, and is
// answered there.
func TestDoneTicksOffAReminderSentElsewhere(t *testing.T) {
	td := newTestDaemon(t, butler)
	id, _ := td.store.AddReminder(context.Background(), "voice:local", time.Now().Add(-time.Minute), "call the plumber")
	refire(t, td, id, ownerKey, "call the plumber") // delivered to the owner's phone
	if got := td.owner(t, "done"); got != "Ticked." || !settled(td, "voice:local") {
		t.Fatalf("reply %q", got)
	}
}

// A bare yes is about the approval waiting, never the reminder: approvals
// come first, and while the twin's question is open a "done" is
// conversation too.
func TestApprovalsComeBeforeAReminder(t *testing.T) {
	t.Run("yes decides the approval", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		td.owner(t, "email the boss")
		remindNow(t, td, ownerKey, "call the plumber")
		if got := td.owner(t, "yes"); !strings.HasPrefix(got, "Just to be sure: yes to #1") {
			t.Fatalf("reply %q", got)
		}
		if got := td.owner(t, "yes"); got != "Sent." || !slices.Equal(td.ran(), []string{"boss"}) {
			t.Fatalf("reply %q, sent %v", got, td.ran())
		}
		if settled(td, ownerKey) {
			t.Fatal("a yes settled the reminder")
		}
	})
	t.Run("done while a question is open", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		td.owner(t, "email the boss")
		remindNow(t, td, ownerKey, "call the plumber")
		td.owner(t, "yes") // "Just to be sure: …"
		if got := td.owner(t, "done"); got != "Heard: done" || settled(td, ownerKey) {
			t.Fatalf("reply %q", got)
		}
		if ap, _ := td.store.GetApproval(context.Background(), 1); ap.Status != "pending" {
			t.Fatalf("#1 is %s", ap.Status)
		}
	})
}

// "Later" puts a reminder back an hour; "tomorrow 9" and the like put it
// back till then, read as the reminders skill reads a time.
func TestLaterPutsAReminderBack(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		words string
		due   func(now time.Time, loc *time.Location) time.Time
		reply string
	}{
		{"later", func(now time.Time, _ *time.Location) time.Time { return now.Add(time.Hour) }, ""},
		{"Later.", func(now time.Time, _ *time.Location) time.Time { return now.Add(time.Hour) }, ""},
		{"in 20 minutes", func(now time.Time, _ *time.Location) time.Time { return now.Add(20 * time.Minute) }, ""},
		{"tomorrow 9", func(now time.Time, loc *time.Location) time.Time {
			d := now.In(loc).AddDate(0, 0, 1)
			return time.Date(d.Year(), d.Month(), d.Day(), 9, 0, 0, 0, loc)
		}, "Back at 9:00 am tomorrow."},
	}
	for _, c := range cases {
		t.Run(c.words, func(t *testing.T) {
			td := newTestDaemon(t, butler)
			remindNow(t, td, ownerKey, "call the plumber")
			now, loc := time.Now(), td.location()
			got := td.owner(t, c.words)
			ps, _ := td.store.PendingReminders(ctx, ownerKey)
			if len(ps) != 1 || ps[0].Snoozes != 1 {
				t.Fatalf("not put back: %+v", ps)
			}
			if want := c.due(now, loc); ps[0].DueAt.Sub(want).Abs() > 5*time.Second {
				t.Fatalf("due %v, want %v", ps[0].DueAt.In(loc), want.In(loc))
			}
			want := c.reply
			if want == "" {
				want = "Back at " + backAt(ps[0].DueAt, time.Now(), loc) + "."
			}
			if got != want || slices.Contains(td.llm.heard(), c.words) {
				t.Fatalf("reply %q, want %q", got, want)
			}
		})
	}
	t.Run("not a time", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		remindNow(t, td, ownerKey, "call the plumber")
		if got := td.owner(t, "tomorrow morning maybe"); got != "Heard: tomorrow morning maybe" || settled(td, ownerKey) {
			t.Fatalf("reply %q", got)
		}
	})
}

// The third time the same reminder is put back, and never again, the twin
// offers to take it on or drop it.
func TestASlippingReminderIsOfferedOnce(t *testing.T) {
	td := newTestDaemon(t, butler)
	const offer = "This one keeps slipping: shall I take it on, or drop it?"
	id := remindNow(t, td, ownerKey, "call the plumber")
	for i := 1; i <= 5; i++ {
		if i > 1 {
			refire(t, td, id, ownerKey, "call the plumber")
		}
		got := td.owner(t, "later")
		if !strings.HasPrefix(got, "Back at ") || strings.Contains(got, offer) != (i == 3) {
			t.Fatalf("put back %d times: %q", i, got)
		}
	}
	if v, _ := td.store.Get(context.Background(), fmt.Sprintf("reminder.slipping.%d", id)); v == "" {
		t.Fatal("the offer isn't noted")
	}
}

// Only the owner's own one-to-one chat answers a reminder in a word: not
// someone else, and not a group the owner is in.
func TestOnlyTheOwnersOwnChatAnswersAReminder(t *testing.T) {
	ctx := context.Background()
	t.Run("someone else", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		remindNow(t, td, "telegram:999", "bring the ladder back")
		got, err := td.Message(ctx, channels.Inbound{Channel: "telegram", ChatID: "999", Sender: "Bob", Text: "done"})
		if err != nil || got == "Ticked." || settled(td, "telegram:999") {
			t.Fatalf("reply %q %v", got, err)
		}
	})
	t.Run("a group", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		remindNow(t, td, "telegram:family", "book the restaurant")
		got, err := td.Message(ctx, channels.Inbound{Channel: "telegram", ChatID: "family", Sender: "owner", Text: "later", IsOwner: true})
		if err != nil || got != "Heard: later" || settled(td, "telegram:family") {
			t.Fatalf("reply %q %v", got, err)
		}
	})
}

// A reminder on the screen says what kind it is, and its tick ticks it off
// before it goes out: it never does.
func TestTheScreensTickTicksAReminderOff(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, butler)
	id, _ := td.store.AddReminder(ctx, ownerKey, time.Now().Add(time.Hour), "call the plumber")
	sd := td.screenData(ctx)
	if len(sd.Reminders) != 1 || sd.Reminders[0]["id"] != id || sd.Reminders[0]["kind"] != "remind" {
		t.Fatalf("screen reminders %+v", sd.Reminders)
	}
	if err := td.TickReminder(ctx, id); err != nil {
		t.Fatal(err)
	}
	if sd := td.screenData(ctx); len(sd.Reminders) != 0 {
		t.Fatalf("still on the screen: %+v", sd.Reminders)
	}
	if due, _ := td.store.DueReminders(ctx, time.Now().Add(2*time.Hour)); len(due) != 0 {
		t.Fatalf("still due: %+v", due)
	}
	if err := td.TickReminder(ctx, 999); !errors.Is(err, api.ErrNoReminder) {
		t.Fatalf("ticking one that isn't there: %v", err)
	}
	if es, _ := td.store.RecentAuditOfKind(ctx, "reminder.done", 1); len(es) != 1 {
		t.Fatal("not in the activity log")
	}
}
