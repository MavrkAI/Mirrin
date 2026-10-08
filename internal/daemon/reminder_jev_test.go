package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/jev/jevtest"
)

// newReminderJevDaemon is a test daemon with Jev switched on against a
// fake, and the butler brain, which answers "Heard: …" when the model gets
// a message.
func newReminderJevDaemon(t *testing.T) (*testDaemon, *jevtest.Server) {
	t.Helper()
	td := newTestDaemon(t, butler)
	srv := jevtest.New(t)
	td.jev.url = srv.URL
	t.Setenv("TYPESAFE_API_KEY", srv.Key())
	if err := td.UpdateConfig(func(c *config.Config) { c.Jev.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	return td, srv
}

// A reply that isn't one of the exact words, read by Jev as done, ticks
// the reminder off; Jev sees only the reminder and the reply.
func TestJevTicksOffAReminderInOtherWords(t *testing.T) {
	ctx := context.Background()
	for _, words := range []string{"paid it", "✅", "all sorted thanks, it went through fine in the end so we're good"} {
		t.Run(words, func(t *testing.T) {
			td, srv := newReminderJevDaemon(t)
			srv.Choose("reply", "done", 0.95, 0.9)
			remindNow(t, td, ownerKey, "pay the electricity bill")
			if got := td.owner(t, words); got != "Ticked." {
				t.Fatalf("reply %q", got)
			}
			if !settled(td, ownerKey) {
				t.Fatal("not ticked off")
			}
			for _, h := range td.llm.heard() {
				if h == words {
					t.Fatalf("the model heard %q", words)
				}
			}
			h, _ := td.store.History(ctx, ownerKey, 2)
			if len(h) != 2 || h[0].PlainText() != words || h[1].PlainText() != "Ticked." {
				t.Fatalf("history %+v", h)
			}
			es, _ := td.store.RecentAuditOfKind(ctx, "reminder.done", 1)
			if len(es) != 1 || !strings.HasSuffix(es[0].Detail, " (jev)") {
				t.Fatalf("activity %+v", es)
			}
			reqs := srv.Requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests", len(reqs))
			}
			var state map[string]any
			if err := json.Unmarshal(reqs[0].State, &state); err != nil {
				t.Fatal(err)
			}
			if len(state) != 2 || state["twin_said"] != "Reminder: pay the electricity bill" || state["owner_replied"] != words {
				t.Fatalf("state %s", reqs[0].State)
			}
			q := reqs[0].Questions["reply"]
			if q.Type != "choice" || strings.Join(q.Options(), ",") != "done,later,drop,other" {
				t.Fatalf("question %+v", q)
			}
		})
	}
}

// "Not yet", read by Jev as later, puts the reminder back an hour.
func TestJevPutsAReminderBackInOtherWords(t *testing.T) {
	ctx := context.Background()
	td, srv := newReminderJevDaemon(t)
	srv.Choose("reply", "later", 0.9, 0.8)
	remindNow(t, td, ownerKey, "call the plumber")
	now := time.Now()
	got := td.owner(t, "not yet")
	ps, _ := td.store.PendingReminders(ctx, ownerKey)
	if len(ps) != 1 || ps[0].Snoozes != 1 {
		t.Fatalf("not put back: %+v", ps)
	}
	if ps[0].DueAt.Sub(now.Add(time.Hour)).Abs() > 5*time.Second {
		t.Fatalf("due %v", ps[0].DueAt)
	}
	if want := "Back at " + backAt(ps[0].DueAt, time.Now(), td.location()) + "."; got != want {
		t.Fatalf("reply %q, want %q", got, want)
	}
}

// The third time Jev puts the same reminder back, and only then, the twin
// offers to take it on or drop it.
func TestJevSlippingReminderIsOfferedOnce(t *testing.T) {
	td, srv := newReminderJevDaemon(t)
	srv.Choose("reply", "later", 0.9, 0.8)
	const offer = "This one keeps slipping: shall I take it on, or drop it?"
	id := remindNow(t, td, ownerKey, "call the plumber")
	for i := 1; i <= 4; i++ {
		if i > 1 {
			refire(t, td, id, ownerKey, "call the plumber")
		}
		got := td.owner(t, "can't right now")
		if !strings.HasPrefix(got, "Back at ") || strings.Contains(got, offer) != (i == 3) {
			t.Fatalf("put back %d times: %q", i, got)
		}
	}
}

// When Jev isn't sure, or says drop or other, the message is conversation.
func TestJevNotSureLeavesTheReminder(t *testing.T) {
	cases := []struct {
		name          string
		option        string
		p, confidence float64
	}{
		{"done at 0.8", "done", 0.8, 0.9},
		{"done but unsure", "done", 0.95, 0.5},
		{"later at 0.8", "later", 0.8, 0.9},
		{"drop", "drop", 0.95, 0.9},
		{"other", "other", 0.95, 0.9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			td, srv := newReminderJevDaemon(t)
			srv.Choose("reply", c.option, c.p, c.confidence)
			remindNow(t, td, ownerKey, "pay the electricity bill")
			if got := td.owner(t, "paid it"); got != "Heard: paid it" || settled(td, ownerKey) {
				t.Fatalf("reply %q", got)
			}
			if srv.Calls() != 1 {
				t.Fatalf("%d requests", srv.Calls())
			}
		})
	}
}

