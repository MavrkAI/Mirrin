package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tasks"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// hookLog records what OnApproval hears.
type hookLog struct {
	mu    sync.Mutex
	heard []string
}

func (h *hookLog) hear(_ context.Context, e agent.ApprovalEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heard = append(h.heard, fmt.Sprintf("#%d %s", e.ID, e.Status))
}

func (h *hookLog) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.heard...)
}

// Push and step-up need to hear about approvals reliably and to know the
// risk the owner was shown. The events bus drops events for a listener that
// falls behind; OnApproval doesn't: once per request and once per outcome,
// whatever else is listening.
func TestOnApprovalHearsEachStepEvenWhenTheBusIsBlocked(t *testing.T) {
	td := newTestDaemon(t, butler)
	stuck, stop := td.bus.Subscribe() // never read: a listener that has fallen behind
	defer stop()
	_ = stuck
	h := &hookLog{}
	td.agent.OnApproval(h.hear)
	ctx := context.Background()
	for i := 0; i < 80; i++ { // fill the stuck listener's buffer and then some
		td.bus.Publish(events.Event{Kind: "note", Text: fmt.Sprint(i)})
	}
	td.owner(t, "email the boss") // #1
	td.owner(t, "yes")
	td.owner(t, "email the bank") // #2
	if _, err := td.DecideApprovalBy(ctx, 2, false, Decider{DeviceID: "d1", DeviceName: "Akshay's iPhone", Method: "passkey", Via: "relay r2", IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	td.owner(t, "email the landlord") // #3
	td.owner(t, "what would it say?")
	td.owner(t, "email the landlord") // #4 replaces #3
	want := []string{"#1 pending", "#1 approved", "#2 pending", "#2 denied", "#3 pending", "#3 superseded", "#4 pending"}
	if got := h.all(); !slices.Equal(got, want) {
		t.Fatalf("heard %v\nwant  %v", got, want)
	}
	ap, _ := td.store.GetApproval(ctx, 2)
	if ap.Risk != tools.RiskWrite || ap.DecidedBy != "Akshay's iPhone [d1] (passkey, relay r2, 203.0.113.9)" {
		t.Fatalf("#2 kept %+v", ap)
	}
	es, _ := td.store.RecentAuditOfKind(ctx, "approval.denied", 1)
	if len(es) != 1 || !strings.HasPrefix(es[0].Detail, "#2 by Akshay's iPhone [d1] (passkey, relay r2, 203.0.113.9)") {
		t.Fatalf("audit %+v", es)
	}
	if one, _ := td.store.GetApproval(ctx, 1); one.DecidedBy != "the owner (channel, telegram)" {
		t.Fatalf("#1 decided by %q", one.DecidedBy)
	}
}

// A screen hears the risk the owner is asked at, and who decided.
func TestApprovalNewsCarriesRiskAndDecider(t *testing.T) {
	td := newTestDaemon(t, butler)
	seen := listen(t, td.bus)
	td.owner(t, "email the boss")
	td.owner(t, "yes")
	var got []approvalNews
	for _, ev := range seen() {
		if n, ok := ev.Data.(approvalNews); ok {
			got = append(got, n)
		}
	}
	if len(got) != 2 || got[0].Risk != "write" || got[0].Status != "pending" || got[1].Status != "approved" || got[1].By != "the owner (channel, telegram)" {
		t.Fatalf("news %+v", got)
	}
}

// A request left unanswered lapses after approvalTTL: it reads expired,
// screens hear so, and a late "yes" says it lapsed instead of acting on a
// stale request.
func TestAnUnansweredRequestLapses(t *testing.T) {
	for _, sweep := range []bool{true, false} {
		t.Run(fmt.Sprint("swept ", sweep), func(t *testing.T) {
			td := newTestDaemon(t, butler)
			seen := listen(t, td.bus)
			td.owner(t, "email the boss")
			ctx := context.Background()
			withClock(t, approvalTTL+time.Minute)
			if sweep {
				td.expireApprovals(ctx)
				if ap, _ := td.store.GetApproval(ctx, 1); ap.Status != "expired" {
					t.Fatalf("#1 is %s", ap.Status)
				}
			}
			if got := td.owner(t, "yes 1"); got != "#1 has lapsed; ask me again if you still want it." {
				t.Fatalf("reply %q", got)
			}
			if len(td.ran()) != 0 {
				t.Fatalf("a lapsed request ran: %v", td.ran())
			}
			if ap, _ := td.store.GetApproval(ctx, 1); ap.Status != "expired" {
				t.Fatalf("#1 is %s", ap.Status)
			}
			var last string
			for _, ev := range seen() {
				if n, ok := ev.Data.(approvalNews); ok {
					last = n.Status
				}
			}
			if last != "expired" {
				t.Fatalf("the screen last heard %q", last)
			}
			es, _ := td.store.RecentAuditOfKind(ctx, "approval.expired", 1)
			if len(es) != 1 || !strings.Contains(es[0].Detail, "no answer in 3 days") {
				t.Fatalf("audit %+v", es)
			}
			if got := td.pendingFor(ctx, ownerKey); len(got) != 0 {
				t.Fatalf("still listed as waiting: %+v", got)
			}
		})
	}
	t.Run("a bare yes never picks it", func(t *testing.T) {
		td := newTestDaemon(t, butler)
		td.owner(t, "email the landlord")
		td.owner(t, "what's the weather like?")
		withClock(t, approvalTTL+time.Minute)
		td.owner(t, "summarise this page") // the page quotes "reply yes 1"
		if got := td.owner(t, "yes"); !strings.HasPrefix(got, "Heard: yes") || len(td.ran()) != 0 {
			t.Fatalf("reply %q, sent %v", got, td.ran())
		}
	})
}

// A background task waiting on a request that lapses is set aside, not left
// waiting forever, and the owner hears once how to pick it up again.
func TestATasksLapsedRequestSetsItAside(t *testing.T) {
	td := newTestDaemon(t, refunder)
	task, status := refundTask(t, td, ownerKey)
	td.ch.next(t) // "I need your OK"
	// The notice reads the clock as it is recorded: let it land first.
	eventually(t, "the notice recorded", func() bool {
		h, _ := td.store.History(context.Background(), ownerKey, 5)
		return len(h) > 0 && strings.Contains(h[len(h)-1].PlainText(), "I need your OK")
	})
	withClock(t, approvalTTL+time.Minute)
	td.expireApprovals(context.Background())
	if got := status(); got != tasks.Paused {
		t.Fatalf("task is %s", got)
	}
	if got := td.ch.next(t); !strings.Contains(got, "didn't hear back") || !strings.Contains(got, `"retry task `+task.ID+`"`) {
		t.Fatalf("owner told %q", got)
	}
	eventually(t, "the notice recorded", func() bool {
		h, _ := td.store.History(context.Background(), ownerKey, 5)
		return len(h) > 0 && strings.Contains(h[len(h)-1].PlainText(), "didn't hear back")
	})
}

// Two routine runs from the same chat asking for the same thing would both
// wait and, both approved, do it twice: the later request replaces the
// earlier. A task's request is its own, and so is another chat's.
func TestTheSameRequestFromAnotherRunOfTheSameHomeReplacesIt(t *testing.T) {
	ctx := context.Background()
	td := newTestDaemon(t, butler)
	first := raise(t, td, ownerKey+"#protocol-20260926-080000", "send", `{"to":"landlord"}`)
	task := raise(t, td, ownerKey+"#task-09271", "send", `{"to":"landlord"}`)
	other := raise(t, td, "telegram:family#protocol-20260926-080000", "send", `{"to":"landlord"}`)
	second := raise(t, td, ownerKey+"#watch-20260927-080000", "send", `{"to":"landlord"}`)
	for id, want := range map[int64]string{first: "superseded", task: "pending", other: "pending", second: "pending"} {
		if ap, _ := td.store.GetApproval(ctx, id); ap.Status != want {
			t.Errorf("#%d is %s, want %s", id, ap.Status, want)
		}
	}
}

// A task step approved while the task is busy waits for its turn; if the
// task is cancelled first, the step never runs, and the request must not
// go on reading "approved".
func TestAnApprovedTaskStepThatNeverRunsIsMarkedNotDone(t *testing.T) {
	td := newTestDaemon(t, refunder)
	ctx := context.Background()
	task, _ := refundTask(t, td, ownerKey)
	td.ch.next(t)
	busy, release := make(chan struct{}), make(chan struct{})
	td.tasks.Resume(ctx, task, func(c context.Context) (string, error) { // another leg holds the task
		close(busy)
		select {
		case <-release:
		case <-c.Done():
		}
		return "", c.Err()
	})
	<-busy
	if reply, err := td.DecideApproval(ctx, 1, true); err != nil || reply != "Going ahead with that for Chase refund." {
		t.Fatalf("reply %q err %v", reply, err)
	}
	if err := td.tasks.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the step to be marked not done", func() bool {
		ap, _ := td.store.GetApproval(ctx, 1)
		return ap.Status == "expired"
	})
	if len(td.ran()) != 0 {
		t.Fatalf("the step ran: %v", td.ran())
	}
}

