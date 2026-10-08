package calendar

import (
	"context"
	"strings"
	"time"

	gcal "google.golang.org/api/calendar/v3"
)

// Attendee is one guest on an event, as Google lists them.
type Attendee struct {
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
	// Self is the owner's own place on the guest list.
	Self bool `json:"self,omitempty"`
	// Resource is a room or a piece of equipment, not a person.
	Resource bool `json:"resource,omitempty"`
	// Response is needsAction, declined, tentative or accepted.
	Response string `json:"response,omitempty"`
}

// descriptionMax is as much of an invite's notes as Soon keeps.
const descriptionMax = 600

// Soon lists the timed and all-day events that start in [from, to), with
// who is coming, the invite's notes and the video link: what a brief
// before a meeting needs. Unlike Upcoming it is never cached.
func (c *Client) Soon(ctx context.Context, from, to time.Time) ([]Event, error) {
	srv, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	calID := c.cfg.CalendarID
	if calID == "" {
		calID = "primary"
	}
	evs, err := srv.Events.List(calID).ShowDeleted(false).SingleEvents(true).
		TimeMin(from.Format(time.RFC3339)).TimeMax(to.Format(time.RFC3339)).
		OrderBy("startTime").MaxResults(20).Context(ctx).Do()
	if err != nil {
		return nil, c.auth.Explain("calendar", err)
	}
	out := []Event{}
	for _, e := range evs.Items {
		if e.Status == "cancelled" {
			continue
		}
		ev := c.full(e)
		// Google lists what is under way too; only what starts in the window counts.
		if ev.Start.Before(from) || !ev.Start.Before(to) {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// full is an event with everything Google gives about it.
func (c *Client) full(e *gcal.Event) Event {
	ev := Event{ID: e.Id, Title: e.Summary, Location: e.Location, Link: videoLink(e)}
	if e.Start != nil && e.Start.Date != "" {
		ev.AllDay = true
		ev.Start, _ = time.ParseInLocation("2006-01-02", e.Start.Date, c.loc)
		ev.End = ev.Start.Add(24 * time.Hour)
	} else if e.Start != nil {
		ev.Start, _ = time.Parse(time.RFC3339, e.Start.DateTime)
		if e.End != nil {
			ev.End, _ = time.Parse(time.RFC3339, e.End.DateTime)
		}
	}
	if d := strings.TrimSpace(e.Description); d != "" {
		if r := []rune(d); len(r) > descriptionMax {
			d = string(r[:descriptionMax]) + "…"
		}
		ev.Description = d
	}
	for _, a := range e.Attendees {
		if a == nil {
			continue
		}
		ev.Attendees = append(ev.Attendees, Attendee{
			Email: strings.TrimSpace(a.Email), Name: strings.TrimSpace(a.DisplayName),
			Self: a.Self, Resource: a.Resource, Response: a.ResponseStatus,
		})
	}
	return ev
}

// videoLink is the event's video call: Meet's own link, else the first
// video entry point another conference tool added.
func videoLink(e *gcal.Event) string {
	if e.HangoutLink != "" {
		return e.HangoutLink
	}
	if e.ConferenceData != nil {
		for _, p := range e.ConferenceData.EntryPoints {
			if p != nil && p.EntryPointType == "video" && p.Uri != "" {
				return p.Uri
			}
		}
	}
	return ""
}
