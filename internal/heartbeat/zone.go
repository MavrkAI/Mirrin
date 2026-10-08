package heartbeat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// Zone is the time zone the twin keeps. Unless the owner pinned one in
// their settings it follows the system's, read again at every look: Go
// reads the system zone once, at start, and a laptop travels.
type Zone struct {
	mu         sync.Mutex
	loc        *time.Location
	pinned     bool          // the owner's setting names a zone
	setting    string        // that setting as last read
	settingFn  func() string // reads the setting again; nil keeps the one given
	system     func() string // the system's zone name, "" when it can't be told
	sysName    string        // the system's zone at the last look
	autoLoaded bool          // loc is the system's zone, loaded for autoFor
	autoFor    string
}

// zoneFor is the zone for the location the daemon passes: nil or
// time.Local follows the system, any other is pinned.
func zoneFor(loc *time.Location) *Zone { return newZone(loc, config.LocalTimezone) }

// newZone is zoneFor reading the system's zone with system.
func newZone(loc *time.Location, system func() string) *Zone {
	z := &Zone{system: system}
	if loc != nil && loc != time.Local {
		z.loc, z.pinned, z.setting = loc, true, loc.String()
	} else {
		z.setting = "Local"
	}
	z.sysName = z.system()
	z.load()
	return z
}

// Location is the zone to read and show times in.
func (z *Zone) Location() *time.Location {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.loc
}

func (z *Zone) setSetting(f func() string) {
	z.mu.Lock()
	z.settingFn = f
	z.mu.Unlock()
}

// load sets loc to the system's zone when not pinned (z.mu held or unshared).
func (z *Zone) load() {
	if z.pinned {
		z.autoLoaded = false
		return
	}
	if z.autoLoaded && z.autoFor == z.sysName {
		return
	}
	z.loc, z.autoLoaded, z.autoFor = time.Local, true, z.sysName
	if z.sysName != "" {
		if l, err := time.LoadLocation(z.sysName); err == nil {
			z.loc = l
		}
	}
}

// zoneChange is what a look at the zone found.
type zoneChange struct {
	changed  bool   // the zone the twin keeps is different now
	moved    bool   // the system's zone changed
	from, to string // the system's zone before and after
	loc      *time.Location
	pinned   bool
}

// refresh reads the setting and the system's zone again.
func (z *Zone) refresh() zoneChange {
	z.mu.Lock()
	fn, sys := z.settingFn, z.system
	z.mu.Unlock()
	setting := ""
	if fn != nil {
		setting = fn() // outside the lock: it may wait on the daemon's config
	}
	name := sys()

	z.mu.Lock()
	defer z.mu.Unlock()
	prev := z.loc
	if fn != nil && setting != z.setting {
		z.setting = setting
		if config.FollowsSystem(setting) {
			z.pinned = false
		} else if l, err := time.LoadLocation(strings.TrimSpace(setting)); err == nil {
			z.loc, z.pinned = l, true
		} // a name that doesn't load keeps the zone as it was; the config check says why
	}
	var c zoneChange
	if name != z.sysName {
		c.moved, c.from, c.to = true, z.sysName, name
		z.sysName = name
	}
	z.load()
	c.changed = z.loc != prev
	c.loc, c.pinned = z.loc, z.pinned
	return c
}

// systemName is the system's zone as last seen.
func (z *Zone) systemName() string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.sysName
}

const (
	zoneKey     = "time.zone.v1"      // the system's zone the owner last heard about (or the first seen)
	zoneToldKey = "time.zone.told.v1" // the system zone the owner was told a pinned zone ignores
)

// zoneRetryWait is how long a zone change that couldn't be told waits
// before it is tried again.
const zoneRetryWait = 5 * time.Minute

