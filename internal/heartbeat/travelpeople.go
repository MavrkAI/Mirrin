package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/memory"
	memskill "github.com/MavrkAI/Mirrin/internal/skills/memory"
)

// The travel welcome remembers who the owner knows there. When the laptop
// lands in a new city, the one-time "You're in X now" notice may carry one
// hedged offer drawn from what the owner told the twin: "You told me Dan
// moved here. Want a nudge tomorrow to see if Dan's free for coffee?" A yes
// (daemon/travelpeople.go) sets a reminder for the owner; nobody else is
// ever messaged. It is said at most once per city per trip, never draws on
// health, money, relationships, family or secrets, and says nothing when
// nothing matches. "Stop telling me these" turns it off (TravelPeopleOffKey).

const (
	// TravelOfferKey holds the offer the latest travel notice made, for the
	// owner's yes (TravelOffer, as JSON).
	TravelOfferKey = "travel.offer.v1"
	// TravelPeopleOffKey is "1" once the owner asked not to hear these.
	TravelPeopleOffKey = "travel.people.off"
	travelTripKey      = "travel.people.trip.v1" // the zones weighed this trip, as a JSON list
	travelHomeKey      = "travel.home.v1"        // the zone the first move left: home, most likely
)

// TravelOffer is what a travel notice offered: a reminder for the owner.
type TravelOffer struct {
	Ask    string    `json:"ask"`           // the question as sent, to tell a yes is about it
	Remind string    `json:"remind"`        // the reminder's text
	Who    string    `json:"who,omitempty"` // the person it's about, if one
	Zone   string    `json:"zone"`
	At     time.Time `json:"at"`
}

// multiCity are zones named for one city that also keep the time of other
// big ones: the twin can't tell from the zone that the owner is in the named
// city, so a match there is offered "if you're nearby".
var multiCity = map[string]bool{
	"America/New_York": true, "America/Chicago": true, "America/Denver": true, "America/Los_Angeles": true,
	"America/Phoenix": true, "America/Toronto": true, "America/Vancouver": true, "America/Sao_Paulo": true,
	"America/Mexico_City": true, "America/Argentina/Buenos_Aires": true, "America/Bogota": true, "America/Lima": true,
	"Asia/Kolkata": true, "Asia/Calcutta": true, "Asia/Shanghai": true, "Asia/Jakarta": true, "Asia/Tokyo": true,
	"Asia/Seoul": true, "Asia/Manila": true, "Asia/Karachi": true, "Asia/Dhaka": true, "Asia/Ho_Chi_Minh": true,
	"Asia/Riyadh": true, "Asia/Dubai": true, "Asia/Tehran": true, "Asia/Bangkok": true, "Asia/Kuala_Lumpur": true,
	"Europe/London": true, "Europe/Paris": true, "Europe/Berlin": true, "Europe/Madrid": true, "Europe/Rome": true,
	"Europe/Moscow": true, "Europe/Istanbul": true, "Europe/Warsaw": true, "Europe/Amsterdam": true,
	"Europe/Zurich": true, "Europe/Brussels": true, "Europe/Kyiv": true, "Australia/Sydney": true,
	"Australia/Brisbane": true, "Pacific/Auckland": true, "Africa/Lagos": true, "Africa/Johannesburg": true,
	"Africa/Cairo": true, "Africa/Nairobi": true,
}

// cityNames are other names people use for a zone's city.
var cityNames = map[string][]string{
	"New York":    {"NYC", "New York City", "Manhattan", "Brooklyn"},
	"Kolkata":     {"Calcutta"},
	"Calcutta":    {"Kolkata"},
	"Ho Chi Minh": {"Ho Chi Minh City", "Saigon"},
	"Kyiv":        {"Kiev"},
	"Kiev":        {"Kyiv"},
	"Sao Paulo":   {"São Paulo"},
	"Mexico City": {"CDMX"},
}

// private is a fact the travel welcome never draws on, whatever its
// subject: health, money, love, family, secrets.
var private = regexp.MustCompile(`(?i)\b(ex|exes|girlfriend|boyfriend|wife|husband|partner|fianc[eé]e?|dating|dated|date|divorc\w*|separated|affair|crush|romantic|mum|mom|mother|dad|father|sister|brother|son|daughter|aunt|uncle|cousin|grand\w*|nan|in-laws?|family|hospital|clinic|therapy|therapist|doctor|cancer|ill|illness|sick|surgery|treatment|rehab|pregnan\w*|funeral|died|dead|passed away|grave|grief|debt|debts|owes?|owed|loan|money|salary|secret\w*|password|anxiety|anxious|depression|depressed|diagnos\w*|mental|ivf|chemo\w*|medication|addiction|rent|bankrupt\w*|mortgage|lawsuit)\b`)

// privateSubjects are subjects beyond memskill.Sensitive kept out too.
var privateSubjects = map[string]bool{"family": true, "relationship": true, "relationships": true, "dating": true, "love": true, "partner": true}

