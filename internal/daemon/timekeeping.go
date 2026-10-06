package daemon

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/skills/calendar"
	"github.com/MavrkAI/Mirrin/internal/skills/email"
	"github.com/MavrkAI/Mirrin/internal/skills/reminders"
)

// ownerJobWithin is how late a built-in job that messages the owner (the
// morning nudge, the evening note) may still run after the lid was shut
// through it; later, it waits for tomorrow rather than arrive at 01:00.
const ownerJobWithin = 3 * time.Hour

// keepTime wires the heartbeat's timekeeping into the rest of the twin. The
// time zone follows the system as the laptop travels (unless the owner
// pinned one), and what reads times in it follows too: the model's sense of
// "now", the reminders tool ("at 9" means 9 where the owner is), the
// weather's city. A pause survives a restart, and a retired model's
// stand-in can be kept with the owner's yes.
func (d *Daemon) keepTime() {
	d.beat.ZoneSetting(func() string {
		c := d.Config()
		return c.User.Timezone
	})
	d.beat.OnZone = d.zoneMoved
	d.agent.Location = d.beat.Location
	d.agent.Tools().Register(d.modelTools()...)
	d.restorePause()
}

// location is the time zone the twin keeps now: the heartbeat's, which
// follows the system as the laptop travels (or the owner's pin), else the
// one it started in.
func (d *Daemon) location() *time.Location {
	if d.beat != nil {
		return d.beat.Location()
	}
	if d.loc != nil {
		return d.loc
	}
	return time.Local
}

// zoneMoved follows a new time zone: what the owner asks for with a time
// ("remind me at 9", "book 9am tomorrow") is read in it, times the calendar
// and email tools show are in it, the presence screen's calendar reads in
// it, and the weather is fetched for its city. The watcher's calendar shows
// times in it too, from a new starting point: its snapshots are compared as
// text, and every event would read as moved. (The inboxes' snapshots carry
// no times, so they are left as they are.)
func (d *Daemon) zoneMoved(loc *time.Location) {
	c := d.Config()
	reg := d.agent.Tools()
	if _, ok := reg.Get("set_reminder"); ok {
		reg.Register(reminders.Tools(d.store, loc)...)
	}
	// One at a time with a disconnect or a switch-off taking the tools away
	// (applyGoogle): between this look and the register, that would be undone.
	d.gmu.Lock()
	if _, ok := reg.Get("create_event"); ok {
		reg.Register(calendar.New(c.Skills.Calendar, loc).Tools()...)
	}
	if d.calendar.Load() != nil {
		d.calendar.Store(calendar.New(c.Skills.Calendar, loc))
	}
	if d.watcher != nil {
		d.watcher.Rebaseline(calendar.New(c.Skills.Calendar, loc)) // only if it watches one
	}
	d.gmu.Unlock()
	if _, ok := reg.Get("list_emails"); ok {
		reg.Register(email.New(c.Skills.Email, c.EmailPassword(), loc).Tools()...)
	}
	weatherCache.Lock()
	weatherCache.w = nil
	weatherCache.Unlock()
	d.log.Info("keeping time in a new zone", "zone", loc.String())
}

// weatherURL is Open-Meteo's forecast endpoint (no key or account).
var weatherURL = "https://api.open-meteo.com/v1/forecast"

// weatherPlace is where the ambient screen's weather is for: the latitude
// and longitude in the settings, else the city the time zone is named for,
// so the screen has weather without anyone typing coordinates. place names
// that city (it's shown with the weather, since it's a guess); ok is false
// when the owner turned weather off or no place is known.
func (d *Daemon) weatherPlace(c config.Config) (lat, lon float64, place string, ok bool) {
	if c.UI.Weather != nil && !*c.UI.Weather {
		return 0, 0, "", false
	}
	if c.UI.Latitude != 0 || c.UI.Longitude != 0 {
		return c.UI.Latitude, c.UI.Longitude, "", true
	}
	zone := strings.TrimSpace(c.User.Timezone)
	if config.FollowsSystem(zone) {
		zone = ""
		if d.beat != nil {
			zone = d.beat.Location().String()
		}
		if zone == "" || zone == "Local" {
			zone = config.LocalTimezone()
		}
	}
	return zoneCoords(zone)
}

