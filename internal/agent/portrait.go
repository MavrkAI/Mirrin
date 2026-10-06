package agent

import "strings"

// A portrait ends with a line on what's new since the last one ("NEW:
// you've been guarding Friday afternoons", or "NEW: NONE"). The line is
// for the screen, which shows it under the portrait for a week; it never
// goes into a turn with the portrait.

// splitPortrait separates the model's portrait from its last line on
// what's new. news is empty when the line says NONE or isn't there. The
// line may be bold, or run on at the end of the last sentence.
func splitPortrait(reply string) (text, news string) {
	reply = strings.TrimSpace(reply)
	i := strings.LastIndex(reply, "NEW:")
	if i < 0 || strings.Contains(reply[i:], "\n") || (i > 0 && !strings.ContainsAny(reply[i-1:i], " \t\n*_")) {
		return reply, ""
	}
	text = strings.TrimRight(reply[:i], " \t\n*_")
	news = strings.Trim(reply[i+len("NEW:"):], " \t*_\"“”")
	switch strings.ToLower(strings.TrimRight(news, ".!")) {
	case "", "none", "nothing", "nothing new":
		news = ""
	}
	return text, news
}
