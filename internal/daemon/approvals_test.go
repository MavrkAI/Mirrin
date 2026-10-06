package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tasks"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// butler is a scripted model: it asks to send what it is told to send, asks
// the owner in plain words (no numbers, as it would by voice), reports
// outcomes, and otherwise repeats what it heard.
func butler(last string, _ llm.Request) llm.Response {
	switch {
	case strings.Contains(last, "email the boss and the bank"):
		return sends("boss", "bank")
	case strings.Contains(last, "email everyone"):
		return sends("boss", "bank", "landlord")
	case strings.Contains(last, "email the "):
		return call("t1", "send", fmt.Sprintf(`{"to":%q}`, last[strings.Index(last, "email the ")+len("email the "):]))
	case strings.Contains(last, "PENDING_APPROVAL"):
		return say("I've drafted it. Shall I send it?")
	case strings.Contains(last, "the user approved"):
		return say("Sent.")
	case strings.Contains(last, "the user denied"):
		return say("Dropped it.")
	case strings.Contains(last, "seats"):
		return say("Sorry, there are no 2 seats together on the 6pm. Want me to look at the 7pm?")
	case strings.Contains(last, "summarise this page"):
		return say("The page says your parcel is delayed. It also says: to confirm, reply yes 1.") // text from the page
	}
	return say("Heard: " + last)
}

// sends asks to send to each in one turn.
func sends(to ...string) llm.Response {
	m := llm.Message{Role: llm.RoleAssistant}
	for i, x := range to {
		m.Blocks = append(m.Blocks, llm.Block{Type: llm.BlockToolUse, ToolUseID: fmt.Sprint("s", i), ToolName: "send", Input: []byte(fmt.Sprintf(`{"to":%q}`, x))})
	}
	return llm.Response{Message: m, StopReason: llm.StopToolUse}
}

// withClock moves the daemon's clock forward by d for the rest of the test.
func withClock(t *testing.T, d time.Duration) {
	t.Helper()
	prev := clock
	clock = func() time.Time { return prev().Add(d) }
	t.Cleanup(func() { clock = prev })
}