// With Jev off, other words are conversation, and nothing is sent.
func TestJevOffLeavesTheReminder(t *testing.T) {
	td, srv := newReminderJevDaemon(t)
	if err := td.UpdateConfig(func(c *config.Config) { c.Jev.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	srv.Choose("reply", "done", 0.99, 0.99)
	remindNow(t, td, ownerKey, "pay the electricity bill")
	if got := td.owner(t, "paid it"); got != "Heard: paid it" || settled(td, ownerKey) {
		t.Fatalf("reply %q", got)
	}
	if srv.Calls() != 0 {
		t.Fatalf("%d requests", srv.Calls())
	}
}

// The exact words still settle a reminder without asking Jev.
func TestExactReminderWordsDontAskJev(t *testing.T) {
	for _, words := range []string{"done", "later", "tomorrow 9"} {
		t.Run(words, func(t *testing.T) {
			td, srv := newReminderJevDaemon(t)
			srv.Choose("reply", "other", 0.99, 0.99)
			remindNow(t, td, ownerKey, "call the plumber")
			got := td.owner(t, words)
			if got != "Ticked." && !strings.HasPrefix(got, "Back at ") {
				t.Fatalf("reply %q", got)
			}
			if srv.Calls() != 0 {
				t.Fatalf("%d requests", srv.Calls())
			}
		})
	}
}

// Jev is never asked when the reply can't be about the reminder: the twin
// waits on an approval, the reminder is over half an hour old, the twin
// has said something since, or the message is too long.
func TestJevNotAskedOffTheReminder(t *testing.T) {
	ctx := context.Background()
	t.Run("an open question", func(t *testing.T) {
		td, srv := newReminderJevDaemon(t)
		srv.Choose("reply", "done", 0.99, 0.99)
		td.owner(t, "email the boss")
		remindNow(t, td, ownerKey, "call the plumber")
		td.owner(t, "yes") // "Just to be sure: …"
		if got := td.owner(t, "yep did that"); got == "Ticked." || settled(td, ownerKey) {
			t.Fatalf("reply %q", got)
		}
		if srv.Calls() != 0 {
			t.Fatalf("%d requests", srv.Calls())
		}
	})
	t.Run("31 minutes on", func(t *testing.T) {
		td, srv := newReminderJevDaemon(t)
		srv.Choose("reply", "done", 0.99, 0.99)
		remindNow(t, td, ownerKey, "call the plumber")
		withClock(t, 31*time.Minute)
		if got := td.owner(t, "yep did that"); got != "Heard: yep did that" || settled(td, ownerKey) {
			t.Fatalf("reply %q", got)
		}
		if srv.Calls() != 0 {
			t.Fatalf("%d requests", srv.Calls())
		}
	})
	t.Run("the twin spoke since", func(t *testing.T) {
		td, srv := newReminderJevDaemon(t)
		srv.Choose("reply", "done", 0.99, 0.99)
		remindNow(t, td, ownerKey, "call the plumber")
		_ = td.Notify(ctx, ownerKey, "Your 3pm moved to 4pm.")
		if got := td.owner(t, "yep did that"); got != "Heard: yep did that" || settled(td, ownerKey) {
			t.Fatalf("reply %q", got)
		}
		if srv.Calls() != 0 {
			t.Fatalf("%d requests", srv.Calls())
		}
	})
	t.Run("201 characters", func(t *testing.T) {
		td, srv := newReminderJevDaemon(t)
		srv.Choose("reply", "done", 0.99, 0.99)
		remindNow(t, td, ownerKey, "call the plumber")
		long := "paid it " + strings.Repeat("x", 193)
		if n := len([]rune(long)); n != 201 {
			t.Fatalf("%d characters", n)
		}
		if got := td.owner(t, long); got == "Ticked." || settled(td, ownerKey) {
			t.Fatalf("reply %q", got)
		}
		if srv.Calls() != 0 {
			t.Fatalf("%d requests", srv.Calls())
		}
	})
}

// Only the owner's own chat asks Jev: not someone else, a group or
// background work.
func TestJevOnlyForTheOwnersOwnChat(t *testing.T) {
	ctx := context.Background()
	t.Run("someone else", func(t *testing.T) {
		td, srv := newReminderJevDaemon(t)
		srv.Choose("reply", "done", 0.99, 0.99)
		remindNow(t, td, "telegram:999", "bring the ladder back")
		got, err := td.Message(ctx, channels.Inbound{Channel: "telegram", ChatID: "999", Sender: "Bob", Text: "yep did that"})
		if err != nil || got == "Ticked." || settled(td, "telegram:999") || srv.Calls() != 0 {
			t.Fatalf("reply %q %v, %d requests", got, err, srv.Calls())
		}
	})
	t.Run("a group", func(t *testing.T) {
		td, srv := newReminderJevDaemon(t)
		srv.Choose("reply", "done", 0.99, 0.99)
		remindNow(t, td, "telegram:family", "book the restaurant")
		got, err := td.Message(ctx, channels.Inbound{Channel: "telegram", ChatID: "family", Sender: "owner", Text: "yep did that", IsOwner: true})
		if err != nil || got == "Ticked." || settled(td, "telegram:family") || srv.Calls() != 0 {
			t.Fatalf("reply %q %v, %d requests", got, err, srv.Calls())
		}
	})
	t.Run("scratch", func(t *testing.T) {
		td, srv := newReminderJevDaemon(t)
		srv.Choose("reply", "done", 0.99, 0.99)
		const key = "telegram:owner#protocol-bills"
		remindNow(t, td, key, "pay the electricity bill")
		if got, ok := td.answerReminder(ctx, channels.Inbound{Channel: "telegram", ChatID: "owner#protocol-bills", Sender: "owner", Text: "yep did that", IsOwner: true}); ok {
			t.Fatalf("settled from scratch work: %q", got)
		}
		if srv.Calls() != 0 {
			t.Fatalf("%d requests", srv.Calls())
		}
	})
}

// A refused key, a slow answer or an answer that makes no sense leave the
// message to the model, quickly.
func TestJevFailingLeavesTheReminder(t *testing.T) {
	cases := []struct {
		name   string
		script func(*jevtest.Server)
	}{
		{"refused", func(s *jevtest.Server) { s.Fail(401, 5) }},
		{"slow", func(s *jevtest.Server) { s.Delay(3 * time.Second) }},
		{"unknown option", func(s *jevtest.Server) {
			s.RawAnswer("reply", `{"type":"choice","choice":"maybe","probabilities":{"maybe":0.99,"done":0.01},"confidence":0.99}`)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			td, srv := newReminderJevDaemon(t)
			c.script(srv)
			remindNow(t, td, ownerKey, "pay the electricity bill")
			start := time.Now()
			got, ok := td.answerReminder(context.Background(), channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "paid it", IsOwner: true})
			if took := time.Since(start); took > 2500*time.Millisecond {
				t.Fatalf("took %v", took)
			}
			if ok || got != "" || settled(td, ownerKey) {
				t.Fatalf("settled: %q", got)
			}
			if srv.Calls() == 0 {
				t.Fatal("Jev wasn't asked")
			}
		})
	}
}

// With Jev switched off, every reply to a reminder gets just what it got
// before Jev: the exact words settle it, everything else, short or long,
// is the model's, and nothing is sent.
func TestJevOffRepliesToAReminderAsBefore(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		words string
		want  string // "tick", "back" or "model"
	}{
		{"done", "tick"},
		{"All done.", "tick"},
		{"later", "back"},
		{"tomorrow 9", "back"},
		{"paid it", "model"},
		{"✅", "model"},
		{"!!!", "model"},
		{"not yet", "model"},
		{"done " + strings.Repeat("and dusted ", 4), "model"},
		{"all sorted thanks " + strings.Repeat("it went through ", 8), "model"},
		{"paid it " + strings.Repeat("x", 193), "model"},
		{"(voice note) done", "model"},
	}
	for _, c := range cases {
		t.Run(c.words, func(t *testing.T) {
			var replies []string
			var states []bool
			for _, withJev := range []bool{false, true} {
				var td *testDaemon
				var srv *jevtest.Server
				if withJev {
					td, srv = newReminderJevDaemon(t)
					srv.Choose("reply", "done", 0.99, 0.99)
					if err := td.UpdateConfig(func(c *config.Config) { c.Jev.Enabled = false }); err != nil {
						t.Fatal(err)
					}
				} else {
					td = newTestDaemon(t, butler)
				}
				remindNow(t, td, ownerKey, "pay the electricity bill")
				got := td.owner(t, c.words)
				ok := false
				switch c.want {
				case "tick":
					ok = got == "Ticked."
				case "back":
					ok = strings.HasPrefix(got, "Back at ")
					if ps, _ := td.store.PendingReminders(ctx, ownerKey); len(ps) != 1 || ps[0].Snoozes != 1 {
						ok = false
					}
				case "model":
					ok = got == "Heard: "+strings.TrimSpace(c.words)
				}
				if !ok {
					t.Fatalf("jev configured %v: reply %q, want %s", withJev, got, c.want)
				}
				if srv != nil && srv.Calls() != 0 {
					t.Fatalf("%d requests", srv.Calls())
				}
				replies = append(replies, got)
				states = append(states, settled(td, ownerKey))
			}
			if replies[0] != replies[1] || states[0] != states[1] {
				t.Fatalf("Jev off differs from no Jev: %q/%v vs %q/%v", replies[0], states[0], replies[1], states[1])
			}
			if states[0] != (c.want != "model") {
				t.Fatalf("settled %v", states[0])
			}
		})
	}
}