// notAName are capitalised words that start a sentence but name nobody.
var notAName = map[string]bool{
	"User": true, "The": true, "I": true, "He": true, "She": true, "They": true, "We": true, "My": true,
	"Owner": true, "His": true, "Her": true, "Their": true, "Our": true, "Someone": true, "A": true, "An": true,
	"Friend": true, "Colleague": true, "It": true, "This": true, "That": true, "Then": true, "Now": true,
	"Last": true, "In": true, "On": true, "Since": true, "Recently": true, "Also": true, "And": true,
	"January": true, "February": true, "March": true, "April": true, "May": true, "June": true, "July": true,
	"August": true, "September": true, "October": true, "November": true, "December": true,
}

// knownHere is the offer to add to the notice that the owner is in zone's
// city now, and done, to call once that notice has gone out. Both are empty
// when there is nothing to offer: no match, said already this trip, home,
// turned off.
func (h *Heartbeat) knownHere(ctx context.Context, prev, zone string) (string, func()) {
	if !isCity(zone) {
		return "", nil
	}
	if off, _ := h.store.Get(ctx, TravelPeopleOffKey); off == "1" {
		return "", nil
	}
	home, _ := h.store.Get(ctx, travelHomeKey)
	if home == "" && prev != "" {
		home = prev // the first move this twin saw left home, most likely
		_ = h.store.Set(ctx, travelHomeKey, home)
	}
	if zone == home {
		// The trip is over: the next one starts afresh.
		return "", func() { _ = h.store.Unset(ctx, travelTripKey) }
	}
	trip := h.tripZones(ctx)
	for _, z := range trip {
		if z == zone {
			return "", nil // this trip has had its offer here (or had nothing)
		}
	}
	weighed := func() {
		if b, err := json.Marshal(append(trip, zone)); err == nil {
			_ = h.store.Set(ctx, travelTripKey, string(b))
		}
	}
	city := place(zone)
	offer, ok, lives := h.offerFor(ctx, zone, city)
	if lives {
		// The owner said they live here: this is home, whatever the first
		// move suggested, and any trip ends here.
		return "", func() {
			_ = h.store.Set(ctx, travelHomeKey, zone)
			_ = h.store.Unset(ctx, travelTripKey)
		}
	}
	if !ok {
		// Nothing now; a fact learned later this trip may still be offered
		// at the next move, not this one.
		return "", weighed
	}
	return " " + offer.Ask, func() {
		weighed()
		offer.At = h.now()
		if b, err := json.Marshal(offer); err == nil {
			_ = h.store.Set(ctx, TravelOfferKey, string(b))
		}
	}
}

// offerFor finds the one thing to offer in city: a person the owner said
// lives or moved there, else something they said they'd like to do there.
// lives is true when the owner said they live in city themselves.
func (h *Heartbeat) offerFor(ctx context.Context, zone, city string) (offer TravelOffer, ok, lives bool) {
	names := append([]string{city}, cityNames[city]...)
	seen := map[int64]bool{}
	var facts []memory.Fact
	for _, n := range names {
		fs, err := h.store.Recall(ctx, n, 50)
		if err != nil {
			return TravelOffer{}, false, false
		}
		for _, f := range fs {
			if !seen[f.ID] {
				seen[f.ID] = true
				facts = append(facts, f)
			}
		}
	}
	for _, f := range facts {
		for _, n := range names {
			if livesThere(f.Content, n) {
				return TravelOffer{}, false, true // the owner lives there: no welcome needed
			}
		}
	}
	hedge := multiCity[zone]
	var thing *TravelOffer
	for _, f := range facts {
		if memskill.Sensitive(f.Subject) || privateSubjects[strings.ToLower(strings.TrimSpace(f.Subject))] || private.MatchString(f.Content) {
			continue
		}
		for _, n := range names {
			if who, verb, ok := personIn(f.Content, n); ok {
				ask := fmt.Sprintf("You told me %s %s here. Want a nudge tomorrow to see if %s's free for coffee?", who, verb, who)
				if hedge {
					ask = fmt.Sprintf("You told me %s %s %s. If you're nearby, want a nudge tomorrow to see if %s's free for coffee?", who, verb, inOrTo(verb, n), who)
				}
				return TravelOffer{Ask: ask, Who: who, Zone: zone,
					Remind: fmt.Sprintf("See if %s's free for coffee while you're in %s", who, city)}, true, false
			}
			if thing == nil {
				if what, ok := wishIn(f.Content, n); ok {
					thing = &TravelOffer{Zone: zone, Remind: "You'd like to " + what,
						Ask: fmt.Sprintf("You told me you'd like to %s. Want a nudge tomorrow about it?", what)}
				}
			}
		}
	}
	if thing != nil {
		return *thing, true, false
	}
	return TravelOffer{}, false, false
}

// tripZones are the zones weighed for an offer since the owner was last
// home: a city is offered for once a trip, however often the clock goes
// back and forth (a border, or Paris, Rome, Paris).
func (h *Heartbeat) tripZones(ctx context.Context) []string {
	raw, _ := h.store.Get(ctx, travelTripKey)
	var zs []string
	if raw != "" && json.Unmarshal([]byte(raw), &zs) != nil {
		zs = nil
	}
	return zs
}