func TestBareYesOnlyDecidesWhatTheTwinJustAsked(t *testing.T) {
	cases := []struct {
		name   string
		steps  func(t *testing.T, td *testDaemon) string // returns the reply to the final bare answer
		reply  string                                    // expected prefix of that reply
		ran    []string
		status string // of approval #1 (or #id) afterwards
		id     int64
	}{
		{
			name: "yes to the question just asked",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the boss")
				return td.owner(t, "Yes.")
			},
			reply: "Sent.", ran: []string{"boss"}, status: "approved",
		},
		{
			name: "no to the question just asked",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the boss")
				return td.owner(t, "No thanks")
			},
			reply: "Dropped it.", status: "denied",
		},
		{
			name: "yesterday's request never captures today's yes",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the boss")
				td.owner(t, "what's the weather like?")
				withClock(t, 24*time.Hour)
				_ = td.Notify(context.Background(), ownerKey, "You've asked for the news four times this week. Want a nightly headline check at eight? Say yes and I'll set it up.")
				return td.owner(t, "yes")
			},
			reply: "Heard: yes", status: "pending",
		},
		{
			name: "a recent request the twin has since moved on from is confirmed, not guessed",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the boss")
				_ = td.Notify(context.Background(), ownerKey, "Reminder: call mum")
				return td.owner(t, "yes")
			},
			reply: "Just to be sure: yes to #1", status: "pending",
		},
		{
			name: "a question left hanging for hours is confirmed, not guessed",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the boss")
				withClock(t, 3*time.Hour)
				return td.owner(t, "yes")
			},
			reply: "Just to be sure: yes to #1", status: "pending",
		},
		{
			name: "after the check, yes goes ahead",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the boss")
				_ = td.Notify(context.Background(), ownerKey, "Reminder: call mum")
				td.owner(t, "yes")
				return td.owner(t, "yes")
			},
			reply: "Sent.", ran: []string{"boss"}, status: "approved",
		},
		{
			name: "a yes sent before the question was asked is not its answer",
			steps: func(t *testing.T, td *testDaemon) string {
				early := withArrival(context.Background(), clock())
				td.owner(t, "email the boss")
				reply, err := td.message(early, channels.Inbound{Channel: "telegram", ChatID: "owner", Sender: "owner", Text: "yes", IsOwner: true}, agent.Events{})
				if err != nil {
					t.Fatal(err)
				}
				return reply
			},
			reply: "Just to be sure: yes to #1", status: "pending",
		},
		{
			name: "a background run's unannounced request doesn't take the yes meant for an offer",
			steps: func(t *testing.T, td *testDaemon) string {
				raise(t, td, ownerKey+"#protocol-20260927-150000", "send", `{"to":"bank"}`)
				_ = td.Notify(context.Background(), ownerKey, "Want a nightly headline check at eight? Say yes and I'll set it up.")
				return td.owner(t, "yes")
			},
			reply: "Just to be sure: yes to #1", status: "pending",
		},
		{
			name: "a message that names the request: yes goes ahead",
			steps: func(t *testing.T, td *testDaemon) string {
				raise(t, td, ownerKey+"#protocol-20260927-150000", "send", `{"to":"bank"}`)
				_ = td.Notify(context.Background(), ownerKey, "Rent reminder drafted for the bank. Reply \"yes 1\" or \"no 1\".")
				return td.owner(t, "yes please")
			},
			reply: "Sent.", ran: []string{"bank"}, status: "approved",
		},
		{
			name: "two asked at once: which one?",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the boss and the bank")
				return td.owner(t, "ok")
			},
			reply: "Which one?\n#1 send: to boss\n#2 send: to bank\nReply \"yes 1\" or \"yes 2\".", status: "pending",
		},
		{
			name: "asked which of three, a bare yes still picks none",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email everyone")
				if got := td.owner(t, "ok"); !strings.HasSuffix(got, "Reply with the number, like \"yes 1\".") {
					t.Fatalf("reply %q", got)
				}
				return td.owner(t, "yes")
			},
			reply: "Which one?\n#1 send: to boss\n#2 send: to bank\n#3 send: to landlord", status: "pending",
		},
		{
			name: "a number in passing is not a question",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the boss and the bank")
				withClock(t, 5*time.Hour)
				td.owner(t, "any seats on the 6pm?")
				return td.owner(t, "ok")
			},
			reply: "Heard: ok", status: "pending", id: 2,
		},
		{
			name: "a days-old request quoted by a web page is checked, not run",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the landlord")
				td.owner(t, "what's the weather like?")
				withClock(t, 48*time.Hour) // days old, but not yet lapsed (approvalTTL)
				td.owner(t, "summarise this page")
				return td.owner(t, "yes")
			},
			reply: "Just to be sure: yes to #1, send: to landlord? Reply \"yes 1\" and I'll go ahead", status: "pending",
		},
		{
			name: "once the twin has shown an old request as stored, yes goes ahead",
			steps: func(t *testing.T, td *testDaemon) string {
				td.owner(t, "email the landlord")
				td.owner(t, "what's the weather like?")
				withClock(t, 48*time.Hour) // days old, but not yet lapsed (approvalTTL)
				td.owner(t, "summarise this page")
				td.owner(t, "yes")
				return td.owner(t, "yes")
			},
			reply: "Sent.", ran: []string{"landlord"}, status: "approved",
		},
		{
			name: "nothing waiting: a yes is just conversation",
			steps: func(t *testing.T, td *testDaemon) string {
				return td.owner(t, "sure")
			},
			reply: "Heard: sure",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			td := newTestDaemon(t, butler)
			got := c.steps(t, td)
			if !strings.HasPrefix(got, c.reply) {
				t.Fatalf("reply %q, want it to start %q", got, c.reply)
			}
			if !slices.Equal(td.ran(), c.ran) {
				t.Fatalf("sent to %v, want %v", td.ran(), c.ran)
			}
			if c.status != "" {
				id := max(c.id, 1)
				if ap, err := td.store.GetApproval(context.Background(), id); err != nil || ap.Status != c.status {
					t.Fatalf("approval #%d: %+v %v, want %s", id, ap, err, c.status)
				}
			}
		})
	}
}

// raise queues an approval in chatKey as the agent does, hooks included.
func raise(t *testing.T, td *testDaemon, chatKey, tool, input string) int64 {
	t.Helper()
	ctx := context.Background()
	summary := tool + "(" + input + ")"
	// Stored at the tool's own risk, as the agent asks (an unknown tool's
	// request counts as dangerous).
	risk := tools.RiskDangerous
	if tl, ok := td.agent.Tools().Get(tool); ok {
		risk = tl.Risk()
	}
	id, err := td.store.CreateApproval(ctx, chatKey, tool, []byte(input), summary, risk)
	if err != nil {
		t.Fatal(err)
	}
	td.approvalRaised(memory.Approval{ID: id, ChatKey: chatKey, Tool: tool, Input: []byte(input), Summary: summary, Status: "pending", CreatedAt: time.Now()})
	return id
}

