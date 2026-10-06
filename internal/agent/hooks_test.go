package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Every step of an approval's life reaches OnApproval exactly once, in
// order, with the risk it was asked at, from whichever turn made it.
func TestOnApprovalHearsEveryStepOnce(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{
		toolUse("t1", "send", `{"to":"priya"}`), text("Shall I? Reply yes 1."),
		toolUse("t2", "send", `{"to":"bank"}`), text("Shall I? Reply yes 2."),
		text("Sent."),
	}}
	a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	var mu sync.Mutex
	var heard []string
	a.OnApproval(func(_ context.Context, e ApprovalEvent) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, fmt.Sprintf("#%d %s %s by=%q why=%q", e.ID, e.Status, e.Risk, e.By, e.Why))
		if e.InputHash == "" || e.Summary == "" || e.ChatKey != "test:1" {
			t.Errorf("event missing what push needs: %+v", e)
		}
	})
	ctx := context.Background()
	if _, err := a.Handle(ctx, "test:1", "send it to priya"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(ctx, "test:1", "and the bank"); err != nil {
		t.Fatal(err)
	}
	ap1, err := a.DecideBy(ctx, "test:1", 1, true, "Akshay's iPhone (passkey, relay r2, 203.0.113.9)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Carry(ctx, ap1, nil); err != nil {
		t.Fatal(err)
	}
	ap2, _ := store.GetApproval(ctx, 2)
	if err := a.Settle(ctx, ap2, "expired", "no answer in 3 days"); err != nil {
		t.Fatal(err)
	}
	if err := a.Settle(ctx, ap2, "superseded", "again"); err == nil {
		t.Fatal("settled twice")
	}
	want := []string{
		`#1 pending write by="" why=""`,
		`#2 pending write by="" why=""`,
		`#1 approved write by="Akshay's iPhone (passkey, relay r2, 203.0.113.9)" why=""`,
		`#2 expired write by="" why="no answer in 3 days"`,
	}
	if strings.Join(heard, "\n") != strings.Join(want, "\n") {
		t.Fatalf("heard:\n%s\nwant:\n%s", strings.Join(heard, "\n"), strings.Join(want, "\n"))
	}
	es, _ := store.RecentAuditOfKind(ctx, "approval.granted", 1)
	if len(es) != 1 || es[0].Detail != "#1 by Akshay's iPhone (passkey, relay r2, 203.0.113.9): send" {
		t.Fatalf("audit %+v", es)
	}
	if got, _ := store.GetApproval(ctx, 1); got.DecidedBy != "Akshay's iPhone (passkey, relay r2, 203.0.113.9)" {
		t.Fatalf("decider not kept: %+v", got)
	}
}

// riskyTool is a tool whose risk depends on the call.
type riskyTool struct{ *tools.Func }

func (r riskyTool) RiskFor(_ context.Context, c tools.Call) tools.Risk {
	if strings.Contains(string(c.Input), "Pay") {
		return tools.RiskDangerous
	}
	return tools.RiskWrite
}

func TestApprovalKeepsTheRiskItWasAskedAt(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{
		toolUse("t1", "click", `{"label":"Next"}`), text("ok"),
		toolUse("t2", "click", `{"label":"Pay now"}`), text("ok"),
	}}
	a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	a.Tools().Register(riskyTool{tools.New("click", "click", nil, tools.RiskWrite, func(context.Context, tools.Call) (string, error) { return "clicked", nil })})
	ctx := context.Background()
	_, _ = a.Handle(ctx, "test:1", "next")
	_, _ = a.Handle(ctx, "test:1", "pay")
	ps, _ := store.PendingApprovals(ctx, "test:1")
	if len(ps) != 2 || ps[0].Risk != tools.RiskWrite || ps[1].Risk != tools.RiskDangerous {
		t.Fatalf("pending %+v", ps)
	}
}

// Forgetting a fact rewrites any request that quoted it. The owner said yes
// to the request as it was, so the rewritten one is not run.
func TestARequestChangedSinceItWasAskedIsNotRun(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{text("It changed, so I didn't send it.")}}
	a, store, ran := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	ctx := context.Background()
	secret := "Tony's locker code is 4471-alpha-bravo."
	fact, _ := store.Remember(ctx, "home", secret, "test")
	input, _ := json.Marshal(map[string]string{"to": "gym", "text": secret})
	id, _ := store.CreateApproval(ctx, "test:1", "send", input, "send(text="+secret+", to=gym)", tools.RiskWrite)
	if _, err := store.ForgetFact(ctx, fact); err != nil {
		t.Fatal(err)
	}
	if ap, _ := store.GetApproval(ctx, id); ap.Intact() {
		t.Skip("forget no longer rewrites approvals; nothing to check")
	}
	if _, err := a.ResolveApproval(ctx, "test:1", id, true); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 0 {
		t.Fatalf("a request changed after it was asked ran: %v", *ran)
	}
	if ap, _ := store.GetApproval(ctx, id); ap.Status != "expired" {
		t.Fatalf("status %q", ap.Status)
	}
	last := fp.reqs[0].Messages[len(fp.reqs[0].Messages)-1].PlainText()
	if !strings.Contains(last, "NOT run") || !strings.Contains(last, "changed after it was asked") {
		t.Fatalf("model told %q", last)
	}
}

