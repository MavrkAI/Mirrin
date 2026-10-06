package voice

import (
	"fmt"
	"strings"
)

// When a part of the voice stops working (the speech engine, the speaker,
// the microphone, whisper), the twin must not just go quiet: the owner is
// told once, in plain words, what happened and what to do. It is told again
// only after that part has worked in between.

// Kinds of trouble, each told once until it recovers.
const (
	troubleVoice   = "voice"   // the chosen speech engine failed; a backup voice is speaking
	troublePlayer  = "player"  // the audio player (sox) failed; the system voice is speaking
	troubleSpeaker = "speaker" // the system voice can't speak either
	troubleMic     = "mic"     // the microphone gives nothing, or its helper died
	troubleEars    = "ears"    // whisper can't transcribe
)

// problem reports trouble of a kind once: on the terminal, and to the host
// (OnProblem: a desktop notification and the presence screen).
func (c *Channel) problem(kind, msg string) {
	c.tmu.Lock()
	if c.told == nil {
		c.told = map[string]bool{}
	}
	already := c.told[kind]
	c.told[kind] = true
	c.tmu.Unlock()
	if already {
		return
	}
	fmt.Fprintf(c.out, "(heads up: %s)\n", msg)
	if c.OnProblem != nil {
		c.OnProblem(msg)
	}
}

// recovered notes that a kind of trouble is over, so a later one is told.
func (c *Channel) recovered(kind string) {
	c.tmu.Lock()
	delete(c.told, kind)
	c.tmu.Unlock()
}

// toldAbout reports whether trouble of a kind has been told and not recovered.
func (c *Channel) toldAbout(kind string) bool {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	return c.told[kind]
}

// brief shortens an error for a spoken or notified sentence: its first line,
// without a trailing full stop.
func brief(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return strings.TrimRight(s, ". ")
}