// followZone looks at the time zone. When the system's has changed (a
// flight, even one taken while the twin was off) the owner hears once where
// they are now and what that means for their reminders; what reads times in
// the zone (OnZone) follows it at once. A paused twin says nothing of its
// own accord: the news waits for the resume. The change counts as told only
// once the message went out, so a channel that was down hears it later.
func (h *Heartbeat) followZone(ctx context.Context) {
	c := h.zone.refresh()
	if c.changed && h.OnZone != nil {
		h.OnZone(c.loc)
	}
	cur := h.zone.systemName()
	if cur == "" {
		return
	}
	h.mu.Lock()
	first := !h.zoneSeen
	h.zoneSeen = true
	h.mu.Unlock()
	if first {
		// The zone at the last look before a restart: a move made while the
		// twin was off is news too.
		stored, _ := h.store.Get(ctx, zoneKey)
		h.mu.Lock()
		h.zoneKnown = stored
		h.mu.Unlock()
	}
	h.mu.Lock()
	known, retry, logged := h.zoneKnown, h.zoneRetry, h.zoneLogged
	h.zoneLogged = cur
	h.mu.Unlock()
	if cur == known {
		h.offerAfterQuiet(ctx, cur)
		return
	}
	if known == "" { // the first look ever: nothing to compare with
		h.zoneTold(ctx, cur, false)
		return
	}
	if logged != cur {
		h.log.Info("time zone changed", "from", known, "to", cur, "pinned", c.pinned)
		h.store.Audit(ctx, "time.zone", "", known+" → "+cur)
	}
	if h.paused.Load() || h.now().Before(retry) {
		return
	}
	owner := h.owner()
	if owner == "" {
		return // no chat to tell yet; a channel may still be connecting
	}
	var text string
	var offered func() // travelpeople.go: an offer the notice carries, kept once it's out
	if c.pinned {
		// Said once per zone the system moves to: the owner chose this.
		if told, _ := h.store.Get(ctx, zoneToldKey); told == cur || cur == c.loc.String() {
			h.zoneTold(ctx, cur, false)
			return
		}
		text = fmt.Sprintf("%s is on %s time now, but I'm keeping to %s time, as your settings say. To follow %s instead, set timezone to Local in config.yaml, then choose Restart from my menu.",
			capitalize(machine()), place(cur), place(c.loc.String()), machine())
	} else {
		text = fmt.Sprintf("You're in %s now; reminders and routines follow local time.", place(cur))
		if !isCity(cur) {
			text = fmt.Sprintf("%s is on %s time now; reminders and routines follow it.", capitalize(machine()), place(cur))
		}
		text += h.keptMoments(ctx, owner, c.loc)
		if isCity(cur) {
			var offer string
			offer, offered = h.knownHere(ctx, known, cur)
			offer, offered = h.waitOutQuiet(cur, offer, offered)
			text += offer
		}
	}
	if err := h.sendWithin(ctx, owner, text); err != nil {
		h.log.Warn("time zone change not told; will try again", "err", err)
		h.mu.Lock()
		h.zoneRetry = h.now().Add(zoneRetryWait)
		h.mu.Unlock()
		return
	}
	if offered != nil {
		offered()
	}
	h.zoneTold(ctx, cur, c.pinned)
}

// zoneTold notes that the owner knows the system's zone is name (or that
// there was nothing to tell), so it isn't said again.
func (h *Heartbeat) zoneTold(ctx context.Context, name string, pinned bool) {
	h.mu.Lock()
	h.zoneKnown, h.zoneRetry = name, time.Time{}
	h.mu.Unlock()
	_ = h.store.Set(ctx, zoneKey, name)
	if pinned {
		_ = h.store.Set(ctx, zoneToldKey, name)
	}
}

// keptMoments tells the owner that reminders already set still go off at
// the moment they were set for, and when that is here.
func (h *Heartbeat) keptMoments(ctx context.Context, owner string, loc *time.Location) string {
	all, err := h.store.AllPendingReminders(ctx, 50)
	if err != nil {
		return ""
	}
	var rs []memory.Reminder
	for _, r := range all {
		if memory.LiveKey(r.ChatKey) == owner {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return ""
	}
	now := h.now().In(loc)
	t := rs[0].DueAt.In(loc)
	at := t.Format("15:04")
	if t.YearDay() != now.YearDay() || t.Year() != now.Year() {
		at = t.Format("Mon 2 Jan 15:04")
	}
	if len(rs) == 1 {
		return fmt.Sprintf(" The reminder you'd already set keeps its moment: %q is at %s here. Ask me if you'd like it moved.", rs[0].Text, at)
	}
	return fmt.Sprintf(" The %d reminders you'd already set keep their moments; the next, %q, is at %s here. Ask me if you'd like any moved.", len(rs), rs[0].Text, at)
}

// place is a zone's city, as people say it: "America/New_York" is "New York".
func place(zone string) string {
	if i := strings.LastIndexByte(zone, '/'); i >= 0 {
		zone = zone[i+1:]
	}
	return strings.ReplaceAll(zone, "_", " ")
}

// isCity reports whether a zone is named for a place (not "UTC", "Etc/GMT+3").
func isCity(zone string) bool {
	return strings.Contains(zone, "/") && !strings.HasPrefix(zone, "Etc/")
}
