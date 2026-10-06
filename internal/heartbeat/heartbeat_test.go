package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/protocols"
)

// outbox is a fake Sender: it fails for any key in down (or with a '#', as a
// real channel does for a scratch key) and records what got through.
type outbox struct {
	mu    sync.Mutex
	down  map[string]bool
	sent  []string
	tries int
}

func (o *outbox) send(_ context.Context, key, text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tries++
	if o.down[key] || strings.Contains(key, "#") {
		return fmt.Errorf("bad chat id %q", key)
	}
	o.sent = append(o.sent, key+" "+text)
	return nil
}

func (o *outbox) messages() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.sent...)
}

func setup(t *testing.T, run Runner, owner string) (*Heartbeat, *memory.Store, *outbox) {
	t.Helper()
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	o := &outbox{down: map[string]bool{}}
	return New(store, run, o.send, func() string { return owner }, time.UTC, nil), store, o
}

func TestReminderSetDuringAProtocolReachesTheOwner(t *testing.T) {
	ctx := context.Background()
	h, store, o := setup(t, nil, "telegram:123")
	id, _ := store.AddReminder(ctx, "telegram:123#protocol-20260927-070000-1", time.Now().Add(-time.Second), "leave for the airport")
	h.fireReminders(ctx)
	if got := o.messages(); len(got) != 1 || got[0] != "telegram:123 Reminder: leave for the airport" {
		t.Fatalf("want one delivery to telegram:123, got %v", got)
	}
	if due, _ := store.DueReminders(ctx, time.Now()); len(due) != 0 {
		t.Fatalf("reminder #%d should be marked fired", id)
	}
}

func TestReminderFallsBackToTheOwnersChannel(t *testing.T) {
	ctx := context.Background()
	h, store, o := setup(t, nil, "telegram:9")
	o.down["whatsapp:1"] = true
	_, _ = store.AddReminder(ctx, "whatsapp:1", time.Now().Add(-time.Second), "water the plants")
	h.fireReminders(ctx)
	if got := o.messages(); len(got) != 1 || !strings.HasPrefix(got[0], "telegram:9 ") {
		t.Fatalf("want delivery on telegram:9, got %v", got)
	}
}

func TestUndeliverableReminderBacksOffThenGivesUpWithANote(t *testing.T) {
	ctx := context.Background()
	h, store, o := setup(t, nil, "whatsapp:1")
	o.down["whatsapp:1"] = true
	id, _ := store.AddReminder(ctx, "whatsapp:1", time.Now().Add(-time.Second), "call mum")

	h.fireReminders(ctx)
	h.fireReminders(ctx) // the next tick must not hammer the channel
	if o.tries != 1 {
		t.Fatalf("want one attempt before the backoff expires, got %d", o.tries)
	}
	for i := 0; i < len(reminderBackoff); i++ {
		due, _ := store.DueReminders(ctx, time.Now().Add(2*time.Hour))
		if len(due) != 1 {
			t.Fatalf("attempt %d: reminder should still be pending", i+2)
		}
		// Pretend the wait is over.
		_ = store.RetryReminder(ctx, id, due[0].Attempts, time.Now().Add(-time.Second))
		h.fireReminders(ctx)
	}
	if o.tries != len(reminderBackoff)+1 {
		t.Fatalf("want %d attempts in all, got %d", len(reminderBackoff)+1, o.tries)
	}
	if due, _ := store.DueReminders(ctx, time.Now().Add(24*time.Hour)); len(due) != 0 {
		t.Fatal("reminder should be given up, not retried forever")
	}
	h.fireReminders(ctx)
	if o.tries != len(reminderBackoff)+1 {
		t.Fatal("a given-up reminder was tried again")
	}
	hist, _ := store.History(ctx, "whatsapp:1", 5)
	if len(hist) == 0 || !strings.Contains(hist[len(hist)-1].PlainText(), "couldn't deliver the reminder \"call mum\"") {
		t.Fatalf("want a note in the owner's conversation, got %+v", hist)
	}
	if es, _ := store.RecentAuditOfKind(ctx, "reminder.failed", 1); len(es) != 1 {
		t.Fatal("give-up not audited")
	}
}

