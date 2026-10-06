package daemon

import (
	"context"
	"regexp"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/channels/voice"
	"github.com/MavrkAI/Mirrin/internal/events"
)

// Messages are written for a chat, where "Shop for men's clothing: …" and
// `Reply "yes 21"` make sense on a screen full of them. Said out loud they
// sound like a form being read. forVoice says the same thing the way a
// person would; what the owner's apps and the screen get is unchanged.

var (
	reReplyYesNo = regexp.MustCompile(`\s*Reply "yes #?(\d+)" or "no #?\d+"\.?`)
	reNeedOK     = regexp.MustCompile(`^I need your OK\.\s+`)
	reHashID     = regexp.MustCompile(`\s*\(?#\d+\)?`)
)

// spokenUpdate rewords text for the voice. title is the task it comes from
// ("" for none) and others how many other tasks are still going, so a task
// is only named when it needs telling apart.
func spokenUpdate(text, title string, others int) string {
	if title != "" && strings.HasPrefix(text, title+": ") {
		text = text[len(title)+2:]
		if others > 0 {
			text = "On " + voice.LowerFirst(title) + ": " + text
		}
	}
	if reReplyYesNo.MatchString(text) {
		text = reReplyYesNo.ReplaceAllString(text, "")
		text = strings.TrimRight(text, " .") + ". Shall I go ahead?"
		if loc := reNeedOK.FindStringIndex(text); loc != nil {
			text = "I need your OK to " + voice.LowerFirst(text[loc[1]:])
		}
	}
	return reHashID.ReplaceAllString(text, "")
}

// forVoice is spokenUpdate for a message about to be spoken, with the task
// it came from (if any) and the others still open.
func (d *Daemon) forVoice(ctx context.Context, text string) string {
	src, _ := events.SourceFrom(ctx)
	others := 0
	if d.tasks != nil && src.Name != "" {
		for _, t := range d.tasks.List() {
			if t.Open() && t.Title != src.Name {
				others++
			}
		}
	}
	return spokenUpdate(text, src.Name, others)
}
