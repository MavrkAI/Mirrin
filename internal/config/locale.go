package config

import (
	"os"
	"regexp"
	"strings"
	"sync"
)

// locale is what the twin assumes about where the user lives until told
// otherwise: the currency its spending caps are in, the voice it uses on phone
// calls, and the country code for phone numbers.
type locale struct {
	Currency    string
	PhoneVoice  string // Twilio/Polly voice
	PhoneLang   string
	CallingCode string
}

// capScale scales the default spending caps (100 and 500 in dollars,
// pounds or euros) to round amounts worth about as much in currencies whose
// unit is worth far less: 10 000 yen, not 100. A currency not listed is 1.
var capScale = map[string]float64{
	"INR": 100, "ZAR": 20, "SEK": 10, "NOK": 10, "DKK": 10, "PLN": 5,
	"JPY": 100, "KRW": 1000, "BRL": 5, "MXN": 20, "AED": 5,
}

// scaledFor is s with its caps scaled to round amounts worth about as much
// in currency.
func (s Spending) scaledFor(currency string) Spending {
	if m := capScale[strings.ToUpper(strings.TrimSpace(currency))]; m > 0 {
		s.PerActionLimit *= m
		s.MonthlyLimit *= m
	}
	return s
}

// locales by ISO 3166 country code. Anywhere else gets fallbackLocale.
var locales = map[string]locale{
	"US": {"USD", "Polly.Joanna-Neural", "en-US", "1"},
	"CA": {"CAD", "Polly.Joanna-Neural", "en-US", "1"},
	"GB": {"GBP", "Polly.Amy-Neural", "en-GB", "44"},
	"IE": {"EUR", "Polly.Niamh-Neural", "en-IE", "353"},
	"AU": {"AUD", "Polly.Olivia-Neural", "en-AU", "61"},
	"NZ": {"NZD", "Polly.Aria-Neural", "en-NZ", "64"},
	"IN": {"INR", "Polly.Kajal-Neural", "en-IN", "91"},
	"ZA": {"ZAR", "Polly.Ayanda-Neural", "en-ZA", "27"},
	"SG": {"SGD", "Polly.Joanna-Neural", "en-US", "65"},
	"DE": {"EUR", "Polly.Vicki-Neural", "de-DE", "49"},
	"AT": {"EUR", "Polly.Hannah-Neural", "de-AT", "43"},
	"CH": {"CHF", "Polly.Sabrina-Neural", "de-CH", "41"},
	"FR": {"EUR", "Polly.Lea-Neural", "fr-FR", "33"},
	"BE": {"EUR", "Polly.Lisa-Neural", "nl-BE", "32"},
	"NL": {"EUR", "Polly.Laura-Neural", "nl-NL", "31"},
	"ES": {"EUR", "Polly.Lucia-Neural", "es-ES", "34"},
	"PT": {"EUR", "Polly.Ines-Neural", "pt-PT", "351"},
	"IT": {"EUR", "Polly.Bianca-Neural", "it-IT", "39"},
	"FI": {"EUR", "Polly.Suvi-Neural", "fi-FI", "358"},
	"SE": {"SEK", "Polly.Elin-Neural", "sv-SE", "46"},
	"NO": {"NOK", "Polly.Ida-Neural", "nb-NO", "47"},
	"DK": {"DKK", "Polly.Sofie-Neural", "da-DK", "45"},
	"PL": {"PLN", "Polly.Ola-Neural", "pl-PL", "48"},
	"JP": {"JPY", "Polly.Kazuha-Neural", "ja-JP", "81"},
	"KR": {"KRW", "Polly.Seoyeon-Neural", "ko-KR", "82"},
	"BR": {"BRL", "Polly.Camila-Neural", "pt-BR", "55"},
	"MX": {"MXN", "Polly.Mia-Neural", "es-MX", "52"},
	"AE": {"AED", "Polly.Hala-Neural", "ar-AE", "971"},
}

var fallbackLocale = locale{"USD", "Polly.Joanna-Neural", "en-US", ""}

