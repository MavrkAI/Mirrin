package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/heartbeat"
	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

// The regression: after a zone move the presence screen's calendar, and the
// watcher's, still read times in the zone the twin started in.
func TestCalendarsFollowAZoneMove(t *testing.T) {
	t.Setenv("TZ", "Europe/Paris")
	td, fake := googleDaemon(t)
	connectThroughThePage(t, td)
	ctx := context.Background()
	fake.AddEvent(googletest.Event{ID: "e1", Summary: "Dentist", Start: "2030-10-01T08:00:00Z", End: "2030-10-01T09:00:00Z"})
	td.watcher.Poll(ctx) // the starting point

	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	// The heartbeat has noticed the move (as its look at the clock does),
	// then tells the rest of the twin.
	td.beat = heartbeat.New(td.store, nil, nil, td.ownerChatKey, tokyo, nil)
	td.zoneMoved(tokyo)
	if cal := td.calendar.Load(); cal == nil || cal.Location() != tokyo {
		t.Fatalf("the screen's calendar didn't follow the move: %v", cal)
	}
	td.watcher.Poll(ctx)
	if auditHas(t, td, "watch.change", "Dentist") {
		t.Fatal("the move read as the dentist having moved")
	}
	fake.AddEvent(googletest.Event{ID: "e2", Summary: "Flight", Start: "2030-10-02T01:00:00Z", End: "2030-10-02T03:00:00Z"})
	td.watcher.Poll(ctx)
	if !auditHas(t, td, "watch.change", "calendar: NEW: Wed 2 Oct 10:00–12:00 | Flight") {
		t.Fatal("the watcher's calendar change isn't in the new zone")
	}
	// The hourly check re-adds the calendar source: it keeps the new zone.
	td.syncGoogleWatch(false)
	fake.AddEvent(googletest.Event{ID: "e3", Summary: "Lunch", Start: "2030-10-03T03:00:00Z", End: "2030-10-03T04:00:00Z"})
	td.watcher.Poll(ctx)
	if !auditHas(t, td, "watch.change", "calendar: NEW: Thu 3 Oct 12:00–13:00 | Lunch") {
		t.Fatal("re-adding the calendar source went back to the old zone")
	}
}
