package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// chatProvider answers by what the last user message says, and can hold the
// approval follow-up until released, so two conversations overlap.
type chatProvider struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (p *chatProvider) Name() string { return "chat" }
func (p *chatProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1].PlainText()
	switch {
	case strings.Contains(last, "the user approved"):
		p.once.Do(func() {
			close(p.entered)
			<-p.release
		})
		r := text("Sent, sir.")
		return &r, nil
	default:
		r := text("Chat B's private reply.")
		return &r, nil
	}
}

func TestStreamedApprovalOnlyStreamsItsOwnConversation(t *testing.T) {
	p := &chatProvider{entered: make(chan struct{}), release: make(chan struct{})}
	a, store, _ := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	a.SetProvider(p)
	ctx := context.Background()
	id, err := store.CreateApproval(ctx, "voice:local", "send", []byte(`{"to":"priya"}`), "send(to=priya)")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var spoken strings.Builder
	done := make(chan error)
	go func() {
		_, err := a.ResolveApprovalStreaming(ctx, "voice:local", id, true, func(s string) {
			mu.Lock()
			spoken.WriteString(s)
			mu.Unlock()
		})
		done <- err
	}()
	<-p.entered
	// Another conversation takes a turn while the approval's follow-up is in flight.
	if _, err := a.Handle(ctx, "whatsapp:bob", "hello"); err != nil {
		t.Fatal(err)
	}
	close(p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := spoken.String()
	mu.Unlock()
	if strings.Contains(got, "Chat B") {
		t.Fatalf("another conversation's reply was streamed into this one: %q", got)
	}
	if !strings.Contains(got, "Sent, sir.") {
		t.Fatalf("the approval's own reply was not streamed: %q", got)
	}
}

func TestOnApprovalHearsEachNewApproval(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{toolUse("t1", "send", `{"to":"priya"}`), text("Shall I? Reply yes 1.")}}
	a, _, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	var heard []ApprovalEvent
	a.OnApproval(func(_ context.Context, e ApprovalEvent) { heard = append(heard, e) })
	if _, err := a.Handle(context.Background(), "test:1", "send it"); err != nil {
		t.Fatal(err)
	}
	if len(heard) != 1 || heard[0].ID != 1 || heard[0].ChatKey != "test:1" || heard[0].Tool != "send" || heard[0].Status != "pending" {
		t.Fatalf("got %+v", heard)
	}
}

func TestCheckApprovedStopsACallThatNoLongerFits(t *testing.T) {
	fp := &fakeProvider{script: []llm.Response{text("The page changed, so I didn't click. Want me to look again?")}}
	a, store, ran := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	ctx := context.Background()
	id, _ := store.CreateApproval(ctx, "test:1", "send", []byte(`{}`), "send()")
	a.CheckApproved = func(_ context.Context, ap memory.Approval) error {
		if ap.ID != id {
			t.Errorf("checked the wrong approval: %+v", ap)
		}
		return errors.New("the page moved on")
	}
	out, err := a.ResolveApproval(ctx, "test:1", id, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 0 {
		t.Fatalf("a stale call ran: %v", *ran)
	}
	last := fp.reqs[0].Messages[len(fp.reqs[0].Messages)-1].PlainText()
	if !strings.Contains(last, "NOT run") || !strings.Contains(last, "the page moved on") {
		t.Fatalf("model not told why: %q", last)
	}
	if !strings.Contains(out, "didn't click") {
		t.Fatalf("got %q", out)
	}
	if ap, _ := store.GetApproval(ctx, id); ap.Status != "expired" {
		t.Fatalf("status %q; nothing ran, so it must not read as approved", ap.Status)
	}
}

func TestOnlyTheFirstOfTwoDecisionsGetsThrough(t *testing.T) {
	a, store, _ := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	ctx := context.Background()
	id, _ := store.CreateApproval(ctx, "test:1", "send", []byte(`{}`), "send()")
	// Many at once, as a double click or the orb and the screen together.
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := a.Decide(ctx, "test:1", id, true); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d decisions got through, want exactly one", won)
	}
	for _, approve := range []bool{true, false} {
		if _, err := a.Decide(ctx, "test:1", id, approve); err == nil || !strings.Contains(err.Error(), "already approved") {
			t.Fatalf("second decision (approve=%v): %v", approve, err)
		}
	}
	if _, err := a.Decide(ctx, "test:2", id, true); err == nil {
		t.Fatal("decided from another conversation")
	}
}