var (
	nameRe = `((?:[A-Z][\p{L}'’-]+)(?: [A-Z][\p{L}'’-]+)?)`
	verbRe = `(?:has |just |recently |now )*(moved to|relocated to|lives in|is living in|works in|is working in|studies in|is studying in)`
	wishRe = regexp.MustCompile(`(?i)^(?:the )?(?:user|owner|i)?\s*(?:wants|would like|'d like|’d like|hopes|plans|is hoping|is planning) to ((?:visit|see|try|eat|drink|go to|walk|climb|explore|tour|hike|swim|check out|stay at|ride|watch|hear|taste|shop at) .+)$`)
	ownRe  = `(?i)^(?:the )?(?:user|owner|i)\s+(?:lives?|is based|has lived|moved) (?:in|to) `
)

// personIn finds "Dan moved to Berlin" in a fact: the person, and what they
// did there in the past or present ("moved", "lives").
func personIn(content, city string) (who, verb string, ok bool) {
	re := regexp.MustCompile(nameRe + ` ` + verbRe + ` (?:central |downtown )?` + regexp.QuoteMeta(city) + `\b`)
	for _, m := range re.FindAllStringSubmatch(content, -1) {
		name := m[1]
		if f := strings.Fields(name); len(f) > 0 && notAName[f[0]] {
			if len(f) == 1 || notAName[f[1]] {
				continue
			}
			name = f[1] // "My Dan" is not a thing; "Friend Dan" is Dan
		}
		if strings.Contains(city, name) {
			continue
		}
		v := strings.Fields(m[2])
		return name, strings.Join(v[:len(v)-1], " "), true
	}
	return "", "", false
}

// inOrTo is where verb puts someone: moved to, lives in.
func inOrTo(verb, city string) string {
	if strings.HasPrefix(verb, "moved") || strings.HasPrefix(verb, "relocated") {
		return "to " + city
	}
	return "in " + city
}

// wishIn finds "wants to visit the Prado in Madrid": what, if it names city.
func wishIn(content, city string) (string, bool) {
	m := wishRe.FindStringSubmatch(strings.TrimSpace(content))
	if m == nil || !regexp.MustCompile(`\b`+regexp.QuoteMeta(city)+`\b`).MatchString(m[1]) {
		return "", false
	}
	what := strings.TrimRight(strings.TrimSpace(m[1]), ".!")
	if len([]rune(what)) > 80 {
		return "", false // too long to say back in a line
	}
	return what, true
}

// livesThere reports whether a fact says the owner lives in city.
func livesThere(content, city string) bool {
	return regexp.MustCompile(ownRe + regexp.QuoteMeta(city) + `\b`).MatchString(strings.TrimSpace(content))
}

// offerWaitsFor is how long an offer kept back for quiet hours may still
// go out: a day on, the welcome is stale.
const offerWaitsFor = 24 * time.Hour

// waitingOffer is a travel offer kept back while the owner's quiet hours
// last. The bare notice goes out at once; the offer follows when they end.
type waitingOffer struct {
	zone  string
	ask   string
	done  func()
	since time.Time
}

// waitOutQuiet passes on the offer for the notice that the owner is in
// zone or, in their quiet hours, keeps it back for offerAfterQuiet and
// passes on nothing: a named, personal offer can wait for the morning.
func (h *Heartbeat) waitOutQuiet(zone, offer string, done func()) (string, func()) {
	quiet := offer != "" && h.Quiet != nil && h.Quiet(h.now())
	h.mu.Lock()
	defer h.mu.Unlock()
	h.offerWaits = nil // a new move: what waited for the last one is moot
	if !quiet {
		return offer, done
	}
	h.offerWaits = &waitingOffer{zone: zone, ask: strings.TrimSpace(offer), done: done, since: h.now()}
	return "", nil
}

// offerAfterQuiet sends the offer kept back for quiet hours once they are
// over, while the owner is still in its zone. It is used up only once it
// has gone out, so a channel that was down hears it at a later look.
func (h *Heartbeat) offerAfterQuiet(ctx context.Context, zone string) {
	h.mu.Lock()
	w := h.offerWaits
	if w != nil && (w.zone != zone || h.now().Sub(w.since) > offerWaitsFor) {
		h.offerWaits, w = nil, nil
	}
	h.mu.Unlock()
	if w == nil || h.paused.Load() || (h.Quiet != nil && h.Quiet(h.now())) {
		return
	}
	owner := h.owner()
	if owner == "" {
		return
	}
	if err := h.sendWithin(ctx, owner, fmt.Sprintf("You're in %s now. %s", place(zone), w.ask)); err != nil {
		h.log.Warn("travel offer not sent; will try again", "err", err)
		return
	}
	h.mu.Lock()
	if h.offerWaits == w {
		h.offerWaits = nil
	}
	h.mu.Unlock()
	if w.done != nil {
		w.done()
	}
}
