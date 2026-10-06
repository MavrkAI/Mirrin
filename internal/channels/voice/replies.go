package voice

import (
	"context"
	"regexp"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/approvals"
)

// A one-word answer ("Yes.", "No.", "Sure.") looks exactly like whisper's
// guesses at noise, so the follow-up window used to throw it away. It is a
// real answer whenever the twin has just asked something, or an approval
// is waiting on the owner: then it goes through.

// stopPhrases end the speech and the work behind it.
var stopPhrases = map[string]bool{
	"stop": true, "stop it": true, "stop talking": true, "stop there": true, "ok stop": true, "okay stop": true,
	"wait": true, "hold on": true, "hang on": true, "quiet": true, "be quiet": true, "enough": true,
	"thats enough": true, "shush": true, "shh": true, "hush": true, "pause": true, "never mind": true,
	"nevermind": true, "cancel": true, "cancel that": true, "shut up": true,
}

var reNotWord = regexp.MustCompile(`[^\p{L}\p{N}' ]+`)

// words lowercases text and keeps only its words ("Stop!" → "stop").
func words(text string) string {
	t := strings.ToLower(text)
	t = strings.NewReplacer("’", "", "'", "").Replace(t) // "that's" → "thats"
	return strings.Join(strings.Fields(reNotWord.ReplaceAllString(t, " ")), " ")
}

// isStop reports whether text asks the twin to stop and says nothing else,
// allowing its name ("Stop, Mirrin").
func isStop(text string, names ...string) bool {
	w := words(text)
	for _, n := range names {
		if n = words(n); n != "" {
			w = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(w, n+" "), " "+n))
		}
	}
	w = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(w, " please"), " now"))
	return stopPhrases[w]
}

// shortAnswer reports whether text is a whole answer on its own: a yes or a
// no (as the approvals gate reads one: "yes", "sure", "no thanks", "yes 12")
// or a stop.
func shortAnswer(text string, names ...string) bool {
	if _, ok := approvals.ParseReply(text, names...); ok {
		return true
	}
	return isStop(text, names...)
}

// asksForAnswer reports whether the twin's last words wait on an answer: a
// question, or an invitation to say yes or no.
func asksForAnswer(said string) bool {
	s := strings.TrimSpace(said)
	if s == "" {
		return false
	}
	if strings.HasSuffix(strings.TrimRight(s, " \"'”’)"), "?") {
		return true
	}
	l := strings.ToLower(s)
	for _, p := range []string{"say yes", "say no", "yes or no", "just say", "shall i", "should i", "want me to", "do you want"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// expectingAnswer reports whether a short answer now would be meant for the
// twin: its last words asked something, or an approval is waiting.
func (c *Channel) expectingAnswer() bool {
	if last, _ := c.lastSaid.Load().(string); asksForAnswer(last) {
		return true
	}
	return c.Pending != nil && c.Pending()
}

// names are the words that address the twin, allowed around a short answer.
func (c *Channel) names() []string {
	out := []string{c.name, c.spokenName, c.cfg.WakeWord}
	return append(out, c.wakeAliases...)
}

// interruptTurn is called only after speech confirms a real interruption.
// Playback cancellation alone is also used for shutdown, so it stays separate.
func (c *Channel) interruptTurn(text string) {
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	active := c.handling.Load()
	c.stopPlayback()
	if !active && !isStop(text, c.names()...) {
		return
	}
	if c.OnInterrupt != nil {
		if notice := c.OnInterrupt(text); notice != nil {
			if active {
				c.stopNotice = notice
			} else {
				go c.Send(context.Background(), "local", notice())
			}
		}
	} else if active && c.OnStop != nil {
		c.OnStop()
	}
}