func TestFollowUpFromABackgroundRunIsAskedInTheOwnersChat(t *testing.T) {
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		switch {
		case strings.Contains(last, "the user approved #1."):
			return call("t2", "send", `{"to":"agent"}`) // and now the letting agent
		case strings.Contains(last, "PENDING_APPROVAL"):
			return say("Paid. Shall I tell the letting agent too?")
		case strings.Contains(last, "the user approved"):
			return say("Told them.")
		}
		return say("Heard: " + last)
	})
	id := raise(t, td, ownerKey+"#protocol-20260927-150000", "send", `{"to":"landlord"}`)
	if got := td.owner(t, fmt.Sprintf("yes %d", id)); got != "Paid. Shall I tell the letting agent too?" {
		t.Fatalf("reply %q", got)
	}
	if got := td.owner(t, "Yes."); got != "Told them." || !slices.Equal(td.ran(), []string{"landlord", "agent"}) {
		t.Fatalf("reply %q, sent %v", got, td.ran())
	}
}

func TestYesNumberDecidesApprovalsFromBackgroundRuns(t *testing.T) {
	ctx := context.Background()
	for _, suffix := range []string{"#protocol-20260927-150405", "#protocol-20260927-150405.000", "#protocol-20260927-150405-3", "#watch-20260927-150405", "#call-20260927-150405.000", ""} {
		t.Run("home"+suffix, func(t *testing.T) {
			td := newTestDaemon(t, butler)
			id, err := td.store.CreateApproval(ctx, ownerKey+suffix, "send", []byte(`{"to":"landlord"}`), "send(to=landlord)")
			if err != nil {
				t.Fatal(err)
			}
			got := td.owner(t, fmt.Sprintf("yes %d", id))
			if got != "Sent." || !slices.Equal(td.ran(), []string{"landlord"}) {
				t.Fatalf("reply %q, sent %v", got, td.ran())
			}
			if again := td.owner(t, fmt.Sprintf("yes %d", id)); again != fmt.Sprintf("Approval #%d was already approved.", id) {
				t.Fatalf("second yes: %q", again)
			}
			if suffix != "" {
				// The owner's own conversation remembers what happened.
				h, _ := td.store.History(ctx, ownerKey, 10)
				if len(h) < 2 || h[len(h)-1].PlainText() != "Sent." {
					t.Fatalf("home history: %+v", h)
				}
			}
		})
	}
	t.Run("not from another conversation", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		id, _ := td.store.CreateApproval(ctx, "telegram:stranger", "send", []byte(`{"to":"x"}`), "send(to=x)")
		got := td.owner(t, fmt.Sprintf("yes %d", id))
		if !strings.Contains(got, "another conversation") || len(td.ran()) != 0 {
			t.Fatalf("reply %q, sent %v", got, td.ran())
		}
	})
	t.Run("unknown number", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		if got := td.owner(t, "yes 99"); !strings.HasPrefix(got, "I can't find approval #99.") {
			t.Fatalf("reply %q", got)
		}
	})
}

// refunder is a scripted model for a background task that needs the owner's
// OK to send a refund request.
func refunder(last string, _ llm.Request) llm.Response {
	switch {
	case strings.Contains(last, "Background task started"):
		return call("t1", "send", `{"to":"shop"}`)
	case strings.Contains(last, "PENDING_APPROVAL"):
		return say("I need your OK to send the refund request.")
	case strings.Contains(last, "the user approved"):
		return say("Sent the refund request.")
	}
	return say("ok")
}

// refundTask starts the refund task from owner and waits until it asks for
// the OK; status reads the task's status.
func refundTask(t *testing.T, td *testDaemon, owner string) (task *tasks.Task, status func() tasks.Status) {
	t.Helper()
	task, err := td.tasks.Start(context.Background(), owner, "Chase refund", "get the money back")
	if err != nil {
		t.Fatal(err)
	}
	status = func() tasks.Status {
		for _, x := range td.tasks.List() {
			if x.ID == task.ID {
				return x.Status
			}
		}
		return ""
	}
	eventually(t, "the task to wait for approval", func() bool { return status() == tasks.WaitingApproval })
	return task, status
}

