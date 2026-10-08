package reminders

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// When the owner tells the twin something with a date in it ("Mum's
// birthday is on the 12th"), the screen offers a reminder under "Noted"
// (daemon/datereminder.go). The date is read here, by rule, never by the
// model: a birthday or an anniversary comes round every year and is worth a
// word the day before; anything else (an appointment, the bins) is worth one
// that morning, or the evening before when it is early.

// Occasion kinds.
const (
	OccasionBirthday    = "birthday"
	OccasionAnniversary = "anniversary"
	OccasionAppointment = "appointment"
)

// Occasion is a dated thing a fact speaks of.
type Occasion struct {
	Kind string
	// Day is the next one, at midnight in the owner's zone.
	Day time.Time
	// At is its time of day, when the fact says one ("at 3pm").
	At     time.Duration
	HasAt  bool
	Yearly bool
}

// Offer is a reminder for an occasion: when it goes out, what it says, and
// how the screen asks and confirms.
type Offer struct {
	Occasion Occasion
	Due      time.Time
	Text     string // the reminder itself, without "Reminder: "
	Ask      string // "Remind me on the 11th?"
	Done     string // "I'll remind you on the 11th, and every year after."
}

var (
	months = map[string]time.Month{
		"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3, "apr": 4, "april": 4,
		"may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7, "aug": 8, "august": 8, "sep": 9, "sept": 9,
		"september": 9, "oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
	}
	weekdays = map[string]time.Weekday{"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6}

	monthRe        = `(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sept?(?:ember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)`
	reISO          = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	reDayMonth     = regexp.MustCompile(`\b(\d{1,2})(?:st|nd|rd|th)?(?: of)? ` + monthRe + `\b(?:,? (\d{4})\b)?`)
	reMonthDay     = regexp.MustCompile(`\b` + monthRe + ` (?:the )?(\d{1,2})(?:st|nd|rd|th)?\b(?:,? (\d{4})\b)?`)
	reDayOnly      = regexp.MustCompile(`\b(on|is) the (\d{1,2})(?:st|nd|rd|th)\b`)
	reWeekday      = regexp.MustCompile(`\b(?:on|next|this) (monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`)
	reTomorrowWord = regexp.MustCompile(`\btomorrow\b`)
	reClock        = regexp.MustCompile(`\bat (\d{1,2})(?:[:.](\d{2}))? ?(am|pm)?\b`)
	rePast         = regexp.MustCompile(`\b(was|were|went|did|had|last|ago|used to)\b`)
	reBirthday     = regexp.MustCompile(`\b(birthday|bday|b-day|born)\b`)
	reAnniv        = regexp.MustCompile(`\banniversary\b`)
)

// FactDate reads the next occasion a fact speaks of, from now (in the
// owner's zone). It says false for a fact with no date, one about the past
// ("was on the 3rd"), or a date that is today or gone.
func FactDate(fact string, now time.Time) (Occasion, bool) {
	s := strings.ToLower(strings.Join(strings.Fields(fact), " "))
	s = strings.ReplaceAll(s, "’", "'")
	o := Occasion{Kind: OccasionAppointment}
	switch {
	case reBirthday.MatchString(s):
		o.Kind, o.Yearly = OccasionBirthday, true
	case reAnniv.MatchString(s):
		o.Kind, o.Yearly = OccasionAnniversary, true
	}
	if rePast.MatchString(s) && !strings.Contains(s, "born") {
		return Occasion{}, false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	day, ok := findDay(s, today, o.Yearly)
	if !ok || !day.After(today) {
		return Occasion{}, false
	}
	o.Day = day
	if !o.Yearly {
		if m := reClock.FindStringSubmatch(s); m != nil {
			if at, ok := clockTime(m); ok {
				o.At, o.HasAt = at, true
			}
		}
	}
	return o, true
}

// findDay is the day s names, the next one after today.
func findDay(s string, today time.Time, yearly bool) (time.Time, bool) {
	loc := today.Location()
	if m := reISO.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return dated(y, time.Month(mo), d, today, yearly, true)
	}
	if m := reDayMonth.FindStringSubmatch(s); m != nil {
		d, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[3])
		return dated(y, months[m[2]], d, today, yearly, m[3] != "")
	}
	if m := reMonthDay.FindStringSubmatch(s); m != nil {
		d, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		return dated(y, months[m[1]], d, today, yearly, m[3] != "")
	}
	// "is the 2nd" is a date only for a birthday or an anniversary: Priya
	// "is the 2nd" child.
	if m := reDayOnly.FindStringSubmatch(s); m != nil && (m[1] == "on" || yearly) {
		d, _ := strconv.Atoi(m[2])
		// This month's, or next month's once it has gone.
		for k := 0; k < 2; k++ {
			t := time.Date(today.Year(), today.Month()+time.Month(k), d, 0, 0, 0, 0, loc)
			if t.Day() != d { // no 31st this month
				continue
			}
			if t.After(today) {
				return t, true
			}
		}
		return time.Time{}, false
	}
	if m := reWeekday.FindStringSubmatch(s); m != nil {
		n := (int(weekdays[m[1]]) - int(today.Weekday()) + 7) % 7
		if n == 0 {
			n = 7
		}
		return today.AddDate(0, 0, n), true
	}
	if reTomorrowWord.MatchString(s) {
		return today.AddDate(0, 0, 1), true
	}
	return time.Time{}, false
}