// A photo, a voice note, a note about an attachment or a message of
// several lines is never read by Jev: the model sees all of it.
func TestJevNotAskedForPhotosVoiceOrSeveralLines(t *testing.T) {
	for _, words := range []string{
		"(photo) paid it, here's the receipt",
		"(voice note) yep did that",
		"paid it\n[A file came with this message; you can't open it on this channel yet.]",
		"paid it\nalso can you book dinner for 7",
	} {
		t.Run(words, func(t *testing.T) {
			td, srv := newReminderJevDaemon(t)
			srv.Choose("reply", "done", 0.99, 0.99)
			remindNow(t, td, ownerKey, "pay the electricity bill")
			if got := td.owner(t, words); got != "Heard: "+words || settled(td, ownerKey) {
				t.Fatalf("reply %q", got)
			}
			if srv.Calls() != 0 {
				t.Fatalf("%d requests", srv.Calls())
			}
		})
	}
}

// A reply that settles the reminder but also asks for something more is
// the model's to answer, and Jev's question says so.
func TestJevQuestionLeavesMoreToTheModel(t *testing.T) {
	td, srv := newReminderJevDaemon(t)
	remindNow(t, td, ownerKey, "pay the electricity bill")
	td.owner(t, "paid it, can you book dinner for 7 tomorrow")
	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	var criteria map[string]string
	if err := json.Unmarshal(reqs[0].Questions["reply"].Criteria, &criteria); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(criteria["other"], "asks it to do something else") {
		t.Fatalf("other: %q", criteria["other"])
	}
	if settled(td, ownerKey) {
		t.Fatal("settled on a neutral answer")
	}
}