// inPlace adds the city to a weather summary guessed from the time zone.
func inPlace(summary, place string) string {
	if place == "" {
		return summary
	}
	return summary + " in " + place
}

// zoneAliases are older zone names still in use, and the zone.tab ones they
// mean.
var zoneAliases = map[string]string{
	"Asia/Calcutta": "Asia/Kolkata", "Asia/Saigon": "Asia/Ho_Chi_Minh", "Asia/Katmandu": "Asia/Kathmandu",
	"Asia/Rangoon": "Asia/Yangon", "Asia/Dacca": "Asia/Dhaka", "Asia/Thimbu": "Asia/Thimphu",
	"Asia/Ulan_Bator": "Asia/Ulaanbaatar", "Asia/Chongqing": "Asia/Shanghai", "Asia/Chungking": "Asia/Shanghai",
	"Asia/Harbin": "Asia/Shanghai", "Asia/Tel_Aviv": "Asia/Jerusalem", "Asia/Istanbul": "Europe/Istanbul",
	"Europe/Kiev": "Europe/Kyiv", "Europe/Belfast": "Europe/London", "Europe/Nicosia": "Asia/Nicosia",
	"America/Buenos_Aires": "America/Argentina/Buenos_Aires", "America/Indianapolis": "America/Indiana/Indianapolis",
	"America/Louisville": "America/Kentucky/Louisville", "America/Montreal": "America/Toronto",
	"Atlantic/Faeroe": "Atlantic/Faroe", "Pacific/Samoa": "Pacific/Pago_Pago",
	"US/Eastern": "America/New_York", "US/Central": "America/Chicago", "US/Mountain": "America/Denver",
	"US/Pacific": "America/Los_Angeles", "US/Alaska": "America/Anchorage", "US/Hawaii": "Pacific/Honolulu",
	"US/Arizona": "America/Phoenix", "Canada/Eastern": "America/Toronto", "Canada/Central": "America/Winnipeg",
	"Canada/Mountain": "America/Edmonton", "Canada/Pacific": "America/Vancouver", "Canada/Atlantic": "America/Halifax",
	"Canada/Newfoundland": "America/St_Johns", "Australia/ACT": "Australia/Sydney", "Australia/Canberra": "Australia/Sydney",
	"Australia/NSW": "Australia/Sydney", "Australia/Victoria": "Australia/Melbourne", "Australia/Queensland": "Australia/Brisbane",
	"Australia/West": "Australia/Perth", "Australia/South": "Australia/Adelaide", "Australia/North": "Australia/Darwin",
	"Australia/Tasmania": "Australia/Hobart", "Brazil/East": "America/Sao_Paulo", "Mexico/General": "America/Mexico_City",
	"GB": "Europe/London", "Eire": "Europe/Dublin", "NZ": "Pacific/Auckland", "Japan": "Asia/Tokyo",
	"Singapore": "Asia/Singapore", "Hongkong": "Asia/Hong_Kong", "PRC": "Asia/Shanghai", "ROK": "Asia/Seoul",
	"Israel": "Asia/Jerusalem", "Iran": "Asia/Tehran", "Egypt": "Africa/Cairo", "Turkey": "Europe/Istanbul",
	"Poland": "Europe/Warsaw", "Portugal": "Europe/Lisbon",
}

var (
	citiesOnce sync.Once
	cities     map[string][2]float64
)

// zoneCoords is where a time zone's city is, and its name.
func zoneCoords(zone string) (lat, lon float64, place string, ok bool) {
	citiesOnce.Do(func() {
		cities = map[string][2]float64{}
		for _, line := range strings.Split(zoneCities, "\n") {
			f := strings.Fields(line)
			if len(f) != 3 {
				continue
			}
			la, err1 := strconv.ParseFloat(f[1], 64)
			lo, err2 := strconv.ParseFloat(f[2], 64)
			if err1 == nil && err2 == nil {
				cities[f[0]] = [2]float64{la, lo}
			}
		}
	})
	zone = strings.TrimSpace(zone)
	if a, ok := zoneAliases[zone]; ok {
		zone = a
	}
	c, ok := cities[zone]
	if !ok {
		return 0, 0, "", false
	}
	place = zone[strings.LastIndexByte(zone, '/')+1:]
	return c[0], c[1], strings.ReplaceAll(place, "_", " "), true
}
