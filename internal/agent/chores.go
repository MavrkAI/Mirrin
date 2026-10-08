package agent

import "regexp"

// A web chore is an ask to do something on a website and stop at the last
// button ("fill in the form and stop before sending", "find the cheapest
// train and stop before booking"): the screen's "Try this" chip, or the
// owner's own words. It steps through a site (open, type, pick, check) far
// past a question's three calls, so it gets a task's room, like a named
// protocol; the gate still asks before anything is sent, booked or paid.
var reChore = regexp.MustCompile(`(?i)\bstop\s+(just\s+)?(before|short\s+of|at)\s+(you\s+|it\s+)?(the\s+)?(send|sending|submit|submitting|book|booking|pay|paying|buy|buying|order|ordering|checkout|check\s*out|(last\s+|final\s+)?button)\b`)

// isChore says whether text asks for a web chore that stops at the button.
func isChore(text string) bool { return reChore.MatchString(text) }

// choreBudget is a web chore's room: a protocol's (zero, unlimited, too).
func choreBudget(b Budgets) int { return b.Protocol }