// A task's step is one step: after it is decided the task carries on from
// its board rather than wrapping up.
func TestADecidedTaskStepCarriesOnWithTheTask(t *testing.T) {
	for _, approve := range []bool{true, false} {
		fp := &fakeProvider{script: []llm.Response{text("ok")}}
		a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
		ctx := context.Background()
		key := "telegram:1#task-09271"
		id, _ := store.CreateApproval(ctx, key, "send", []byte(`{"to":"shop"}`), "send(to=shop)", tools.RiskWrite)
		if _, err := a.ResolveApproval(ctx, key, id, approve); err != nil {
			t.Fatal(err)
		}
		last := fp.reqs[0].Messages[len(fp.reqs[0].Messages)-1].PlainText()
		if !strings.Contains(last, "carry on with the task from your board") && !strings.Contains(last, "Carry on with the task from your board") {
			t.Fatalf("approve=%v: the task was not told to carry on: %q", approve, last)
		}
		if strings.Contains(last, "Tell the user the outcome") {
			t.Fatalf("approve=%v: a task step was told to report back and stop: %q", approve, last)
		}
	}
	// Outside a task the outcome is simply reported.
	fp := &fakeProvider{script: []llm.Response{text("Sent.")}}
	a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	id, _ := store.CreateApproval(context.Background(), "telegram:1", "send", []byte(`{}`), "send()")
	_, _ = a.ResolveApproval(context.Background(), "telegram:1", id, true)
	if last := fp.reqs[0].Messages[len(fp.reqs[0].Messages)-1].PlainText(); !strings.Contains(last, "Tell the user the outcome") {
		t.Fatalf("got %q", last)
	}
}

func TestPerformRunsTheCallInTheTurnThatDecidedIt(t *testing.T) {
	a, store, ran := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	ctx := context.Background()
	id, _ := store.CreateApproval(ctx, "test:1", "send", []byte(`{"to":"x"}`), "send(to=x)", tools.RiskWrite)
	ap, err := a.DecideBy(ctx, "test:1", id, true, "the owner (reply, telegram)")
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Perform(ctx, ap); !strings.Contains(got, "executed and succeeded") || len(*ran) != 1 {
		t.Fatalf("got %q, ran %v", got, *ran)
	}
	id2, _ := store.CreateApproval(ctx, "test:1", "send", []byte(`{"to":"y"}`), "send(to=y)", tools.RiskWrite)
	ap2, _ := a.DecideBy(ctx, "test:1", id2, false, "")
	if got := a.Perform(ctx, ap2); !strings.Contains(got, "turned down") || len(*ran) != 1 {
		t.Fatalf("got %q, ran %v", got, *ran)
	}
}

// The retry queue: a push that fails is tried again after a growing pause,
// one that can never work is dropped, and a newer event for the same
// approval replaces an older one still waiting.
func TestRetryQueueDeliversAndGivesUp(t *testing.T) {
	var mu sync.Mutex
	var got []string
	fails := map[string]int{"#1 pending": 2}
	delivered := make(chan string, 16)
	q := NewRetryQueue(func(_ context.Context, e ApprovalEvent) error {
		k := fmt.Sprintf("#%d %s", e.ID, e.Status)
		mu.Lock()
		defer mu.Unlock()
		got = append(got, k)
		if e.ID == 3 {
			return Permanent(errors.New("subscription gone"))
		}
		if fails[k] > 0 {
			fails[k]--
			return errors.New("offline")
		}
		delivered <- k
		return nil
	}, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	q.Add(ApprovalEvent{ID: 1, Status: "pending"})
	q.Add(ApprovalEvent{ID: 3, Status: "pending"})
	for _, want := range []string{"#1 pending"} {
		select {
		case k := <-delivered:
			if k != want {
				t.Fatalf("delivered %q, want %q", k, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("never delivered %q; tried %v", want, got)
		}
	}
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	tries := strings.Join(got, ",")
	mu.Unlock()
	if strings.Count(tries, "#1 pending") != 3 || strings.Count(tries, "#3 pending") != 1 {
		t.Fatalf("tries %s: want #1 three times (two failures), #3 once (permanent)", tries)
	}
	if q.Len() != 0 {
		t.Fatalf("%d left in the queue", q.Len())
	}
}

func TestRetryQueueKeepsOnlyTheNewestEventPerApproval(t *testing.T) {
	block := make(chan struct{})
	var mu sync.Mutex
	var got []string
	q := NewRetryQueue(func(_ context.Context, e ApprovalEvent) error {
		<-block
		mu.Lock()
		got = append(got, fmt.Sprintf("#%d %s", e.ID, e.Status))
		mu.Unlock()
		return nil
	}, nil, 2, nil)
	q.Add(ApprovalEvent{ID: 1, Status: "pending"})
	q.Add(ApprovalEvent{ID: 1, Status: "approved"}) // before anything was sent
	q.Add(ApprovalEvent{ID: 2, Status: "pending"})
	q.Add(ApprovalEvent{ID: 3, Status: "pending"}) // over the limit: #1 goes
	if q.Len() != 2 {
		t.Fatalf("len %d", q.Len())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for q.Len() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "#2 pending,#3 pending" {
		t.Fatalf("delivered %v", got)
	}
}

// hooks run on the live agent, whichever turn's copy raised the approval.
func TestOnApprovalAddedMidTurnIsHeard(t *testing.T) {
	a, _, _ := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	turn := a.turn("hi")
	var n int
	a.OnApproval(func(context.Context, ApprovalEvent) { n++ })
	turn.emit(context.Background(), memory.Approval{ID: 9, Input: []byte(`{}`)}, "pending", "", "")
	if n != 1 {
		t.Fatalf("heard %d times", n)
	}
}