func TestBadScheduleSkipsOnlyThatProtocol(t *testing.T) {
	h, _, _ := setup(t, nil, "c:1")
	err := h.LoadProtocols([]protocols.Protocol{
		{Name: "a early", Schedule: "0 7 * * *", Prompt: "x"},
		{Name: "b broken", Schedule: "0 0 17 * * 5", Prompt: "x"},
		{Name: "c morning briefing", Schedule: "30 7 * * 1-5", Prompt: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "b broken") || !strings.Contains(err.Error(), "five fields") {
		t.Fatalf("want an error naming the broken protocol, got %v", err)
	}
	if n := len(h.jobs); n != 2 {
		t.Fatalf("want the two good protocols scheduled, got %d", n)
	}
	if CheckSchedule("0 0 17 * * 5") == nil || CheckSchedule("0 17 * * 5") != nil || CheckSchedule("") != nil {
		t.Fatal("CheckSchedule disagrees with the scheduler")
	}
}

// runs are two runs of a protocol back to back, as the scheduler makes them
// (at most one notice a day) and as the owner asks for them from the tray or
// the API (every one answers).
var runs = []struct {
	name      string
	scheduled bool
	notices   int
}{
	{"scheduled", true, 1},
	{"run now", false, 2},
}

func TestProtocolWithMissingVariableIsSkippedAndOwnerTold(t *testing.T) {
	for _, r := range runs {
		t.Run(r.name, func(t *testing.T) {
			ctx := context.Background()
			ran := 0
			h, _, o := setup(t, func(context.Context, string, string) (string, error) { ran++; return "ok", nil }, "c:1")
			p := protocols.Protocol{Name: "commute check", Prompt: "Check traffic from {{origin}} to {{ work }}.",
				Vars: map[string]protocols.Var{"origin": {Description: "Where you leave from.", Required: true}}}
			h.runProtocol(ctx, p, r.scheduled)
			h.runProtocol(ctx, p, r.scheduled)
			if ran != 0 {
				t.Fatal("a protocol with unset variables should not run")
			}
			got := o.messages()
			if len(got) != r.notices || !strings.Contains(got[0], "origin (where you leave from) and work") || !strings.Contains(got[0], "Commute check didn't run") || !strings.Contains(got[0], "then say /reload.") {
				t.Fatalf("want %d plain notices naming the variables, got %v", r.notices, got)
			}
		})
	}
}

func TestProtocolFailureTellsTheOwnerPlainly(t *testing.T) {
	for _, r := range runs {
		t.Run(r.name, func(t *testing.T) {
			ctx := context.Background()
			h, _, o := setup(t, func(context.Context, string, string) (string, error) {
				return "", errors.New(`anthropic 529: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			}, "c:1")
			p := protocols.Protocol{Name: "morning briefing", Schedule: "0 7 * * *", Prompt: "brief me"}
			h.runProtocol(ctx, p, r.scheduled)
			h.runProtocol(ctx, p, r.scheduled)
			got := o.messages()
			if len(got) != r.notices {
				t.Fatalf("want %d failure notices, got %v", r.notices, got)
			}
			if !strings.Contains(got[0], "Morning briefing didn't run this time: the model service was busy") || strings.ContainsAny(got[0], "{}") {
				t.Fatalf("notice should be plain words, got %q", got[0])
			}
		})
	}
}

func TestRunNowAnswersEvenAfterTheDailyNotice(t *testing.T) {
	ctx := context.Background()
	h, _, o := setup(t, func(context.Context, string, string) (string, error) { return "", context.DeadlineExceeded }, "c:1")
	p := protocols.Protocol{Name: "news", Schedule: "0 7 * * *", Prompt: "news"}
	h.runProtocol(ctx, p, true) // the 7am run fails and says so
	h.RunProtocol(ctx, p)       // the owner clicks Run now
	h.runProtocol(ctx, p, true) // a later scheduled failure stays quiet
	if got := o.messages(); len(got) != 2 || !strings.Contains(got[1], "took longer than ten minutes") {
		t.Fatalf("run now should always get an answer, and only it: %v", got)
	}
}

func TestProtocolsDueTogetherGetTheirOwnConversations(t *testing.T) {
	var mu sync.Mutex
	keys := map[string]bool{}
	release := make(chan struct{})
	h, _, _ := setup(t, func(_ context.Context, key, _ string) (string, error) {
		mu.Lock()
		keys[key] = true
		mu.Unlock()
		<-release
		return "NOTHING_TO_REPORT", nil
	}, "c:1")
	var wg sync.WaitGroup
	for _, name := range []string{"news", "weather"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			h.RunProtocol(context.Background(), protocols.Protocol{Name: n, Prompt: n})
		}(name)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if len(keys) != 2 {
		t.Fatalf("two protocols at once should not share a conversation: %v", keys)
	}
}

// The give-up note goes through the daemon's record hook, which holds it
// back while a turn is running in that chat (between a tool call and its
// result it would break the model's tool loop).
func TestGiveUpNoteGoesThroughTheRecordHook(t *testing.T) {
	ctx := context.Background()
	h, store, o := setup(t, nil, "whatsapp:1")
	o.down["whatsapp:1"] = true
	var recorded []string
	h.Record = func(_ context.Context, key string, m llm.Message) { recorded = append(recorded, key+" "+m.PlainText()) }
	id, _ := store.AddReminder(ctx, "whatsapp:1", time.Now().Add(-time.Second), "call mum")
	_ = store.RetryReminder(ctx, id, len(reminderBackoff), time.Now().Add(-time.Second))
	h.fireReminders(ctx)
	if len(recorded) != 1 || !strings.HasPrefix(recorded[0], "whatsapp:1 (I couldn't deliver the reminder") {
		t.Fatalf("recorded %q", recorded)
	}
	if hist, _ := store.History(ctx, "whatsapp:1", 5); len(hist) != 0 {
		t.Fatalf("written around the hook: %+v", hist)
	}
}

// A twin that started without a key runs its protocols anyway; the owner is
// told what is missing, not "something went wrong".
func TestProtocolWithoutAKeySaysWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	h, _, o := setup(t, func(context.Context, string, string) (string, error) {
		return "", &llm.KeyMissingError{Provider: "anthropic", Env: "ANTHROPIC_API_KEY"}
	}, "c:1")
	h.RunProtocol(ctx, protocols.Protocol{Name: "news", Prompt: "news"})
	got := o.messages()
	if len(got) != 1 || !strings.Contains(got[0], "needs an API key") || strings.Contains(got[0], "went wrong") || strings.Contains(got[0], "..") {
		t.Fatalf("owner told %q", got)
	}
}
