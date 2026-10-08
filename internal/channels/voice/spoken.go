package voice

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A long answer read out is hard to follow, and a draft, a list or a table
// is something to look at, not to hear. So at this computer the twin says
// the gist, says "It's all on your screen", and the presence screen shows
// the whole answer. "Just read it to me" reads that one out and has long
// answers read out from then on; "put long answers on my screen" undoes it.

const (
	// longWords is how many spoken words make an answer long.
	longWords = 60
	// gistWords is about how much is said before the rest is held back, in
	// case the answer goes on the screen.
	gistWords = 35
	// shownFor is how long "just read it to me" still means the answer
	// that went on the screen.
	shownFor = 10 * time.Minute
)

// ScreenHooks are what the host gives the channel to put answers on the
// presence screen.
type ScreenHooks struct {
	// For returns how to put a reply in chatID on the screen, or nil when
	// it can't go there (a call, or long answers are read out).
	For func(chatID string) func(text string) bool
	// ReadAloud has long answers read out (true) or put on the screen
	// (false) from now on.
	ReadAloud func(on bool)
}

// shownText is the last answer put on the screen, for "just read it to me".
type shownText struct {
	mu   sync.Mutex
	text string
	at   time.Time
}

var (
	reListItem  = regexp.MustCompile(`(?m)^[ \t]*(?:[-*•▪◦]|\d{1,2}[.)])[ \t]+\S`)
	reTableRow  = regexp.MustCompile(`(?m)^[^|\n]*\|[^|\n]*\|.*$`)
	reDraftMark = regexp.MustCompile("(?mi)^[ \\t]*(?:subject:|dear[ \\t]+\\S|```)")
	// reStructured is a sentence, as the model wrote it, that starts a
	// list, a table, a draft or a heading (perhaps after a short lead-in
	// merged into it: "Here you go: - milk").
	reStructured = regexp.MustCompile("(?i)^\\s*(?:[-*•▪◦]|\\d{1,2}[.)]|#{1,6}|subject:|dear\\s|```)\\s*\\S|:\\s+(?:[-*•▪◦]|\\d{1,2}[.)])\\s+\\S|\\|[^|]*\\|")
)

// ForScreen reports whether an answer, as the model wrote it, is better
// read than heard: over longWords spoken words, or a list of three or
// more, a table or a draft.
func ForScreen(text string) bool {
	if spokenWords(text) > longWords {
		return true
	}
	if len(reListItem.FindAllString(text, -1)) >= 3 || len(reTableRow.FindAllString(text, -1)) >= 2 {
		return true
	}
	return reDraftMark.MatchString(text)
}

// spokenWords counts the words text has said out loud.
func spokenWords(text string) int { return len(strings.Fields(speakable(text))) }

// structured reports whether a sentence is part of something to look at.
func structured(s string) bool { return reStructured.MatchString(s) }

// holdBack keeps s from being said when the answer may go on the screen:
// once the gist has been said, or something to look at starts, the rest
// waits for Close to decide.
func (v *voiceStream) holdBack(s string) bool {
	if v.scr == nil || v.approvalPrompt != "" {
		return false
	}
	if len(v.heldBack) == 0 && !structured(s) && (v.gist == 0 || v.gist+spokenWords(s) <= gistWords) {
		return false
	}
	v.heldBack = append(v.heldBack, s)
	return true
}

// speak says s now.
func (v *voiceStream) speak(s string) {
	v.q.Push(s)
	v.text.WriteString(strings.TrimSpace(s) + " ")
	v.gist += spokenWords(s)
}

// release says what was held back, in order.
func (v *voiceStream) release() {
	held := v.heldBack
	v.heldBack = nil
	for _, s := range held {
		v.speak(s)
	}
}

// putOnScreen ends an answer that was held back: a long one, or one to
// look at, goes on the screen with "It's all on your screen" (and its
// closing question, still said, so a follow-up is heard as the answer).
// Otherwise, or if the screen can't show it, the rest is said after all.
func (v *voiceStream) putOnScreen() {
	if len(v.heldBack) == 0 {
		return
	}
	full := strings.TrimSpace(v.answer.String())
	if v.approvalPrompt != "" || !ForScreen(full) || !v.scr(full) {
		v.release()
		return
	}
	last := v.heldBack[len(v.heldBack)-1]
	v.heldBack = nil
	v.c.shown.mu.Lock()
	v.c.shown.text, v.c.shown.at = full, time.Now()
	v.c.shown.mu.Unlock()
	v.speak("It's all on your screen.")
	if asksForAnswer(last) && !structured(last) {
		v.speak(last)
	}
}

// takeShown is the answer last put on the screen, if recent, once.
func (c *Channel) takeShown() string {
	c.shown.mu.Lock()
	defer c.shown.mu.Unlock()
	t := c.shown.text
	c.shown.text = ""
	if t == "" || time.Since(c.shown.at) > shownFor {
		return ""
	}
	return t
}

// readPhrases ask for the answer on the screen to be read out instead.
var readPhrases = map[string]bool{
	"just read it to me": true, "just read it": true, "read it to me": true, "read it out": true,
	"read it out to me": true, "just read it out": true, "read it aloud": true, "read it out loud": true,
	"just read it out loud": true, "read it to me instead": true, "can you just read it to me": true,
	"can you read it to me": true, "just read them to me": true, "read them to me": true,
}

// screenPhrases put long answers back on the screen.
var screenPhrases = map[string]bool{
	"put long answers on my screen": true, "put long answers on the screen": true,
	"show long answers on my screen": true, "show long answers on the screen": true,
	"put long answers on screen": true, "show long answers on screen": true,
}

// phrase is text as words, without the twin's name or a trailing please.
func (c *Channel) phrase(text string) string {
	w := words(text)
	for _, n := range c.names() {
		if n = words(n); n != "" {
			w = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(w, n+" "), " "+n))
		}
	}
	return strings.TrimSpace(strings.TrimSuffix(w, " please"))
}

// screenCommand answers "just read it to me" and "put long answers on my
// screen" itself, and reports whether it did. "Just read it to me" is only
// taken while an answer it means is on the screen; otherwise it's the
// twin's to hear.
func (c *Channel) screenCommand(ctx context.Context, text string) bool {
	if c.Screen.ReadAloud == nil {
		return false
	}
	p := c.phrase(text)
	switch {
	case readPhrases[p]:
		shown := c.takeShown()
		if shown == "" {
			return false
		}
		c.Screen.ReadAloud(true)
		c.say(ctx, "Sure. I'll read long answers out from now on; say \"put long answers on my screen\" to change it back.")
		c.say(ctx, shown)
		return true
	case screenPhrases[p]:
		c.Screen.ReadAloud(false)
		c.say(ctx, "Right, long answers go on your screen from now on.")
		return true
	}
	return false
}

// say speaks a line of the channel's own and shows it in the terminal.
func (c *Channel) say(ctx context.Context, text string) {
	fmt.Fprintf(c.out, "%s: %s\n", c.name, text)
	c.lastSaid.Store(text)
	c.Speak(ctx, text)
}
