package voice

import (
	"strings"
	"testing"
	"time"
)

// A click on the orb opens a listening window without the wake word, says
// nothing, and hands on what the owner then says.
func TestListenNowOpensAListeningWindow(t *testing.T) {
	r := newRig(t, nil)
	if r.c.ListenNow() {
		t.Fatal("ListenNow before the wake loop runs should say voice isn't listening")
	}
	r.run()
	r.eventually("the wake loop to be live", func() bool { return r.c.wakeLive.Load() })
	r.say("u1", "what's on this afternoon")
	if !r.c.ListenNow() {
		t.Fatal("ListenNow refused while the wake loop runs")
	}
	r.eventually("a listening window", func() bool { return r.listens() == 1 })
	r.event("utterance u1 (followup 0.6s)")
	if got := r.turn(); !strings.Contains(got, "afternoon") {
		t.Fatalf("handed on %q", got)
	}
	if s := r.spoken(); strings.Contains(s, "Sir?") {
		t.Fatalf("a click shouldn't be answered aloud, said %q", s)
	}
}

// A click that hears nothing just closes the window.
func TestListenNowHearingNothingHandsNothingOn(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	r.eventually("the wake loop to be live", func() bool { return r.c.wakeLive.Load() })
	r.c.ListenNow()
	r.eventually("a listening window", func() bool { return r.listens() == 1 })
	r.event("silence - (followup)")
	r.noTurn(300 * time.Millisecond)
}

// Answering a question by repeating one of its options is the owner
// talking, not the twin's own voice heard back: "Which one — the
// comfortable option …?" "Comfortable option." used to be thrown away as
// an echo, and the twin stopped listening.
func TestAnAnswerThatRepeatsTheQuestionIsHeard(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	r.eventually("the wake loop to be live", func() bool { return r.c.wakeLive.Load() })
	r.c.lastSaid.Store("Right. Which one — the comfortable option at twenty-two to twenty-six hundred, or shall I look for something between mid-range and that?")
	r.say("u1", "Comfortable option.")
	r.c.ListenNow() // a follow-up window, opened after the reply finished
	r.eventually("a listening window", func() bool { return r.listens() == 1 })
	r.event("utterance u1 (followup 0.6s)")
	if got := r.turn(); !strings.Contains(strings.ToLower(got), "comfortable") {
		t.Fatalf("handed on %q", got)
	}
}
