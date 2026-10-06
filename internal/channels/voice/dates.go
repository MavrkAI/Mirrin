package voice

import (
	"regexp"
	"strconv"
	"strings"
)

// Dates are written for the eye ("Oct 2"); a voice reading
// them literally says "oct two". These say them the way people do:
// "October the second", "the second of October", "Friday the second of October".

var months = map[string]string{
	"jan": "January", "feb": "February", "mar": "March", "apr": "April", "may": "May", "jun": "June",
	"jul": "July", "aug": "August", "sep": "September", "sept": "September", "oct": "October", "nov": "November", "dec": "December",
}

var weekdays = map[string]string{
	"mon": "Monday", "tue": "Tuesday", "tues": "Tuesday", "wed": "Wednesday", "thu": "Thursday", "thur": "Thursday",
	"thurs": "Thursday", "fri": "Friday", "sat": "Saturday", "sun": "Sunday",
}

var ordinals = []string{"", "first", "second", "third", "fourth", "fifth", "sixth", "seventh", "eighth", "ninth", "tenth",
	"eleventh", "twelfth", "thirteenth", "fourteenth", "fifteenth", "sixteenth", "seventeenth", "eighteenth", "nineteenth", "twentieth",
	"twenty-first", "twenty-second", "twenty-third", "twenty-fourth", "twenty-fifth", "twenty-sixth", "twenty-seventh", "twenty-eighth", "twenty-ninth", "thirtieth",
	"thirty-first"}

const (
	reMonth   = `(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)`
	reWeekday = `((mon(?:day)?|tue(?:s(?:day)?)?|wed(?:nesday)?|thu(?:r(?:s(?:day)?)?)?|fri(?:day)?|sat(?:urday)?|sun(?:day)?)\.?,?\s+)?`
	reDay     = `(\d{1,2})(?:st|nd|rd|th)?`
)

var (
	// "Fri, Oct 2", "October 2nd", "oct 2" ("Oct 2026" isn't: the day stops at two digits)
	reMonthDay = regexp.MustCompile(`(?i)\b` + reWeekday + reMonth + `\.?\s+` + reDay + `\b`)
	// "Fri 2 Oct", "2nd of October", "2 oct"
	reDayMonth = regexp.MustCompile(`(?i)\b` + reWeekday + reDay + `\s+(?:of\s+)?` + reMonth + `\b`)
)

func speakDates(s string) string {
	s = reMonthDay.ReplaceAllStringFunc(s, func(m string) string {
		g := reMonthDay.FindStringSubmatch(m)
		return spokenDate(m, g[1], g[2], g[3], g[4], "%M the %D")
	})
	return reDayMonth.ReplaceAllStringFunc(s, func(m string) string {
		g := reDayMonth.FindStringSubmatch(m)
		return spokenDate(m, g[1], g[2], g[4], g[3], "the %D of %M")
	})
}

// spokenDate says one date, or gives back what it matched when that isn't one.
func spokenDate(matched, before, wd, mon, day, form string) string {
	n, _ := strconv.Atoi(day)
	// "may" and "mar" are words too; only the capitalised ones are months.
	if n < 1 || n > 31 || mon == "may" || mon == "mar" {
		return matched
	}
	key := strings.ToLower(mon)
	month, ok := months[key]
	if !ok {
		month = months[key[:3]]
	}
	spoken := strings.NewReplacer("%M", month, "%D", ordinals[n]).Replace(form)
	// A weekday is written capitalised; "I sat 3 Dec" isn't a Saturday.
	if wd != "" && strings.ToLower(wd[:1]) == wd[:1] {
		spoken = before + spoken
	} else if wd != "" {
		k := strings.ToLower(wd)
		name, ok := weekdays[k]
		if !ok {
			name = weekdays[k[:3]]
		}
		spoken = name + " " + spoken
	}
	return spoken
}