// zoneRegions maps common time zones to countries. Timezone is the best clue
// to where someone lives: laptops set it from their location, while LANG is
// en_US on many machines everywhere.
var zoneRegions = map[string]string{
	"America/New_York": "US", "America/Chicago": "US", "America/Denver": "US", "America/Los_Angeles": "US",
	"America/Phoenix": "US", "America/Anchorage": "US", "Pacific/Honolulu": "US", "America/Detroit": "US",
	"America/Boise": "US", "America/Indiana/Indianapolis": "US", "America/Kentucky/Louisville": "US",
	"America/Toronto": "CA", "America/Vancouver": "CA", "America/Edmonton": "CA", "America/Winnipeg": "CA",
	"America/Halifax": "CA", "America/St_Johns": "CA", "America/Regina": "CA", "America/Montreal": "CA",
	"Europe/London": "GB", "Europe/Belfast": "GB", "Europe/Dublin": "IE",
	"Pacific/Auckland": "NZ", "Asia/Kolkata": "IN", "Asia/Calcutta": "IN", "Africa/Johannesburg": "ZA",
	"Asia/Singapore": "SG", "Europe/Berlin": "DE", "Europe/Vienna": "AT", "Europe/Zurich": "CH",
	"Europe/Paris": "FR", "Europe/Brussels": "BE", "Europe/Amsterdam": "NL", "Europe/Madrid": "ES",
	"Europe/Lisbon": "PT", "Europe/Rome": "IT", "Europe/Helsinki": "FI", "Europe/Stockholm": "SE",
	"Europe/Oslo": "NO", "Europe/Copenhagen": "DK", "Europe/Warsaw": "PL", "Asia/Tokyo": "JP",
	"Asia/Seoul": "KR", "America/Sao_Paulo": "BR", "America/Mexico_City": "MX", "Asia/Dubai": "AE",
}

var reLocaleRegion = regexp.MustCompile(`^[a-z]{2,3}[_-]([A-Z]{2})\b`)

// regionFrom picks a country from a time zone, else from a POSIX locale
// such as "en_GB.UTF-8". Empty when neither says.
func regionFrom(tz, posixLocale string) string {
	if strings.HasPrefix(tz, "Australia/") {
		return "AU"
	}
	if r, ok := zoneRegions[tz]; ok {
		return r
	}
	if m := reLocaleRegion.FindStringSubmatch(posixLocale); m != nil {
		return m[1]
	}
	return ""
}

var (
	regionOnce sync.Once
	region     string
)

// Region guesses the user's country from the system time zone and locale.
// Empty when it can't tell.
func Region() string {
	regionOnce.Do(func() {
		lc := ""
		for _, k := range []string{"LC_ALL", "LC_MONETARY", "LANG"} {
			if v := os.Getenv(k); v != "" {
				lc = v
				break
			}
		}
		region = regionFrom(LocalTimezone(), lc)
	})
	return region
}

// localeFor returns the defaults for a country.
func localeFor(region string) locale {
	if l, ok := locales[region]; ok {
		return l
	}
	return fallbackLocale
}

// LocalTimezone is the system's IANA time zone name, or "" if unknown.
func LocalTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return strings.TrimPrefix(tz, ":")
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			return link[i+len("zoneinfo/"):]
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil { // Debian and friends
		return strings.TrimSpace(string(b))
	}
	return ""
}

// PhoneExample is an example number in E.164 for the user's country.
func PhoneExample() string {
	switch Region() {
	case "AU":
		return "+61400000000"
	case "GB":
		return "+447700900123"
	case "US", "CA", "":
		return "+14155550123"
	}
	if code := localeFor(Region()).CallingCode; code != "" {
		return "+" + code + "…"
	}
	return "+14155550123"
}

var reE164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// NormalizePhone turns what someone types ("+61 400 000-000") into E.164
// ("+61400000000") and reports whether the result is valid. A number without
// its country code ("0400 000 000") is not.
func NormalizePhone(s string) (string, bool) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(s) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return s, false
		}
	}
	out := b.String()
	if strings.HasPrefix(out, "00") { // international prefix used in much of the world
		out = "+" + out[2:]
	}
	return out, reE164.MatchString(out)
}
