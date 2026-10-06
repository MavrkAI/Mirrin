package backup

import (
	"fmt"
	"sort"
	"time"
)

// Retention says how many snapshots to keep: the newest of each of the last
// Daily days, then of the Weekly weeks before those, then of the Monthly
// months before those (borg's prune rules: a snapshot kept by one rule
// doesn't count towards the next). The newest snapshot this machine wrote
// and read back is never pruned.
type Retention struct {
	Daily, Weekly, Monthly int
}

// DefaultRetention keeps 7 daily, 4 weekly and 6 monthly snapshots.
var DefaultRetention = Retention{Daily: 7, Weekly: 4, Monthly: 6}

// Apply splits snapshots into those to keep and those to drop. Names that
// claim a time more than a day ahead of now are kept and never counted, so
// oddly named files can't push real snapshots out.
func (r Retention) Apply(objs []Object, newestVerified string, now time.Time) (keep, drop []Object) {
	why := r.plan(objs, newestVerified, now)
	for _, o := range objs {
		if _, ok := why[o.Name]; ok {
			keep = append(keep, o)
		} else {
			drop = append(drop, o)
		}
	}
	return keep, drop
}

// plan returns why each kept snapshot is kept: "daily", "weekly",
// "monthly" (with " (oldest)" when a rule ran short), "newest verified" or
// "unknown" for names it can't place.
func (r Retention) plan(objs []Object, newestVerified string, now time.Time) map[string]string {
	type snap struct {
		name string
		at   time.Time
	}
	kept := map[string]string{}
	var snaps []snap
	for _, o := range objs {
		kind, at, ok := parseName(o.Name)
		if !ok || kind != "snap" || at.After(now.Add(24*time.Hour)) {
			kept[o.Name] = "unknown"
			continue
		}
		snaps = append(snaps, snap{o.Name, at})
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].at.After(snaps[j].at) })
	rule := func(label string, n int, period func(time.Time) string) {
		if n <= 0 || len(snaps) == 0 {
			return
		}
		last, count := "", 0
		for _, s := range snaps {
			p := period(s.at)
			if p == last {
				continue
			}
			last = p
			if _, done := kept[s.name]; done {
				continue
			}
			kept[s.name] = label
			if count++; count == n {
				return
			}
		}
		// Fewer periods than asked for: keep the oldest too, as borg does.
		if oldest := snaps[len(snaps)-1]; kept[oldest.name] == "" {
			kept[oldest.name] = label + " (oldest)"
		}
	}
	rule("daily", r.Daily, func(t time.Time) string { return t.Format("2006-01-02") })
	rule("weekly", r.Weekly, func(t time.Time) string {
		y, w := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	})
	rule("monthly", r.Monthly, func(t time.Time) string { return t.Format("2006-01") })
	if newestVerified != "" {
		for _, o := range objs {
			if o.Name == newestVerified {
				kept[o.Name] = "newest verified"
			}
		}
	}
	return kept
}