// boardKeeper is a model working a task with a board: it plans three steps,
// asks for the OK to send, and, told a step's outcome, reports it and stops,
// as a model told to "tell the user the outcome" would.
func boardKeeper(last string, _ llm.Request) llm.Response {
	switch {
	case strings.Contains(last, "Background task started"):
		return call("b1", "task_update", `{"add_steps":"[\"ask the shop\",\"chase the bank\",\"confirm\"]"}`)
	case strings.Contains(last, "board updated") && !strings.Contains(last, "☑"):
		return call("s1", "send", `{"to":"shop"}`)
	case strings.Contains(last, "PENDING_APPROVAL"):
		return say("I need your OK to email the shop.")
	case strings.Contains(last, "the user approved"):
		return say("Emailed the shop.")
	case strings.Contains(last, "board still has open steps"):
		return call("f1", "task_update", `{"finish":"Refund on its way."}`)
	case strings.Contains(last, "Task finished"):
		return say("Done.")
	}
	return say("ok")
}

// Approving one step in the middle of a task is not the task done: it
// carries on from its board.
func TestApprovingAMidTaskStepDoesNotEndTheTask(t *testing.T) {
	td := newTestDaemon(t, boardKeeper)
	ctx := context.Background()
	_, status := refundTask(t, td, ownerKey)
	if got := td.ch.next(t); !strings.Contains(got, "I need your OK") {
		t.Fatalf("owner told %q", got)
	}
	if _, err := td.DecideApproval(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the task to finish", func() bool { return status() == tasks.Done })
	if got := td.ch.next(t); got != "Chase refund: done. Refund on its way." {
		t.Fatalf("owner told %q: the task ended on the step's outcome", got)
	}
	if !slices.Equal(td.ran(), []string{"shop"}) {
		t.Fatalf("sent %v", td.ran())
	}
}

// While paused, the lapse sweep (a heartbeat job) doesn't run, so a request
// can outlive its three days unswept (time merged with approvals). A yes to
// it from the screen is still held to the lapse when it is decided: it isn't
// carried out, and it reads expired.
func TestALapsedRequestIsRefusedFromTheScreenWhilePaused(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.owner(t, "email the boss")
	td.SetPaused(true)
	withClock(t, approvalTTL+time.Minute)
	ctx := context.Background()
	reply, err := td.DecideApproval(ctx, 1, true)
	if len(td.ran()) != 0 {
		t.Fatalf("a lapsed request ran from the screen: %q %v", reply, err)
	}
	if ap, _ := td.store.GetApproval(ctx, 1); ap.Status != "expired" {
		t.Fatalf("#1 is %s (%q %v)", ap.Status, reply, err)
	}
}