// dated is day d of month mo: in year y when it is given (or, for a yearly
// occasion, the next one after today, whatever year was said: "born 3 May
// 1985"), else this year's, or next year's once this year's has gone. A
// one-off whose day went by in the last two months is in the past, not next
// year.
func dated(y int, mo time.Month, d int, today time.Time, yearly, hasYear bool) (time.Time, bool) {
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return time.Time{}, false
	}
	valid := func(t time.Time) bool { return t.Day() == d && t.Month() == mo }
	if hasYear && !yearly {
		t := time.Date(y, mo, d, 0, 0, 0, 0, today.Location())
		return t, valid(t)
	}
	if yearly && mo == 2 && d == 29 {
		d = 28 // a leap-day birthday is kept on the 28th in the years between
	}
	t := time.Date(today.Year(), mo, d, 0, 0, 0, 0, today.Location())
	if !valid(t) {
		return time.Time{}, false
	}
	if t.After(today) {
		return t, true
	}
	if !yearly && today.Sub(t) < 60*24*time.Hour {
		return time.Time{}, false
	}
	return t.AddDate(1, 0, 0), true
}

// clockTime reads "at 3pm", "at 15:30", "at 9": an hour from 1 to 6 with no
// am or pm is the afternoon.
func clockTime(m []string) (time.Duration, bool) {
	h, _ := strconv.Atoi(m[1])
	mins, _ := strconv.Atoi(m[2])
	switch m[3] {
	case "am", "pm":
		if h < 1 || h > 12 {
			return 0, false
		}
		h %= 12
		if m[3] == "pm" {
			h += 12
		}
	default:
		if m[2] == "" && h >= 1 && h <= 6 {
			h += 12
		}
	}
	if h > 23 || mins > 59 {
		return 0, false
	}
	return time.Duration(h)*time.Hour + time.Duration(mins)*time.Minute, true
}

// Remind is the reminder for an occasion a fact speaks of, from now: for a
// birthday or an anniversary, 9 am the day before; for anything else, 8 am
// that morning, or 6 pm the evening before when it starts before ten. When
// that time has already gone, it is the next of those still to come before
// the occasion. It says false when there is none.
func Remind(fact string, now time.Time) (Offer, bool) {
	o, ok := FactDate(fact, now)
	if !ok {
		return Offer{}, false
	}
	return o.offer(fact, now)
}

// NextYear is the reminder for the same yearly occasion a year on.
func (o Occasion) NextYear(fact string, now time.Time) (Offer, bool) {
	if !o.Yearly {
		return Offer{}, false
	}
	n := o
	n.Day = time.Date(o.Day.Year()+1, o.Day.Month(), o.Day.Day(), 0, 0, 0, 0, o.Day.Location())
	return n.offer(fact, now)
}

func (o Occasion) offer(fact string, now time.Time) (Offer, bool) {
	at := func(days, hour int) time.Time {
		d := o.Day.AddDate(0, 0, days)
		return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, d.Location())
	}
	var tries []time.Time
	switch {
	case o.Yearly:
		tries = []time.Time{at(-1, 9), at(-1, 18), at(0, 8)}
	case o.HasAt && o.At < 10*time.Hour:
		tries = []time.Time{at(-1, 18), at(0, 7)}
	default:
		tries = []time.Time{at(0, 8), at(-1, 18)}
	}
	start := o.Day.Add(o.At)
	for _, due := range tries {
		if !due.After(now) || (o.HasAt && !due.Before(start)) {
			continue
		}
		when := "today"
		if due.YearDay() != o.Day.YearDay() {
			when = "tomorrow"
		}
		text := strings.TrimRight(strings.TrimSpace(fact), ".") + " (" + when + ")"
		ask, done := "Remind me "+dayWords(due, now)+"?", "I'll remind you "+dayWords(due, now)
		if o.Yearly {
			done += ", and every year after"
		}
		return Offer{Occasion: o, Due: due, Text: text, Ask: ask, Done: done + "."}, true
	}
	return Offer{}, false
}

// dayWords says when due is, from now: "at 6pm" today, "tomorrow", "on the
// 11th" within the month ahead, else "on 11 March".
func dayWords(due, now time.Time) string {
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, due.Location())
	day := time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, due.Location())
	switch days := int(day.Sub(today).Hours()/24 + 0.5); {
	case days <= 0:
		return "at " + clockWords(due)
	case days == 1:
		return "tomorrow"
	case days < 28:
		return "on the " + ordinal(due.Day())
	}
	return fmt.Sprintf("on %d %s", due.Day(), due.Month())
}

func clockWords(t time.Time) string {
	h := t.Hour() % 12
	if h == 0 {
		h = 12
	}
	ap := "am"
	if t.Hour() >= 12 {
		ap = "pm"
	}
	if t.Minute() != 0 {
		return fmt.Sprintf("%d:%02d%s", h, t.Minute(), ap)
	}
	return fmt.Sprintf("%d%s", h, ap)
}

func ordinal(n int) string {
	suf := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suf = "st"
		case 2:
			suf = "nd"
		case 3:
			suf = "rd"
		}
	}
	return strconv.Itoa(n) + suf
}