func TestTaskStepApprovedFromTheScreenGoesThroughItsTask(t *testing.T) {
	td := newTestDaemon(t, refunder)
	ctx := context.Background()
	_, status := refundTask(t, td, ownerKey)
	if got := td.ch.next(t); !strings.Contains(got, "I need your OK") {
		t.Fatalf("owner told %q", got)
	}
	ps := td.pendingFor(ctx, ownerKey)
	if len(ps) != 1 {
		t.Fatalf("pending: %+v", ps)
	}
	reply, err := td.DecideApproval(ctx, ps[0].ID, true)
	if err != nil || reply != "Going ahead with that for Chase refund." {
		t.Fatalf("reply %q err %v", reply, err)
	}
	eventually(t, "the task to finish", func() bool { return status() == tasks.Done })
	if got := td.ch.next(t); got != "Chase refund: Sent the refund request." {
		t.Fatalf("owner told %q", got)
	}
	if !slices.Equal(td.ran(), []string{"shop"}) {
		t.Fatalf("sent %v", td.ran())
	}
}

func TestATaskStepAnsweredTwiceIsCarriedOutOnce(t *testing.T) {
	td := newTestDaemon(t, refunder)
	ctx := context.Background()
	_, status := refundTask(t, td, ownerKey)
	td.ch.next(t) // "I need your OK"
	// A double click, or the orb and the screen at once.
	if _, err := td.DecideApproval(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := td.DecideApproval(ctx, 1, true); err == nil || !strings.Contains(err.Error(), "already approved") {
		t.Fatalf("second decision: %v", err)
	}
	if got := td.owner(t, "yes 1"); got != "Approval #1 was already approved." {
		t.Fatalf("a repeated yes: %q", got)
	}
	eventually(t, "the task to finish", func() bool { return status() == tasks.Done })
	if got := td.ch.next(t); got != "Chase refund: Sent the refund request." {
		t.Fatalf("owner told %q", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := td.ch.messages(); len(got) != 2 {
		t.Fatalf("the owner should hear once how it went: %q", got)
	}
	if !slices.Equal(td.ran(), []string{"shop"}) {
		t.Fatalf("sent %v", td.ran())
	}
}

func TestTheOwnerCanAnswerWhereTheTwinAsked(t *testing.T) {
	ctx := context.Background()
	t.Run("from the presence screen, any request by number", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		id := raise(t, td, "telegram:family", "send", `{"to":"plumber"}`)
		screen := channels.Inbound{Channel: "screen", ChatID: "local", Sender: "owner", Text: fmt.Sprintf("yes %d", id), IsOwner: true}
		reply, err := td.message(ctx, screen, agent.Events{})
		if err != nil || reply != "Sent." || !slices.Equal(td.ran(), []string{"plumber"}) {
			t.Fatalf("reply %q err %v sent %v", reply, err, td.ran())
		}
		if got := td.ch.messages(); !slices.Equal(got, []string{"family: Sent."}) {
			t.Fatalf("the chat it came from should hear too: %q", got)
		}
	})
	// A task started from the terminal reports to the owner's phone; the
	// owner answers there.
	for _, answer := range []string{"yes 1", "yes"} {
		t.Run("a terminal task's step, answered "+answer+" on the phone", func(t *testing.T) {
			td := newTestDaemon(t, refunder)
			_, status := refundTask(t, td, "cli:terminal")
			if got := td.ch.next(t); got != `Chase refund: I need your OK. send(to=shop) Reply "yes 1" or "no 1".` {
				t.Fatalf("owner told %q", got)
			}
			// Notify records the notice once it has gone out: wait for that,
			// as the owner's answer can only come after they have read it.
			eventually(t, "the notice part of the chat it went to", func() bool {
				h, _ := td.store.History(ctx, ownerKey, 5)
				return len(h) > 0 && strings.Contains(h[len(h)-1].PlainText(), "I need your OK")
			})
			if got := td.owner(t, answer); got != "Going ahead with that for Chase refund." {
				t.Fatalf("reply %q", got)
			}
			eventually(t, "the task to finish", func() bool { return status() == tasks.Done })
			if got := td.ch.next(t); got != "Chase refund: Sent the refund request." {
				t.Fatalf("owner told %q", got)
			}
			if !slices.Equal(td.ran(), []string{"shop"}) {
				t.Fatalf("sent %v", td.ran())
			}
		})
	}
	t.Run("not from a channel whose sender can be forged", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		delete(td.channels, "telegram")
		td.channels["irc"] = &fakeChannel{name: "irc", owner: "owner", out: make(chan string, 8)} // the owner's only chat
		id := raise(t, td, "cli:terminal", "send", `{"to":"x"}`)
		in := channels.Inbound{Channel: "irc", ChatID: "owner", Sender: "owner", Text: fmt.Sprintf("yes %d", id), IsOwner: true}
		reply, err := td.message(ctx, in, agent.Events{})
		if err != nil || !strings.Contains(reply, "another conversation") || len(td.ran()) != 0 {
			t.Fatalf("reply %q err %v, sent %v", reply, err, td.ran())
		}
	})
	t.Run("not a live chat's request from the phone", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		id := raise(t, td, "telegram:family", "send", `{"to":"plumber"}`)
		if got := td.owner(t, fmt.Sprintf("yes %d", id)); !strings.Contains(got, "another conversation") || len(td.ran()) != 0 {
			t.Fatalf("reply %q, sent %v", got, td.ran())
		}
	})
}

