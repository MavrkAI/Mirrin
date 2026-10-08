package reminders

import (
	"testing"
	"time"
)

// now is Wednesday 7 October 2026, mid-morning, in London.
func factNow(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("no zone data")
	}
	return time.Date(2026, 10, 7, 10, 30, 0, 0, loc)
}

// A fact's date is read by rule: the next one, in the owner's zone.
func TestFactDateReadsTheNextDay(t *testing.T) {
	now := factNow(t)
	for _, c := range []struct {
		fact   string
		want   string // 2006-01-02, or "" for none
		kind   string
		yearly bool
	}{
		{"Mum's birthday is on the 12th.", "2026-10-12", OccasionBirthday, true},
		{"Mum's birthday is the 3rd", "2026-11-03", OccasionBirthday, true}, // this month's has gone
		{"Priya's birthday is 12 March", "2027-03-12", OccasionBirthday, true},
		{"Priya's birthday is March 12th", "2027-03-12", OccasionBirthday, true},
		{"Our anniversary is the 14th of October", "2026-10-14", OccasionAnniversary, true},
		{"Akshay was born on 3 May 1985", "2027-05-03", OccasionBirthday, true},
		{"Dentist appointment on 2026-10-20 at 3pm", "2026-10-20", OccasionAppointment, false},
		{"The bins go out on Friday", "2026-10-09", OccasionAppointment, false},
		{"Exam on 5 January", "2027-01-05", OccasionAppointment, false},
		{"Haircut tomorrow at 9:30", "2026-10-08", OccasionAppointment, false},
		// none
		{"Akshay doesn't eat meat.", "", "", false},
		{"Priya is the 2nd child", "", "", false},
		{"The party was on the 12th", "", "", false},
		{"The meeting on 1 October went well", "", "", false},
		{"Book club is on 2 October", "", "", false}, // just gone, not next year
		{"Lunch on the 7th", "2026-11-07", OccasionAppointment, false},
		{"Interview on 2025-12-01", "", "", false},
		{"Payday is on the 31st", "2026-10-31", OccasionAppointment, false},
	} {
		o, ok := FactDate(c.fact, now)
		if c.want == "" {
			if ok {
				t.Errorf("%q: read %v, want no date", c.fact, o.Day)
			}
			continue
		}
		if !ok || o.Day.Format("2006-01-02") != c.want || o.Kind != c.kind || o.Yearly != c.yearly {
			t.Errorf("%q: got %v %v %q yearly=%v, want %s %s yearly=%v", c.fact, ok, o.Day.Format("2006-01-02"), o.Kind, o.Yearly, c.want, c.kind, c.yearly)
		}
	}
}

// A birthday is a word the day before; an appointment that morning, or
// the evening before when it is early; a time already gone is the next
// one still to come before the occasion.
func TestRemindLeadTimeByKind(t *testing.T) {
	now := factNow(t)
	for _, c := range []struct {
		fact, due, ask, text string
	}{
		{"Mum's birthday is on the 12th.", "2026-10-11 09:00", "Remind me on the 11th?", "Mum's birthday is on the 12th (tomorrow)"},
		{"Dentist on the 20th at 3pm", "2026-10-20 08:00", "Remind me on the 20th?", "Dentist on the 20th at 3pm (today)"},
		{"Flight on the 9th at 7am", "2026-10-08 18:00", "Remind me tomorrow?", "Flight on the 9th at 7am (tomorrow)"},
		{"Priya's birthday is 12 March", "2027-03-11 09:00", "Remind me on 11 March?", "Priya's birthday is 12 March (tomorrow)"},
		{"Mum's birthday is tomorrow", "2026-10-07 18:00", "Remind me at 6pm?", "Mum's birthday is tomorrow (tomorrow)"},
	} {
		o, ok := Remind(c.fact, now)
		if !ok || o.Due.Format("2006-01-02 15:04") != c.due || o.Ask != c.ask || o.Text != c.text {
			t.Errorf("%q: got %v %s %q %q, want %s %q %q", c.fact, ok, o.Due.Format("2006-01-02 15:04"), o.Ask, o.Text, c.due, c.ask, c.text)
		}
	}
	o, _ := Remind("Mum's birthday is on the 12th.", now)
	if o.Done != "I'll remind you on the 11th, and every year after." {
		t.Errorf("confirmation: %q", o.Done)
	}
	if o, _ := Remind("Lunch on the 20th", now); o.Done != "I'll remind you on the 20th." {
		t.Errorf("one-off confirmation: %q", o.Done)
	}
	// Nothing left before it starts: no offer.
	evening := time.Date(2026, 10, 7, 19, 0, 0, 0, now.Location())
	if o, ok := Remind("Haircut tomorrow at 7am", evening); ok {
		t.Errorf("a 7am haircut was offered at %v", o.Due)
	}
	if o, ok := Remind("Haircut tomorrow at 8am", evening); !ok || o.Due.Format("2006-01-02 15:04") != "2026-10-08 07:00" {
		t.Errorf("an 8am haircut: %v %v", ok, o.Due)
	}
}

// A yearly occasion comes round again a year on.
func TestNextYear(t *testing.T) {
	now := factNow(t)
	fact := "Mum's birthday is on the 12th."
	o, _ := Remind(fact, now)
	after := o.Due.Add(time.Minute)
	n, ok := o.Occasion.NextYear(fact, after)
	if !ok || n.Due.Format("2006-01-02 15:04") != "2027-10-11 09:00" {
		t.Fatalf("next year: %v %v", ok, n.Due)
	}
	if lunch, _ := Remind("Lunch on the 20th", now); func() bool { _, ok := lunch.Occasion.NextYear("Lunch on the 20th", now); return ok }() {
		t.Fatal("a one-off came round again")
	}
}
