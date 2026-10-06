// Package reminders lets Mirrin nudge the user at a chosen time.
package reminders

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	mem "github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// MaxChecks is how many follow-ups may be open at once.
const MaxChecks = 10

// Tools returns set_reminder / follow_up / list_reminders / cancel_reminder.
func Tools(store *mem.Store, loc *time.Location) []tools.Tool {
	if loc == nil {
		loc = time.Local
	}
	return []tools.Tool{
		tools.New("set_reminder",
			"Schedule a reminder that you will deliver to this chat at the given time.",
			tools.Schema(map[string]tools.Prop{
				"when": {Type: "string", Description: "Absolute time in RFC3339 (2026-09-18T07:30:00+10:00) or local 'YYYY-MM-DD HH:MM', or relative like '45m', '2h', '3d'", Required: true},
				"text": {Type: "string", Description: "What to remind the user about", Required: true},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ When, Text string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				due, err := ParseWhen(in.When, time.Now(), loc)
				if err != nil {
					return "", err
				}
				if due.Before(time.Now()) {
					return "", fmt.Errorf("%s is in the past", due.In(loc).Format(time.RFC1123))
				}
				id, err := store.AddReminder(ctx, call.ChatKey, due, in.Text)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("reminder #%d set for %s", id, due.In(loc).Format("Mon 2 Jan 15:04")), nil
			}),
		tools.New("follow_up",
			"Promise to check back on something (a reply you're waiting for, a refund, how something went). At that time you'll look again with your tools and speak up only if there is news or a decision.",
			tools.Schema(map[string]tools.Prop{
				"when":  {Type: "string", Description: "When to look again: RFC3339 (2026-09-18T10:00:00+10:00), local 'YYYY-MM-DD HH:MM', or relative like '2h', '3d'", Required: true},
				"about": {Type: "string", Description: "What to check on, in a few words (\"Acme's reply about the refund\")", Required: true},
				"notes": {Type: "string", Description: "What you'll need then: who, what was sent and when, order numbers, what counts as news"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ When, About, Notes string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if strings.TrimSpace(in.About) == "" {
					return "", fmt.Errorf("say what to check on")
				}
				if mem.UsageKind(call.ChatKey) == "followup" {
					// A follow-up runs once: looking again is the owner's call.
					return "A follow-up can't set another. Offer to look again, and set one if the owner says yes.", nil
				}
				due, err := ParseWhen(in.When, time.Now(), loc)
				if err != nil {
					return "", err
				}
				if due.Before(time.Now()) {
					return "", fmt.Errorf("%s is in the past", due.In(loc).Format(time.RFC1123))
				}
				id, err := store.AddCheck(ctx, call.ChatKey, due, strings.TrimSpace(in.About), strings.TrimSpace(in.Notes), MaxChecks)
				if errors.Is(err, mem.ErrTooManyChecks) {
					return fmt.Sprintf("There are already %d follow-ups open. Finish or cancel one first.", MaxChecks), nil
				}
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("follow-up #%d set for %s", id, due.In(loc).Format("Mon 2 Jan 15:04")), nil
			}),
		tools.New("list_reminders", "List pending reminders and follow-ups for this chat.", nil, tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				rs, err := store.PendingReminders(ctx, call.ChatKey)
				if err != nil {
					return "", err
				}
				if len(rs) == 0 {
					return "no pending reminders", nil
				}
				var b strings.Builder
				for _, r := range rs {
					what := r.Text
					if r.Kind == mem.KindCheck {
						what = "follow-up: " + what
					}
					fmt.Fprintf(&b, "#%d %s — %s\n", r.ID, r.DueAt.In(loc).Format("Mon 2 Jan 15:04"), what)
				}
				return b.String(), nil
			}),
		tools.New("cancel_reminder", "Cancel a pending reminder or follow-up by id.",
			tools.Schema(map[string]tools.Prop{"id": {Type: "integer", Description: "Reminder id", Required: true}}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ ID int64 }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if err := store.CancelReminder(ctx, in.ID); err != nil {
					return "", err
				}
				return fmt.Sprintf("cancelled #%d", in.ID), nil
			}),
	}
}

// ParseWhen reads when a reminder is due, from now in loc: an absolute
// time (RFC 3339, or local "2006-01-02 15:04"), a span ("45m", "2h",
// "3d"), or the owner's own words for putting one back: "in 20 minutes",
// "in an hour", "tonight" (8 pm, or an hour from now once that has
// passed), "tomorrow" (9 am) or "tomorrow 9" ("tomorrow at 2pm",
// "tomorrow 9:30"; an hour from 1 to 6 with no am or pm is the afternoon).
func ParseWhen(s string, now time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	s = strings.TrimSpace(s)
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(d), nil
	}
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil {
			return now.Add(time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	if t, ok := spoken(strings.ToLower(strings.Join(strings.Fields(s), " ")), now.In(loc)); ok {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("could not parse time %q", s)
}

var (
	reIn       = regexp.MustCompile(`^in (an?|\d+) (minutes?|mins?|hours?|hrs?|days?)$`)
	reTomorrow = regexp.MustCompile(`^tomorrow(?: (?:at )?(\d{1,2})(?:[:.](\d{2}))? ?(am|pm)?)?$`)
)

// spoken reads the few ways people say when: now is in the owner's zone.
func spoken(s string, now time.Time) (time.Time, bool) {
	if m := reIn.FindStringSubmatch(s); m != nil {
		n := 1
		if m[1] != "a" && m[1] != "an" {
			n, _ = strconv.Atoi(m[1])
		}
		unit := time.Minute
		switch m[2][0] {
		case 'h':
			unit = time.Hour
		case 'd':
			unit = 24 * time.Hour
		}
		return now.Add(time.Duration(n) * unit), true
	}
	if s == "tonight" {
		at := time.Date(now.Year(), now.Month(), now.Day(), 20, 0, 0, 0, now.Location())
		if !at.After(now) {
			at = now.Add(time.Hour)
		}
		return at, true
	}
	m := reTomorrow.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	h, mins := 9, 0
	if m[1] != "" {
		h, _ = strconv.Atoi(m[1])
		mins, _ = strconv.Atoi(m[2]) // none is on the hour
		switch m[3] {
		case "am", "pm":
			if h < 1 || h > 12 {
				return time.Time{}, false
			}
			h %= 12
			if m[3] == "pm" {
				h += 12
			}
		default:
			if h >= 1 && h <= 6 { // "tomorrow 3" is the afternoon
				h += 12
			}
		}
		if h > 23 || mins > 59 {
			return time.Time{}, false
		}
	}
	d := now.AddDate(0, 0, 1)
	return time.Date(d.Year(), d.Month(), d.Day(), h, mins, 0, 0, now.Location()), true
}