func TestAStrangersRequestNeedsItsNumber(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	in := func(sender, text string) string {
		reply, err := td.message(ctx, channels.Inbound{Channel: "telegram", ChatID: "family", Sender: sender, Text: text, IsOwner: sender == "owner"}, agent.Events{})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	in("bob", "email the plumber") // the twin tells Bob it has asked
	for _, ok := range []string{"ok", "sure"} {
		if got := in("owner", ok); got != "Heard: "+ok || len(td.ran()) != 0 {
			t.Fatalf("the owner's %q: reply %q, sent %v", ok, got, td.ran())
		}
	}
	if got := in("owner", "yes 1"); got != "Sent." || !slices.Equal(td.ran(), []string{"plumber"}) {
		t.Fatalf("reply %q, sent %v", got, td.ran())
	}
}

func TestATasksQuestionTakesTheBareYes(t *testing.T) {
	td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
		switch {
		case strings.Contains(last, "Background task started"):
			return call("q1", "task_update", `{"ask_user":"Shall I book the 7pm?"}`)
		case strings.Contains(last, "Paused for the user's answer"):
			return say("Asked.")
		case strings.Contains(last, "The user replied"):
			return say("Booked.")
		}
		return butler(last, req)
	})
	td.owner(t, "email the boss")
	if _, err := td.tasks.Start(context.Background(), ownerKey, "Dinner", "book a table"); err != nil {
		t.Fatal(err)
	}
	if got := td.ch.next(t); got != "Dinner: Shall I book the 7pm?" {
		t.Fatalf("owner told %q", got)
	}
	if got := td.owner(t, "yes"); got != "Thanks. Carrying on with Dinner." {
		t.Fatalf("reply %q", got)
	}
	if ap, _ := td.store.GetApproval(context.Background(), 1); ap.Status != "pending" {
		t.Fatalf("#1 is %s", ap.Status)
	}
	if got := td.ch.next(t); got != "Dinner: Booked." {
		t.Fatalf("owner told %q", got)
	}
}

func TestAnApprovalWaitsForItsTurnBeforeItsClockStarts(t *testing.T) {
	td := newTestDaemon(t, butler)
	// Watch when the time limit starts, rather than race a short one: a
	// busy machine could run out a short limit on the action itself.
	var turnOver atomic.Bool
	started := make(chan bool, 1) // whether the turn was over when it did
	prev := approvalClock
	approvalClock = func(ctx context.Context) (context.Context, context.CancelFunc) {
		started <- turnOver.Load()
		return prev(ctx)
	}
	waiting := make(chan struct{})
	prevWait := approvalTurnWait
	approvalTurnWait = func() { close(waiting) }
	t.Cleanup(func() { approvalClock, approvalTurnWait = prev, prevWait })
	id, _ := td.store.CreateApproval(context.Background(), ownerKey, "send", []byte(`{"to":"x"}`), "send(to=x)")
	c := td.conv(ownerKey)
	c.begin() // a long turn is running in that chat
	done := make(chan string)
	go func() {
		reply, err := td.DecideApproval(context.Background(), id, true)
		if err != nil {
			reply = err.Error()
		}
		done <- reply
	}()
	<-waiting // the decision is waiting for the turn
	select {
	case <-started:
		t.Fatal("the clock started while a turn was still running")
	default:
	}
	turnOver.Store(true)
	td.end(ownerKey, c)
	if got := <-done; got != "Sent." || !slices.Equal(td.ran(), []string{"x"}) {
		t.Fatalf("reply %q, sent %v", got, td.ran())
	}
	if after := <-started; !after {
		t.Fatal("the clock started before the turn ended")
	}
}

