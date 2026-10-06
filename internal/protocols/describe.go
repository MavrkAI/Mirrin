package protocols

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Describe says when a schedule runs, in words: "every day at 7:00",
// "weekdays at 8:30", "weekends at 9:45", "Sundays at 18:00", "on the 1st
// at 9:00". Times are on the 24-hour clock without a leading zero; the
// Routines page shows them in the screen's own format. A shape it doesn't
// know is given back as the cron text, and no schedule is "when you ask".
func Describe(schedule string) string {
	s := strings.TrimSpace(schedule)
	switch strings.ToLower(s) {
	case "":
		return "when you ask"
	case "@hourly":
		return "every hour"
	case "@daily", "@midnight":
		return "every day at 0:00"
	case "@weekly":
		return "Sundays at 0:00"
	case "@monthly":
		return "on the 1st at 0:00"
	}
	f := strings.Fields(s)
	if len(f) != 5 || f[3] != "*" {
		return s
	}
	minute, hour, dom, dow := f[0], f[1], f[2], f[4]
	if hour == "*" && dom == "*" && dow == "*" {
		if minute == "0" {
			return "every hour"
		}
		if n, ok := strings.CutPrefix(minute, "*/"); ok && inRange(n, 1, 59) {
			return "every " + n + " minutes"
		}
		return s
	}
	at, ok := clockTimes(minute, hour)
	if !ok {
		return s
	}
	switch {
	case dom == "*" && dow == "*":
		return "every day " + at
	case dom == "*":
		if days, ok := dayWords(dow); ok {
			return days + " " + at
		}
	case dow == "*":
		if days, ok := monthDays(dom); ok {
			return "on the " + days + " " + at
		}
	}
	return s
}

// clockTimes is "at 7:00", or "at 7:00 and 19:00" for a list of hours.
func clockTimes(minute, hour string) (string, bool) {
	if !inRange(minute, 0, 59) {
		return "", false
	}
	m, _ := strconv.Atoi(minute)
	var times []string
	for _, h := range strings.Split(hour, ",") {
		if !inRange(h, 0, 23) {
			return "", false
		}
		n, _ := strconv.Atoi(h)
		times = append(times, Clock(n, m))
	}
	return "at " + and(times), true
}

// Clock is a time of day as the twin writes it: "7:00", "18:30".
func Clock(hour, minute int) string { return fmt.Sprintf("%d:%02d", hour, minute) }

var (
	dayNames  = []string{"Sundays", "Mondays", "Tuesdays", "Wednesdays", "Thursdays", "Fridays", "Saturdays"}
	dayAbbrev = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

// dayWords names the days of the week a cron field picks: "weekdays",
// "weekends", "Sundays", "Mondays and Thursdays".
func dayWords(field string) (string, bool) {
	var on [7]bool
	for _, part := range strings.Split(field, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, ok := weekday(lo)
		if !ok {
			return "", false
		}
		b := a
		if isRange {
			if b, ok = weekday(hi); !ok || b < a {
				return "", false
			}
		}
		for d := a; d <= b; d++ {
			on[d] = true
		}
	}
	var picked []string
	weekdays, weekends := true, true
	for d := range 7 {
		weekend := d == 0 || d == 6
		if on[d] != !weekend {
			weekdays = false
		}
		if on[d] != weekend {
			weekends = false
		}
	}
	switch {
	case weekdays:
		return "weekdays", true
	case weekends:
		return "weekends", true
	case on == [7]bool{true, true, true, true, true, true, true}:
		return "every day", true
	}
	for _, d := range []int{1, 2, 3, 4, 5, 6, 0} { // the week as people say it, Monday first
		if on[d] {
			picked = append(picked, dayNames[d])
		}
	}
	return and(picked), true
}

func weekday(s string) (int, bool) {
	if d, ok := dayAbbrev[strings.ToLower(s)]; ok {
		return d, true
	}
	if !inRange(s, 0, 6) {
		return 0, false
	}
	d, _ := strconv.Atoi(s)
	return d, true
}

// monthDays is "1st", or "1st and 15th".
func monthDays(field string) (string, bool) {
	var out []string
	for _, part := range strings.Split(field, ",") {
		if !inRange(part, 1, 31) {
			return "", false
		}
		n, _ := strconv.Atoi(part)
		out = append(out, ordinal(n))
	}
	return and(out), true
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}

// inRange reports whether s is a plain whole number from lo to hi.
func inRange(s string, lo, hi int) bool {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= lo && n <= hi
}

// and joins words as a sentence does: "a", "a and b", "a, b and c".
func and(ws []string) string {
	if len(ws) < 2 {
		return strings.Join(ws, "")
	}
	return strings.Join(ws[:len(ws)-1], ", ") + " and " + ws[len(ws)-1]
}

// NextRun is when a schedule next comes due after now, in now's zone: zero
// for no schedule, or one cron can't read.
func NextRun(schedule string, now time.Time) time.Time {
	if strings.TrimSpace(schedule) == "" {
		return time.Time{}
	}
	s, err := cron.ParseStandard(schedule)
	if err != nil {
		return time.Time{}
	}
	return s.Next(now)
}

// WhenText says when a run is, from now: "today at 7:00", "tomorrow at
// 7:00", "Monday 5 Oct at 7:00". t is read in now's zone.
func WhenText(t, now time.Time) string {
	t = t.In(now.Location())
	at := " at " + Clock(t.Hour(), t.Minute())
	y, m, d := now.Date()
	switch ty, tm, td := t.Date(); {
	case ty == y && tm == m && td == d:
		return "today" + at
	case time.Date(y, m, d+1, 0, 0, 0, 0, now.Location()).Format(time.DateOnly) == t.Format(time.DateOnly):
		return "tomorrow" + at
	}
	return t.Format("Monday 2 Jan") + at
}

// SkipKey is where the store keeps the date of a protocol's next scheduled
// run the owner asked to skip (update_protocol's skip_next, or Skip next
// on the Routines page). The heartbeat skips that run and clears it.
func SkipKey(name string) string { return "protocol.skip." + Slug(name) }
