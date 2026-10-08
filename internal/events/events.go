// Package events is the daemon's live feed: what Mirrin is doing right now,
// for screens that show presence rather than transcripts.
package events

import (
	"context"
	"sync"
	"time"
)

// Event is one thing that happened. An "approval" event marks one raised
// (the moment it needs the owner) or decided; its Data carries the id and status.
// A "react" event has the character react to something that really
// happened: Text is pleased, sorry or attentive, and Data a Reaction.
type Event struct {
	Kind string    `json:"kind"` // state | heard | note | said | approval | reminder | notice | react
	Text string    `json:"text,omitempty"`
	Data any       `json:"data,omitempty"`
	At   time.Time `json:"at"`
}

// Reaction is the data of a "react" event: why the character reacts, in a
// few words of the twin's own (a task finished), never the owner's.
type Reaction struct {
	Why string `json:"why"`
}

// Source is where a message the twin sends on its own comes from: a
// routine's result (Kind "protocol", Name the routine's, Briefing when it is
// tagged briefing), a "reminder", a first-week "tip", an "idea" it noticed.
// It travels in the context from where the message starts to where it is
// delivered, which decides how loudly it arrives.
type Source struct {
	Kind, Name string
	Briefing   bool
	Quiet      bool // no desktop notification: only tips and ideas held for later
}

type sourceKey struct{}

// WithSource marks what ctx delivers as coming from s.
func WithSource(ctx context.Context, s Source) context.Context {
	return context.WithValue(ctx, sourceKey{}, s)
}

// SourceFrom is the source ctx carries, if any. System notices carry none.
func SourceFrom(ctx context.Context) (Source, bool) {
	s, ok := ctx.Value(sourceKey{}).(Source)
	return s, ok
}

// maxHold is how long something may show as thinking before the screens
// stop believing it, so a turn that never ended can't leave the character
// thinking for good. A bus takes it when it is built.
var maxHold = 15 * time.Minute

// hold is one piece of work's part in what the screens show.
type hold struct {
	state string
	since time.Time // when it moved to state
	timer *time.Timer
}

// Bus fans events out to subscribers and works out the shown state from
// everything under way, so a chat that finishes elsewhere can't put the
// character back to idle while she is still speaking.
type Bus struct {
	mu      sync.RWMutex
	subs    map[chan Event]struct{}
	holds   map[uint64]*hold // what is under way, by Presence
	nextID  uint64
	legacy  uint64 // the holder behind Publish(Event{Kind: "state"}); 0 for none
	shown   string // the state last published
	maxHold time.Duration
	last    []Event
	screens int // presence screens open on this computer (screens.go)
}

// New builds a bus.
func New() *Bus {
	return &Bus{subs: map[chan Event]struct{}{}, holds: map[uint64]*hold{}, shown: "idle", maxHold: maxHold}
}

// Presence is one piece of work the screens show until it ends: a turn
// thinking, or the microphone listening and speaking.
type Presence struct {
	b  *Bus
	id uint64
}

// Begin starts a piece of work in state (thinking, listening or speaking;
// idle shows nothing) and holds it until End.
func (b *Bus) Begin(state string) *Presence {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	b.holdLocked(b.nextID, state)
	return &Presence{b: b, id: b.nextID}
}

// Set moves the work to another state. After End it does nothing.
func (p *Presence) Set(state string) {
	if p == nil {
		return
	}
	p.b.mu.Lock()
	defer p.b.mu.Unlock()
	if _, ok := p.b.holds[p.id]; ok {
		p.b.holdLocked(p.id, state)
	}
}

// End says the work is over. Ending it again does nothing.
func (p *Presence) End() {
	if p == nil {
		return
	}
	p.b.mu.Lock()
	defer p.b.mu.Unlock()
	p.b.dropLocked(p.id)
}

// Publish sends an event to every subscriber (non-blocking; slow ones drop).
// A "state" event from code that holds no Presence moves one shared holder,
// which "idle" ends, and screens hear only the state that results.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ev.Kind != "state" {
		b.emitLocked(ev)
		return
	}
	switch {
	case ev.Text == "idle" || ev.Text == "":
		b.dropLocked(b.legacy)
		b.legacy = 0
	case b.legacy == 0:
		b.nextID++
		b.legacy = b.nextID
		b.holdLocked(b.legacy, ev.Text)
	default:
		b.holdLocked(b.legacy, ev.Text)
	}
}

// State returns the shown presence state: the busiest of everything under
// way (speaking, then listening, then thinking), or idle.
func (b *Bus) State() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.computeLocked(time.Now())
}

// Recent returns the last events, oldest first.
func (b *Bus) Recent() []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Event(nil), b.last...)
}

// Subscribe returns a channel of events and a function to stop.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// rank orders the states: the highest any holder is in is the one shown.
func rank(state string) int {
	switch state {
	case "speaking":
		return 3
	case "listening":
		return 2
	case "thinking":
		return 1
	}
	return 0
}

// holdLocked puts holder id in state. A new state starts its clock, and
// thinking sets a timer so the screens hear when it has gone stale.
func (b *Bus) holdLocked(id uint64, state string) {
	h := b.holds[id]
	if h == nil || h.state != state {
		if h != nil && h.timer != nil {
			h.timer.Stop()
		}
		h = &hold{state: state, since: time.Now()}
		if state == "thinking" {
			h.timer = time.AfterFunc(b.maxHold, b.recheck)
		}
		b.holds[id] = h
	}
	b.showLocked()
}

// dropLocked ends holder id, if it is still there.
func (b *Bus) dropLocked(id uint64) {
	if h := b.holds[id]; h != nil {
		if h.timer != nil {
			h.timer.Stop()
		}
		delete(b.holds, id)
	}
	b.showLocked()
}

// recheck tells the screens when a stale thinking holder stops counting.
func (b *Bus) recheck() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.showLocked()
}

// computeLocked works out the state to show at now. A holder that has been
// thinking for maxHold no longer counts; moving it to another state brings
// it back.
func (b *Bus) computeLocked(now time.Time) string {
	shown := "idle"
	for _, h := range b.holds {
		if h.state == "thinking" && now.Sub(h.since) >= b.maxHold {
			continue
		}
		if rank(h.state) > rank(shown) {
			shown = h.state
		}
	}
	return shown
}

// showLocked publishes the shown state when it has changed.
func (b *Bus) showLocked() {
	if s := b.computeLocked(time.Now()); s != b.shown {
		b.shown = s
		b.emitLocked(Event{Kind: "state", Text: s})
	}
}

// emitLocked records ev and hands it to every subscriber. It runs under the
// lock, so every subscriber hears the states in the order they were shown.
func (b *Bus) emitLocked(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	b.last = append(b.last, ev)
	if len(b.last) > 50 {
		b.last = b.last[len(b.last)-50:]
	}
	b.sendLocked(ev)
}

// Flash hands ev to the screens open now without keeping it among the
// Recent events: news that counts only as it happens, such as a fact the
// twin just kept, which a screen opened later mustn't find, nor anyone once
// the fact is forgotten.
func (b *Bus) Flash(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	b.sendLocked(ev)
}

// sendLocked hands ev to every subscriber (non-blocking; slow ones drop).
func (b *Bus) sendLocked(ev Event) {
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