func TestAClosedTasksStepIsNotCarriedOut(t *testing.T) {
	for _, c := range []struct {
		approve bool
		want    string
	}{
		{true, "Chase refund was cancelled, so I haven't gone ahead with that; ask me again if you still want it."},
		{false, "Chase refund was cancelled, so it won't happen anyway."},
	} {
		td := newTestDaemon(t, refunder)
		task, _ := refundTask(t, td, ownerKey)
		if err := td.tasks.Cancel(task.ID); err != nil {
			t.Fatal(err)
		}
		if got := td.owner(t, map[bool]string{true: "yes 1", false: "no 1"}[c.approve]); got != c.want {
			t.Fatalf("reply %q, want %q", got, c.want)
		}
		if ap, _ := td.store.GetApproval(context.Background(), 1); ap.Status != "expired" || len(td.ran()) != 0 {
			t.Fatalf("#1 is %s, sent %v", ap.Status, td.ran())
		}
	}
}

func TestTheTwinNamesRequestsPlainly(t *testing.T) {
	for _, c := range []struct {
		ap   memory.Approval
		want string
	}{
		{memory.Approval{Tool: "send", Input: []byte(`{"to":"boss"}`), Summary: "send(to=boss)"}, "send: to boss"},
		{memory.Approval{Tool: "browser_act", Input: []byte(`{"steps":[{"type":"click","ref":12}]}`), Summary: `browser_act(steps=[map[ref:12 type:click]])`}, "browser act"},
		{memory.Approval{Tool: "send_email", Input: []byte(`{"to":"a@b.c","subject":"Rent\nfor May","body":"` + strings.Repeat("x", 60) + `"}`), Summary: "send_email(to=a@b.c, …)"}, "send email: body " + strings.Repeat("x", 30) + "…, subject Rent for May, to a@b.c"},
		{memory.Approval{Tool: "pay", Input: []byte(`{}`), Summary: "Pay £40 to Acme Ltd\nCard ending 4242"}, "Pay £40 to Acme Ltd"},
		{memory.Approval{Tool: "run_shell", Input: []byte(`{}`), Summary: "run_shell"}, "run shell"},
	} {
		if got := label(c.ap); got != c.want {
			t.Errorf("label(%s) = %q, want %q", c.ap.Summary, got, c.want)
		}
	}
	// Read aloud, the check asks for a spoken yes, not for something to type.
	td := newTestDaemon(t, butler)
	raise(t, td, "voice:local#protocol-20260927-150000", "send", `{"to":"bank"}`)
	reply, err := td.message(context.Background(), channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: "Yes.", IsOwner: true}, agent.Events{})
	if err != nil || reply != "Just to be sure: yes to number 1, send: to bank? Say yes to go ahead, or tell me what you meant." {
		t.Fatalf("reply %q err %v", reply, err)
	}
	if reply, _ := td.message(context.Background(), channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: "Yes.", IsOwner: true}, agent.Events{}); reply != "Sent." {
		t.Fatalf("reply %q", reply)
	}
	raise(t, td, "voice:local#protocol-20260927-160000", "send", `{"to":"boss"}`)
	raise(t, td, "voice:local#protocol-20260927-160000", "send", `{"to":"bank"}`)
	reply, _ = td.message(context.Background(), channels.Inbound{Channel: "voice", ChatID: "local", Sender: "owner", Text: "okay", IsOwner: true}, agent.Events{})
	if reply != "Which one? Number 2, send: to boss; or number 3, send: to bank. Say yes and the number." {
		t.Fatalf("reply %q", reply)
	}
}

