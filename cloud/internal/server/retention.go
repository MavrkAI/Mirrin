package server

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
)

// Retention on the service's side, the same rules the daemon prunes by
// (internal/backup/retention.go): the newest snapshot of each of the last 7
// days, then of the 4 weeks before those, then of the 6 months before
// those, a snapshot kept by one rule not counting towards the next. The
// newest snapshot stored is always kept, handover markers are never pruned,
// and a name dated more than a day ahead is kept and not counted.
const (
	keepDaily   = 7
	keepWeekly  = 4
	keepMonthly = 6
)

var snapRE = regexp.MustCompile(`^snap-(\d{8}T\d{6}Z)-[0-9a-f]{8}\.age$`)

// retentionDrops returns the snapshots retention removes from objs. keep,
// the object just stored, is never one of them: it is the newest verified.
func retentionDrops(objs []store.StoredObject, keep string, now time.Time) []string {
	type snap struct {
		name string
		at   time.Time
	}
	var snaps []snap
	kept := map[string]bool{}
	for _, o := range objs {
		m := snapRE.FindStringSubmatch(o.Name)
		if m == nil {
			continue // a handover marker
		}
		at, err := time.Parse("20060102T150405Z", m[1])
		if err != nil || at.After(now.Add(24*time.Hour)) {
			continue
		}
		snaps = append(snaps, snap{o.Name, at})
	}
	if len(snaps) == 0 {
		return nil
	}
	sort.SliceStable(snaps, func(i, j int) bool {
		if snaps[i].at.Equal(snaps[j].at) {
			return snaps[i].name > snaps[j].name
		}
		return snaps[i].at.After(snaps[j].at)
	})
	rule := func(n int, period func(time.Time) string) {
		last, count := "", 0
		for _, s := range snaps {
			p := period(s.at)
			if p == last {
				continue
			}
			last = p
			if kept[s.name] {
				continue
			}
			kept[s.name] = true
			if count++; count == n {
				return
			}
		}
		kept[snaps[len(snaps)-1].name] = true // fewer periods than asked: the oldest too
	}
	rule(keepDaily, func(t time.Time) string { return t.Format("2006-01-02") })
	rule(keepWeekly, func(t time.Time) string {
		y, w := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	})
	rule(keepMonthly, func(t time.Time) string { return t.Format("2006-01") })
	kept[snaps[0].name] = true // the newest, always (the daily rule keeps it)
	kept[keep] = true
	var drop []string
	for _, s := range snaps {
		if !kept[s.name] {
			drop = append(drop, s.name)
		}
	}
	sort.Strings(drop)
	return drop
}
