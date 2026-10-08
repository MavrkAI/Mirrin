package calendar

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

// Soon carries who a meeting is with, the invite's notes and its video
// link; a meeting already under way, or one past the window, is left out.
func TestSoonCarriesGuestsNotesAndLink(t *testing.T) {
	c, fake := connected(t) // "Dentist" in two hours
	now := time.Now().UTC().Truncate(time.Minute)
	fake.AddEvent(googletest.Event{ID: "e2", Summary: "Q3 catch-up", Start: now.Add(10 * time.Minute).Format(time.RFC3339), End: now.Add(40 * time.Minute).Format(time.RFC3339),
		Description: "Agenda: numbers", HangoutLink: "https://meet.google.com/abc-defg-hij",
		Attendees: []googletest.Attendee{{Email: "me@example.com", Self: true, Response: "accepted"}, {Email: "priya@example.com", Name: "Priya Shah"}, {Email: "room@resource.example.com", Resource: true}}})
	fake.AddEvent(googletest.Event{ID: "e3", Summary: "Standup", Start: now.Add(-5 * time.Minute).Format(time.RFC3339), End: now.Add(10 * time.Minute).Format(time.RFC3339)})
	fake.AddEvent(googletest.Event{ID: "e4", Summary: "Zoom call", Start: now.Add(12 * time.Minute).Format(time.RFC3339), End: now.Add(20 * time.Minute).Format(time.RFC3339), VideoURI: "https://zoom.example/j/1"})

	evs, err := c.Soon(context.Background(), now, now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].ID != "e2" || evs[1].ID != "e4" {
		t.Fatalf("soon: %+v", evs)
	}
	e := evs[0]
	if e.Description != "Agenda: numbers" || e.Link != "https://meet.google.com/abc-defg-hij" {
		t.Fatalf("notes or link: %+v", e)
	}
	if len(e.Attendees) != 3 || !e.Attendees[0].Self || e.Attendees[1].Name != "Priya Shah" || e.Attendees[1].Email != "priya@example.com" || !e.Attendees[2].Resource {
		t.Fatalf("attendees: %+v", e.Attendees)
	}
	if evs[1].Link != "https://zoom.example/j/1" {
		t.Fatalf("another tool's video link: %q", evs[1].Link)
	}
}

// Screens ask Upcoming every few seconds and may hang on a wall: they never
// get the guest list or the invite's notes, and the watcher's snapshot reads
// as it always did, so an upgrade isn't a week of "changed" events.
func TestScreensAndSnapshotLeaveGuestsOut(t *testing.T) {
	c, fake := connected(t)
	start := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Minute)
	fake.AddEvent(googletest.Event{ID: "e2", Summary: "Lunch", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339),
		Description: "private notes", Attendees: []googletest.Attendee{{Email: "priya@example.com", Name: "Priya"}}})
	up, err := c.Upcoming(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range up {
		if len(e.Attendees) != 0 || e.Description != "" || e.ID != "" {
			t.Fatalf("a screen got %+v", e)
		}
	}
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap["e2"]; !strings.HasSuffix(got, "| Lunch") || strings.Contains(got, "Priya") || strings.Contains(got, "private") {
		t.Fatalf("snapshot line: %q", got)
	}
}