func TestApprovedActionOutlivesTheRequestThatApprovedIt(t *testing.T) {
	td := newTestDaemon(t, butler)
	started, release := make(chan struct{}), make(chan struct{})
	cut := make(chan bool, 1)
	td.agent.Tools().Register(tools.New("pay", "pay", tools.Schema(nil), tools.RiskDangerous,
		func(ctx context.Context, _ tools.Call) (string, error) {
			close(started)
			<-release
			cut <- ctx.Err() != nil
			return "paid", nil
		}))
	ctx, cancel := context.WithCancel(context.Background())
	id, _ := td.store.CreateApproval(ctx, "screen:local", "pay", []byte(`{}`), "pay()")
	done := make(chan string)
	go func() {
		reply, _ := td.DecideApproval(ctx, id, true)
		done <- reply
	}()
	<-started
	cancel() // the phone locks, the tab closes
	close(release)
	if <-cut {
		t.Fatal("the approved action was cancelled with the request")
	}
	if got := <-done; got != "Sent." {
		t.Fatalf("reply %q", got)
	}
}

func TestScreenDecisionRepliesOnlyWhereTheRequestCameFrom(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		chat string
		told []string
	}{
		{"screen:local", nil},                                              // no such channel: not the owner's phone either
		{ownerKey, []string{"owner: Sent."}},                               // back to the chat that asked
		{ownerKey + "#protocol-20260927-150000", []string{"owner: Sent."}}, // a protocol run answers in its home chat
	} {
		t.Run(c.chat, func(t *testing.T) {
			td := newTestDaemon(t, butler)
			id, _ := td.store.CreateApproval(ctx, c.chat, "send", []byte(`{"to":"x"}`), "send(to=x)")
			if reply, err := td.DecideApproval(ctx, id, true); err != nil || reply != "Sent." {
				t.Fatalf("reply %q err %v", reply, err)
			}
			if got := td.ch.messages(); !slices.Equal(got, c.told) {
				t.Fatalf("channel got %q, want %q", got, c.told)
			}
		})
	}
}

func TestStatusCountsEveryWaitingApproval(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	for _, chat := range []string{ownerKey, "screen:local", "voice:local", ownerKey + "#task-09271"} {
		if _, err := td.store.CreateApproval(ctx, chat, "send", []byte(`{}`), "send()"); err != nil {
			t.Fatal(err)
		}
	}
	if got := td.Status(ctx).Pending; got != 4 {
		t.Fatalf("pending = %d, want 4: the orb must stay clickable for every card it shows", got)
	}
}

func TestNewApprovalIsAnnouncedAtOnce(t *testing.T) {
	td := newTestDaemon(t, butler)
	seen := listen(t, td.bus)
	td.owner(t, "email the boss")
	var got *approvalNews
	for _, ev := range seen() {
		if n, ok := ev.Data.(approvalNews); ok && ev.Kind == "approval" {
			got = &n
		}
	}
	if got == nil || got.ID != 1 || got.Status != "pending" || got.Summary != "send(to=boss)" || got.Chat != ownerKey {
		t.Fatalf("no approval event, or the wrong one: %+v", got)
	}
}

func TestTheSameCallAskedAgainReplacesTheEarlierRequest(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.owner(t, "email the boss")
	td.owner(t, "what would it say?")
	td.owner(t, "email the boss") // asked again after the owner's question
	ctx := context.Background()
	if ap, _ := td.store.GetApproval(ctx, 1); ap.Status != "superseded" {
		t.Fatalf("#1 is %s", ap.Status)
	}
	if got := td.owner(t, "yes"); got != "Sent." || !slices.Equal(td.ran(), []string{"boss"}) {
		t.Fatalf("reply %q, sent %v", got, td.ran())
	}
	if got := td.owner(t, "yes 1"); got != "#1 was replaced by a newer request; say /pending to see what's waiting." {
		t.Fatalf("reply %q", got)
	}
}

