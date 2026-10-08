package calendar

import (
	"time"

	gcal "google.golang.org/api/calendar/v3"
)

// The watcher's clash check (watch.Spans) needs each event's times, not just
// the line it compares. Snapshot keeps them as it lists the events.

// addSpan records e's start and end. All-day and cancelled events have no
// span: neither runs into a meeting. Nor do ones the owner declined, or
// ones marked free (focus time, a working-location block).
func addSpan(spans map[string][2]time.Time, e *gcal.Event) {
	if e.Status == "cancelled" || e.Transparency == "transparent" || declined(e) ||
		e.Start == nil || e.End == nil || e.Start.DateTime == "" || e.End.DateTime == "" {
		return
	}
	st, err1 := time.Parse(time.RFC3339, e.Start.DateTime)
	en, err2 := time.Parse(time.RFC3339, e.End.DateTime)
	if err1 != nil || err2 != nil {
		return
	}
	spans[e.Id] = [2]time.Time{st, en}
}

// declined reports whether the owner turned e down.
func declined(e *gcal.Event) bool {
	for _, a := range e.Attendees {
		if a != nil && a.Self && a.ResponseStatus == "declined" {
			return true
		}
	}
	return false
}

func (c *Client) keepSpans(spans map[string][2]time.Time) {
	c.mu.Lock()
	c.spans = spans
	c.mu.Unlock()
}

// Span is when the event with id key starts and ends, in the calendar's
// time zone, as of the last Snapshot.
func (c *Client) Span(key string) (start, end time.Time, ok bool) {
	c.mu.Lock()
	sp, ok := c.spans[key]
	c.mu.Unlock()
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	return sp[0].In(c.loc), sp[1].In(c.loc), true
}
