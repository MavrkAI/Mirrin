package health

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunRepairsAndReports(t *testing.T) {
	broken := true
	repairs := 0
	m := New(
		Func("a", "A", func(context.Context) (State, string, string) {
			if broken {
				return Fail, "down", "restart"
			}
			return OK, "up", ""
		}, func(context.Context) error { repairs++; broken = false; return nil }),
		Func("b", "B", func(context.Context) (State, string, string) { return Warn, "meh", "" }, nil),
		Func("c", "C", func(context.Context) (State, string, string) { return Fail, "no", "" }, func(context.Context) error { return errors.New("can't") }),
	)
	changes := 0
	m.OnChange = func(prev, cur Report) { changes++ }
	rep := m.Run(context.Background())
	if repairs != 1 || rep.Results[0].State != OK || !rep.Results[0].Fixed {
		t.Fatalf("repair not applied: %+v", rep.Results[0])
	}
	if rep.Summary() != "1 problem(s)" || len(rep.Problems()) != 2 {
		t.Fatalf("summary %q problems %d", rep.Summary(), len(rep.Problems()))
	}
	if changes != 1 {
		t.Fatalf("expected one change notification, got %d", changes)
	}
	m.Run(context.Background())
	if changes != 1 {
		t.Fatal("unchanged report should not notify")
	}
}

func TestRoundsDontOverlap(t *testing.T) {
	// A slow round that started first must not overwrite a newer one.
	slow := make(chan struct{})
	var calls atomic.Int32
	m := New(Func("api", "API", func(context.Context) (State, string, string) {
		if calls.Add(1) == 1 {
			<-slow // the first round read the old state and is still going
			return OK, "stale", ""
		}
		return Fail, "fresh", ""
	}, nil))
	first := make(chan Report)
	go func() { first <- m.Run(context.Background()) }()
	for calls.Load() == 0 {
		runtime.Gosched()
	}
	second := make(chan Report)
	go func() { second <- m.Run(context.Background()) }()
	// Let the second round finish if it can run alongside the first.
	for deadline := time.Now().Add(200 * time.Millisecond); calls.Load() < 2 && time.Now().Before(deadline); {
		runtime.Gosched()
	}
	close(slow)
	<-first
	<-second
	if got := m.Last().Results[0].Detail; got != "fresh" {
		t.Fatalf("kept %q from the round that started first", got)
	}
}