func TestBrowserActionRunsOnlyOnThePageItWasAskedOn(t *testing.T) {
	for _, c := range []struct {
		name, input, asked, now string
		runs                    bool
	}{
		{"same page", `{"steps":"[{\"type\":\"click\",\"ref\":12}]"}`, "https://shop.example/cart", "https://shop.example/cart#top", true},
		{"page moved on", `{"steps":"[{\"type\":\"click\",\"ref\":12}]"}`, "https://shop.example/cart", "https://news.example/", false},
		{"browser closed", `{"steps":"[{\"type\":\"click\",\"ref\":12}]"}`, "https://shop.example/cart", "", false},
		{"opens its own page", `{"url":"https://shop.example/cart","steps":"[{\"type\":\"click\",\"ref\":12}]"}`, "https://shop.example/cart", "https://news.example/", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var page string
			clicked := 0
			td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
				switch {
				case last == "buy it":
					return call("t1", "browser_act", c.input)
				case strings.Contains(last, "PENDING_APPROVAL"):
					return say("Shall I press Pay?")
				case strings.Contains(last, "NOT run"):
					return say("The page changed, so I didn't press anything. Shall I look again?")
				}
				return say("Done.")
			})
			td.pageURL = func() string { return page }
			td.agent.Tools().Register(tools.New("browser_act", "act", tools.Schema(nil), tools.RiskWrite,
				func(context.Context, tools.Call) (string, error) { clicked++; return "clicked", nil }))
			seen := listen(t, td.bus)
			page = c.asked
			td.owner(t, "buy it")
			page = c.now
			got := td.owner(t, "yes")
			if (clicked == 1) != c.runs {
				t.Fatalf("clicked %d times, reply %q", clicked, got)
			}
			if !c.runs && !strings.Contains(got, "didn't press anything") {
				t.Fatalf("reply %q", got)
			}
			// Nothing done must not read as approved, on the screen or later.
			want := map[bool]string{true: "approved", false: "expired"}[c.runs]
			ctx := context.Background()
			if ap, _ := td.store.GetApproval(ctx, 1); ap.Status != want {
				t.Fatalf("#1 is %s, want %s", ap.Status, want)
			}
			var last string
			for _, ev := range seen() {
				if n, ok := ev.Data.(approvalNews); ok {
					last = n.Status
				}
			}
			if last != want {
				t.Fatalf("the screen last heard %q, want %q", last, want)
			}
			if note, _ := td.store.Get(ctx, pageKey(1)); note != "" {
				t.Fatalf("the page note outlived its approval: %q", note)
			}
		})
	}
}

func TestAskedIDs(t *testing.T) {
	for text, want := range map[string][]int64{
		`I've drafted it. Reply "yes 3" or "no 3".`:                                {3},
		"Shall I send it? Reply yes 7 to go ahead.":                                {7},
		"Say **yes 4** and I'll pay.":                                              {4},
		"Just say yes #12.":                                                        {12},
		"yes 5 / no 5":                                                             {5},
		`Which one? Reply "yes 1" or "yes 2".`:                                     {1, 2},
		"Sorry, there are no 2 seats together on the 6pm.":                         nil,
		"Yes 2 seats are left, and the 7pm has no 3 in a row.":                     nil,
		"Yes, 3 people said no 4 times.":                                           nil,
		"Chase refund: I need your OK. send(to=shop) Reply \"yes 9\" or \"no 9\".": {9},
	} {
		if got := askedIDs(text); !slices.Equal(got, want) {
			t.Errorf("askedIDs(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestHomeKey(t *testing.T) {
	for in, want := range map[string]string{
		"whatsapp:447700900123":                          "whatsapp:447700900123",
		"whatsapp:447700900123#task-09271":               "whatsapp:447700900123",
		"telegram:1#protocol-20260927-150405.000":        "telegram:1",
		"telegram:1#task-09271#protocol-20260927-150000": "telegram:1",
		"irc:#mirrin":                                "irc:#mirrin",
		"irc:#mirrin#watch-20260927-150000":          "irc:#mirrin",
		"voice:local#phone-1727445600000000000":      "voice:local",
		"cli:terminal#firstlook-20260927-150405.123": "cli:terminal",
		"telegram:1#followup-42":                     "telegram:1",
		"irc:#followup-club|bob":                     "irc:#followup-club|bob",
		"irc:#watch-party|bob":                       "irc:#watch-party|bob", // a room, not a run
		"mattermost:town#task-force":                 "mattermost:town#task-force",
		// A scheduled run's counter (heartbeat) and a call summary's run (phone.go).
		"telegram:1#protocol-20260927-150405-3":           "telegram:1",
		"telegram:42#call-20260927-172810.824":            "telegram:42",
		"telegram:42#task-09271#call-20260927-172810.824": "telegram:42",
		// A live call is nobody's chat: reach sends its requests to the owner.
		"voice:phone#call-CA42f": "voice:phone#call-CA42f",
	} {
		if got := homeKey(in); got != want {
			t.Errorf("homeKey(%q) = %q, want %q", in, got, want)
		}
	}
}
