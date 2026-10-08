package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

// taskState is where the background tasks stand, in a line for every turn's
// prompt: the open ones and what they wait for, those set aside and why,
// and those dropped or finished lately, so a chat doesn't carry on with a
// booking the owner dropped on the screen (its history still says it's
// under way).
func taskState(ts []tasks.Task, now time.Time) string {
	var open, ended []string
	for _, t := range ts {
		switch {
		case t.Status == tasks.Cancelled && now.Sub(t.Updated) < 48*time.Hour:
			ended = append(ended, fmt.Sprintf("%q was dropped by the owner: it is not happening, don't describe it as pending or under way unless they ask for it again", t.Title))
		case t.Status == tasks.Done && now.Sub(t.Updated) < 48*time.Hour:
			r := strings.TrimSpace(t.Result)
			if len([]rune(r)) > 200 {
				r = string([]rune(r)[:200]) + "…"
			}
			if r == "" {
				r = "no result recorded"
			}
			ended = append(ended, fmt.Sprintf("%q finished: %s%s", t.Title, r, shotsNote(t.Shots)))
		case t.Status == tasks.Failed && now.Sub(t.Updated) < 48*time.Hour:
			ended = append(ended, fmt.Sprintf("%q failed", t.Title))
		case t.Status == tasks.Paused:
			// Still the owner's: "how's Bali going?" gets why it stopped.
			why := strings.TrimSpace(t.PausedFor)
			if why == "" {
				why = "it ran out of steps before finishing"
			}
			if t.PausedBy != tasks.PausedBudget {
				why += "; it carries on only when the owner says to try it again"
			}
			open = append(open, fmt.Sprintf("%q is set aside: %s", t.Title, why))
		case t.Open():
			// When it started and when it asked: a title or question that says
			// "tomorrow" means the day after it was written, and a booking
			// for a date now past is no longer bookable.
			s := fmt.Sprintf("%q (started %s) is %s", t.Title, when(t.Created, now), strings.ReplaceAll(string(t.Status), "_", " "))
			if t.Status == tasks.WaitingUser && t.Question != "" {
				q := []rune(t.Question)
				if len(q) > 160 {
					q = append(q[:160], '…')
				}
				s += ", asking"
				if !t.AskedAt.IsZero() {
					s += " since " + when(t.AskedAt, now)
				}
				s += ": " + string(q)
			}
			open = append(open, s)
		}
	}
	if len(open) == 0 && len(ended) == 0 {
		return ""
	}
	out := "none open."
	if len(open) > 0 {
		out = "open: " + strings.Join(open, "; ") + "."
	}
	if len(ended) > 0 {
		out += " Lately: " + strings.Join(ended, "; ") + "."
	}
	return out
}

// when names a moment for the prompt: "today 14:05", "yesterday 21:30" or
// "Thu 1 Oct 21:30", so relative words in old text can be read right.
func when(t, now time.Time) string {
	if t.IsZero() {
		return "at an unknown time"
	}
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch days := int(time.Date(y2, m2, d2, 0, 0, 0, 0, now.Location()).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, now.Location())).Hours() / 24); days {
	case 0:
		return "today " + t.Format("15:04")
	case 1:
		return "yesterday " + t.Format("15:04")
	}
	return t.Format("Mon 2 Jan 15:04")
}

// firedState says which reminders went off lately and aren't ticked off,
// for the same per-turn line as the tasks.
func firedState(rs []memory.Reminder, now time.Time) string {
	if len(rs) == 0 {
		return ""
	}
	var said []string
	for _, r := range rs {
		said = append(said, fmt.Sprintf("%q went off %s and isn't ticked off yet", r.Text, when(r.FiredAt, now)))
	}
	return " Reminders that already went off (don't describe them as still to come): " + strings.Join(said, "; ") + "."
}
