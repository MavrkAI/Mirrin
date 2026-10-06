package watch

import (
	"context"
	"strings"
	"testing"
)

// A source whose items read differently without anything changing (a
// calendar showing its times in a new time zone) starts again from a new
// baseline, and later changes read the new way.
func TestRebaselineTakesANewStartingPoint(t *testing.T) {
	src := &fakeSource{items: map[string]string{"e1": "Mon 14:00 | Dentist"}}
	h := newHarness(t, src)
	ctx := context.Background()
	h.w.Poll(ctx) // baseline
	moved := &fakeSource{items: map[string]string{"e1": "Mon 22:00 | Dentist"}}
	if !h.w.Rebaseline(moved) {
		t.Fatal("rebaseline of a watched source reported false")
	}
	h.w.Poll(ctx)
	if h.taskCount() != 0 {
		t.Fatalf("a new zone's times were reported as changes: %v", h.tasks)
	}
	moved.set("e1", "Mon 23:00 | Dentist")
	h.w.Poll(ctx)
	if h.taskCount() != 1 || !strings.Contains(h.tasks[0], "CHANGED: Mon 22:00 | Dentist → Mon 23:00 | Dentist") {
		t.Fatalf("change after the move not reported in the new zone: %v", h.tasks)
	}
	if h.w.Rebaseline(&fakeSource{name: "inbox", items: map[string]string{}}) || h.w.Has("inbox") {
		t.Fatal("rebaseline added a source that wasn't watched")
	}
}

// blockingSource holds its snapshot until released, to rebaseline while a
// poll of it is under way.
type blockingSource struct {
	fakeSource
	started chan struct{}
	release chan struct{}
}

func (b *blockingSource) Snapshot(ctx context.Context) (map[string]string, error) {
	close(b.started)
	<-b.release
	return b.fakeSource.Snapshot(ctx)
}

func TestRebaselineLetsGoOfAPollUnderWay(t *testing.T) {
	ctx := context.Background()
	old := &blockingSource{fakeSource: fakeSource{items: map[string]string{"e1": "Mon 14:00 | Dentist"}}, started: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t)
	_ = h.store.Set(ctx, "watch:calendar", `{"e1":"Mon 13:00 | Dentist"}`)
	h.w.Add(old, false)
	done := make(chan struct{})
	go func() { h.w.Poll(ctx); close(done) }()
	<-old.started
	moved := &fakeSource{items: map[string]string{"e1": "Mon 22:00 | Dentist"}}
	h.w.Rebaseline(moved)
	close(old.release)
	<-done
	if h.taskCount() != 0 {
		t.Fatalf("the old source's poll was still compared: %v", h.tasks)
	}
	h.w.Poll(ctx) // the new baseline
	h.w.Poll(ctx)
	if h.taskCount() != 0 {
		t.Fatalf("the move was reported as a change: %v", h.tasks)
	}
	if raw, _ := h.store.Get(ctx, "watch:calendar"); !strings.Contains(raw, "Mon 22:00") {
		t.Fatalf("baseline not in the new zone: %s", raw)
	}
}

// The regression: Poll copied the sources, but each source's generation was
// read only when its turn came. A rebaseline while an earlier source (a
// slow inbox) was polled let the old calendar be polled under the new
// generation, saving the old zone's snapshot as the baseline.
func TestRebaselineWhileAnEarlierSourceIsPolled(t *testing.T) {
	ctx := context.Background()
	inbox := &blockingSource{fakeSource: fakeSource{name: "inbox", items: map[string]string{}}, started: make(chan struct{}), release: make(chan struct{})}
	old := &fakeSource{items: map[string]string{"e1": "Mon 14:00 | Dentist"}}
	h := newHarness(t)
	_ = h.store.Set(ctx, "watch:calendar", `{"e1":"Mon 14:00 | Dentist"}`)
	h.w.Add(inbox, false)
	h.w.Add(old, false)
	done := make(chan struct{})
	go func() { h.w.Poll(ctx); close(done) }()
	<-inbox.started
	moved := &fakeSource{items: map[string]string{"e1": "Mon 22:00 | Dentist"}}
	h.w.Rebaseline(moved)
	close(inbox.release)
	<-done
	h.w.Add(&fakeSource{name: "inbox", items: map[string]string{}}, false)
	h.w.Poll(ctx) // the new baseline
	h.w.Poll(ctx)
	if h.taskCount() != 0 {
		t.Fatalf("the move was reported as a change: %v", h.tasks)
	}
	if raw, _ := h.store.Get(ctx, "watch:calendar"); !strings.Contains(raw, "Mon 22:00") {
		t.Fatalf("baseline not in the new zone: %s", raw)
	}
}
