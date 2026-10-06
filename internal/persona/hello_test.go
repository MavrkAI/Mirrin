package persona

import (
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	for _, c := range []struct{ s, name, address, want string }{
		{"Good morning{, address}.", "Akshay", "sir", "Good morning, sir."},
		{"Good morning{, address}.", "Akshay", "", "Good morning."},
		{"Late one{, address}.", "", "ma'am", "Late one, ma'am."},
		{"Morning, {name}.", "Akshaya", "", "Morning, Akshaya."},
		{"Morning, {name}.", "", "", "Morning."},
		{"Oh, hello, {name}.", "", "", "Oh, hello."},
		{"Hi {name}!", "  ", "", "Hi!"},
		{"{address}, {name}", "Akshaya", "boss", "boss, Akshaya"},
		{"Good to see you{, address}. What can I take off your plate?", "", "sir", "Good to see you, sir. What can I take off your plate?"},
		{"Hello. Nyra here.", "Akshaya", "sir", "Hello. Nyra here."},
		// what is filled in isn't filled in again
		{"Morning, {name}.", "{address}", "sir", "Morning, {address}."},
	} {
		if got := Render(c.s, c.name, c.address); got != c.want {
			t.Errorf("Render(%q, %q, %q) = %q, want %q", c.s, c.name, c.address, got, c.want)
		}
	}
}

func TestPartOfDay(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 3, h, m, 0, 0, time.UTC) }
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{at(4, 59), "late"}, {at(5, 0), "morning"}, {at(7, 40), "morning"}, {at(11, 59), "morning"},
		{at(12, 0), "afternoon"}, {at(16, 59), "afternoon"}, {at(17, 0), "evening"}, {at(22, 59), "evening"},
		{at(23, 0), "late"}, {at(0, 0), "late"}, {at(1, 0), "late"},
	} {
		if got := PartOfDay(c.t); got != c.want {
			t.Errorf("%s: %q, want %q", c.t.Format("15:04"), got, c.want)
		}
	}
}

// Every bundled persona has one hello for each part of the day, and a late
// one that never mentions sleep.
func TestBundledHellos(t *testing.T) {
	want := map[string]map[string]string{
		"mirrin": {"morning": "Good morning, sir.", "afternoon": "Good afternoon, sir.", "evening": "Good evening, sir.", "late": "Late one, sir."},
		"nyra":   {"morning": "Morning, Akshaya.", "afternoon": "Afternoon, Akshaya.", "evening": "Evening, Akshaya.", "late": "Hello, Akshaya."},
		"pickoo": {"morning": "Morning, Akshaya!", "afternoon": "Afternoon, Akshaya!", "evening": "Evening, Akshaya!", "late": "Oh, hello, Akshaya."},
	}
	for _, p := range Bundled() {
		w, ok := want[p.ID]
		if !ok {
			t.Errorf("%s: no hellos checked", p.ID)
			continue
		}
		if len(p.Hellos) != len(PartsOfDay) {
			t.Errorf("%s has hellos %v", p.ID, p.Hellos)
		}
		for _, part := range PartsOfDay {
			if got := Render(p.Hellos[part], "Akshaya", p.Address); got != w[part] {
				t.Errorf("%s %s: %q, want %q", p.ID, part, got, w[part])
			}
			if l := strings.ToLower(p.Hellos[part]); strings.Contains(l, "sleep") || strings.Contains(l, "bed") {
				t.Errorf("%s %s mentions sleep: %q", p.ID, part, p.Hellos[part])
			}
		}
	}
	if g := Render(Default().Greeting, "Akshay", "sir"); g != "Good to see you, sir. What can I take off your plate?" {
		t.Errorf("Mirrin's greeting %q", g)
	}
}
