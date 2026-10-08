package calendar

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	gauth "github.com/MavrkAI/Mirrin/internal/skills/google"
	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func connected(t *testing.T) (*Client, *googletest.Fake) {
	t.Helper()
	fake := googletest.New(t)
	dir := t.TempDir()
	cfg := config.Calendar{Enabled: true, CredentialsFile: filepath.Join(dir, "c.json"), TokenFile: filepath.Join(dir, "t.json"), CalendarID: "primary"}
	gauth.NewAuth(cfg).Transport = fake.Transport()
	// An access token that expired an hour ago, as after any night's sleep.
	fake.WriteFiles(t, cfg.CredentialsFile, cfg.TokenFile, time.Now().Add(-time.Hour))
	start := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Minute)
	fake.AddEvent(googletest.Event{ID: "e1", Summary: "Dentist", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339)})
	return New(cfg, time.UTC), fake
}

func run(t *testing.T, c *Client, name string, input any) string {
	t.Helper()
	for _, tl := range c.Tools() {
		if tl.Spec().Name == name {
			raw, _ := json.Marshal(input)
			out, err := tl.Run(context.Background(), tools.Call{Input: raw})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("no tool %s", name)
	return ""
}

// Every calendar call used to renew the Google sign-in again (the renewed
// token was never kept): one extra round trip to Google per screen refresh
// and watcher tick. Now it is renewed once and shared.
func TestCalendarRenewsTheSignInOnce(t *testing.T) {
	c, fake := connected(t)
	for range 3 {
		if out := run(t, c, "list_events", map[string]any{"from": time.Now().Format(time.RFC3339), "to": time.Now().Add(48 * time.Hour).Format(time.RFC3339)}); !strings.Contains(out, "Dentist") {
			t.Fatalf("list: %s", out)
		}
	}
	for range 2 {
		snap, err := c.Snapshot(context.Background())
		if err != nil || len(snap) != 1 {
			t.Fatalf("snapshot: %v %v", snap, err)
		}
	}
	// A second client for the same account (the daemon builds a new one when
	// settings change) shares the sign-in too.
	again := New(c.cfg, time.UTC)
	if _, err := again.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fake.Refreshes(); got != 1 {
		t.Fatalf("renewed %d times for 6 calls, want 1", got)
	}
}

// The presence screen asks every 15 s per open screen; the next events are
// kept for a minute instead of asking Google each time.
func TestUpcomingIsKeptBriefly(t *testing.T) {
	c, fake := connected(t)
	for range 4 {
		evs, err := c.Upcoming(context.Background(), 5)
		if err != nil || len(evs) != 1 || evs[0].Title != "Dentist" {
			t.Fatalf("upcoming: %v %v", evs, err)
		}
	}
	if got := fake.Calls("/calendar/v3/calendars/primary/events"); got != 1 {
		t.Fatalf("asked Google %d times", got)
	}
	c.forget() // the calendar's own tools changed something
	if _, err := c.Upcoming(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if got := fake.Calls("/calendar/v3/calendars/primary/events"); got != 2 {
		t.Fatalf("a change wasn't picked up (%d calls)", got)
	}
}

func TestCalendarErrorsSayWhatToDo(t *testing.T) {
	c, fake := connected(t)
	fake.Disable("calendar")
	for _, tl := range c.Tools() {
		if tl.Spec().Name != "list_events" {
			continue
		}
		_, err := tl.Run(context.Background(), tools.Call{Input: json.RawMessage(`{}`)})
		if err == nil || !strings.Contains(err.Error(), "Calendar API is turned off") {
			t.Fatalf("got %v", err)
		}
	}
	dir := t.TempDir()
	lone := New(config.Calendar{CredentialsFile: filepath.Join(dir, "c.json"), TokenFile: filepath.Join(dir, "t.json")}, time.UTC)
	if _, err := lone.Upcoming(context.Background(), 3); err == nil || !strings.Contains(err.Error(), "Accounts") {
		t.Fatalf("not connected: %v", err)
	}
}

// The watcher's clash check reads each event's times from the last
// snapshot, in the calendar's own zone.
func TestSnapshotKeepsEachEventsTimes(t *testing.T) {
	c, fake := connected(t)
	if _, _, ok := c.Span("e1"); ok {
		t.Fatal("a time known before any snapshot")
	}
	paris, _ := time.LoadLocation("Europe/Paris")
	c.loc = paris
	fake.AddEvent(googletest.Event{ID: "e2", Summary: "Standup", Start: "2030-10-01T08:00:00Z", End: "2030-10-01T08:15:00Z"})
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, en, ok := c.Span("e2")
	if !ok || st.Location() != paris || st.Format("15:04") != "10:00" || en.Format("15:04") != "10:15" {
		t.Fatalf("span %v–%v %v", st, en, ok)
	}
	if _, _, ok := c.Span("nope"); ok {
		t.Fatal("a time for an event that isn't there")
	}
}

// An event the owner declined, or one marked free, runs into nothing.
func TestDeclinedAndFreeEventsHaveNoSpan(t *testing.T) {
	c, fake := connected(t)
	fake.AddEvent(googletest.Event{ID: "no", Summary: "Offsite", Start: "2030-10-01T08:00:00Z", End: "2030-10-01T09:00:00Z", Declined: true})
	fake.AddEvent(googletest.Event{ID: "free", Summary: "Focus time", Start: "2030-10-01T10:00:00Z", End: "2030-10-01T12:00:00Z", Free: true})
	fake.AddEvent(googletest.Event{ID: "busy", Summary: "Review", Start: "2030-10-01T13:00:00Z", End: "2030-10-01T14:00:00Z"})
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"no", "free"} {
		if _, _, ok := c.Span(k); ok {
			t.Errorf("%s has a span", k)
		}
	}
	if _, _, ok := c.Span("busy"); !ok {
		t.Error("an ordinary event lost its span")
	}
}
