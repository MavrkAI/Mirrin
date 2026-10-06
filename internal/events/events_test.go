package events

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// shown collects the states a bus publishes, in order.
func shown(t *testing.T, b *Bus) func() []string {
	t.Helper()
	ch, stop := b.Subscribe()
	t.Cleanup(stop)
	var got []string
	return func() []string {
		for {
			select {
			case ev := <-ch:
				if ev.Kind == "state" {
					got = append(got, ev.Text)
				}
			default:
				return got
			}
		}
	}
}

// A Telegram turn that ends while the voice is speaking leaves her speaking.
func TestOverlappingHoldersShowTheBusiest(t *testing.T) {
	b := New()
	seen := shown(t, b)
	voice := b.Begin("speaking")
	turn := b.Begin("thinking")
	turn.End()
	if got := b.State(); got != "speaking" {
		t.Fatalf("state %q after the turn ended under speech, want speaking", got)
	}
	voice.End()
	if got := b.State(); got != "idle" {
		t.Fatalf("state %q with nothing under way", got)
	}
	if got := seen(); !slices.Equal(got, []string{"speaking", "idle"}) {
		t.Fatalf("published %q", got)
	}
}

func TestEndInAnyOrderReturnsToIdle(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}, {1, 2, 0}} {
		b := New()
		ps := []*Presence{b.Begin("thinking"), b.Begin("listening"), b.Begin("speaking")}
		for _, i := range order {
			ps[i].End()
			ps[i].End() // ending twice is harmless
		}
		if got := b.State(); got != "idle" {
			t.Fatalf("order %v: state %q", order, got)
		}
		ps[1].Set("speaking") // and an ended one can't come back
		if got := b.State(); got != "idle" {
			t.Fatalf("order %v: Set after End showed %q", order, got)
		}
	}
}

func TestShownStateFollowsTheBusiestHolder(t *testing.T) {
	b := New()
	seen := shown(t, b)
	voice := b.Begin("idle")
	turn := b.Begin("thinking")
	voice.Set("listening")
	voice.Set("listening") // no change, nothing published
	voice.Set("speaking")
	voice.Set("idle")
	turn.End()
	want := []string{"thinking", "listening", "speaking", "thinking", "idle"}
	if got := seen(); !slices.Equal(got, want) {
		t.Fatalf("published %q, want %q", got, want)
	}
}

func TestStaleThinkingExpires(t *testing.T) {
	prev := maxHold
	maxHold = 50 * time.Millisecond
	b := New()
	maxHold = prev
	ch, stop := b.Subscribe()
	defer stop()
	hung := b.Begin("thinking") // a turn that never ends
	voice := b.Begin("idle")
	if got := b.State(); got != "thinking" {
		t.Fatalf("state %q", got)
	}
	var got []string
	deadline := time.After(5 * time.Second)
	for !slices.Equal(got, []string{"thinking", "idle"}) {
		select {
		case ev := <-ch:
			got = append(got, ev.Text)
		case <-deadline:
			t.Fatalf("a stale thinking holder still shows; published %q", got)
		}
	}
	if s := b.State(); s != "idle" {
		t.Fatalf("state %q after maxHold", s)
	}
	// Moving on brings a long-lived holder back, as the microphone's is.
	voice.Set("thinking")
	voice.Set("speaking")
	if s := b.State(); s != "speaking" {
		t.Fatalf("state %q", s)
	}
	hung.End()
	voice.End()
	if s := b.State(); s != "idle" {
		t.Fatalf("state %q", s)
	}
}

func TestOneStateEventPerChange(t *testing.T) {
	b := New()
	seen := shown(t, b)
	a := b.Begin("thinking")
	c := b.Begin("thinking") // still thinking: nothing new to show
	a.End()                  // c still thinks
	c.End()
	b.Publish(Event{Kind: "note", Text: "Looking it up"}) // not a state
	if got := seen(); !slices.Equal(got, []string{"thinking", "idle"}) {
		t.Fatalf("published %q", got)
	}
}

// Callers not yet moved to Begin still publish state events: they set the
// state and "idle" clears it, without clearing anyone else's.
func TestLegacyPublishSetsAndClears(t *testing.T) {
	b := New()
	seen := shown(t, b)
	b.Publish(Event{Kind: "state", Text: "thinking"})
	if got := b.State(); got != "thinking" {
		t.Fatalf("state %q", got)
	}
	b.Publish(Event{Kind: "state", Text: "speaking"})
	b.Publish(Event{Kind: "state", Text: "idle"})
	if got := b.State(); got != "idle" {
		t.Fatalf("state %q", got)
	}
	voice := b.Begin("speaking")
	b.Publish(Event{Kind: "state", Text: "thinking"})
	b.Publish(Event{Kind: "state", Text: "idle"})
	if got := b.State(); got != "speaking" {
		t.Fatalf("a legacy idle cleared someone else's speaking: %q", got)
	}
	voice.End()
	want := []string{"thinking", "speaking", "idle", "speaking", "idle"}
	if got := seen(); !slices.Equal(got, want) {
		t.Fatalf("published %q, want %q", got, want)
	}
	for _, ev := range b.Recent() {
		if ev.At.IsZero() {
			t.Fatalf("an event without a time: %+v", ev)
		}
	}
}

// Every subscriber hears the states in the order they were shown, however
// many turns start and end at once.
func TestConcurrentHoldersSettleOnTheTruth(t *testing.T) {
	b := New()
	ch, stop := b.Subscribe()
	defer stop()
	voice := b.Begin("speaking")
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := b.Begin("thinking")
			b.Publish(Event{Kind: "note", Text: "busy"})
			p.End()
		}()
	}
	wg.Wait()
	if got := b.State(); got != "speaking" {
		t.Fatalf("state %q", got)
	}
	voice.End()
	last := ""
	for {
		select {
		case ev := <-ch:
			if ev.Kind == "state" {
				last = ev.Text
			}
			continue
		default:
		}
		break
	}
	if last != "idle" {
		t.Fatalf("the screens last heard %q", last)
	}
}

// A flash reaches the screens open now and isn't kept for later ones.
func TestFlashIsNotKept(t *testing.T) {
	b := New()
	ch, stop := b.Subscribe()
	defer stop()
	b.Publish(Event{Kind: "said", Text: "Good to know."})
	b.Flash(Event{Kind: "remembered", Text: "Akshay doesn't eat meat."})
	var got []string
	for len(got) < 2 {
		select {
		case ev := <-ch:
			if ev.At.IsZero() {
				t.Fatalf("%s has no time", ev.Kind)
			}
			got = append(got, ev.Kind)
		case <-time.After(time.Second):
			t.Fatalf("heard only %v", got)
		}
	}
	if !slices.Equal(got, []string{"said", "remembered"}) {
		t.Fatalf("heard %v", got)
	}
	if r := b.Recent(); len(r) != 1 || r[0].Kind != "said" {
		t.Fatalf("kept %+v", r)
	}
}
