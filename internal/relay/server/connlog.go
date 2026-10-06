package server

import (
	"net/netip"
	"sync"
	"time"
)

// Retention of the connection log: raw records for 72 hours, then only
// hourly per-handle totals, for 30 days. Nothing here reaches a log file;
// the operator reads it over SSH from the metrics listener.
const (
	rawKeep    = 72 * time.Hour
	hourlyKeep = 30 * 24 * time.Hour
)

// connRecord is one spliced connection. The payload is never recorded,
// and the client only as its /24 or /48.
type connRecord struct {
	At       int64        `json:"at"` // unix seconds, when it ended
	Handle   string       `json:"handle"`
	Client   netip.Prefix `json:"client"`
	ToDaemon int64        `json:"to_daemon"` // bytes
	ToClient int64        `json:"to_client"`
	Millis   int64        `json:"duration_ms"`
}

type hourKey struct {
	hour   int64 // unix seconds, truncated to the hour
	handle string
}

type hourTotal struct {
	Hour     int64  `json:"hour"`
	Handle   string `json:"handle"`
	Conns    int64  `json:"conns"`
	ToDaemon int64  `json:"to_daemon"`
	ToClient int64  `json:"to_client"`
}

// connLog is a bounded in-memory log: a ring of raw records and a table of
// hourly totals, each dropping its oldest entries first.
type connLog struct {
	mu        sync.Mutex
	max       int
	ring      []connRecord // grows to max, then wraps
	next      int          // where the next record goes
	n         int          // records held
	maxHourly int
	hourly    map[hourKey]*hourTotal
	order     []hourKey // insertion order, oldest first
}

func newConnLog(c ConnLog) *connLog {
	return &connLog{max: c.Entries, maxHourly: c.HourlyEntries, hourly: map[hourKey]*hourTotal{}}
}

func (l *connLog) add(r connRecord) {
	if l.max == 0 && l.maxHourly == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 {
		if len(l.ring) < l.max {
			l.ring = append(l.ring, r)
		} else {
			l.ring[l.next] = r
		}
		l.next = (l.next + 1) % l.max
		l.n = min(l.n+1, l.max)
	}
	if l.maxHourly > 0 {
		k := hourKey{r.At - r.At%3600, r.Handle}
		t := l.hourly[k]
		if t == nil {
			t = &hourTotal{Hour: k.hour, Handle: k.handle}
			l.hourly[k] = t
			l.order = append(l.order, k)
		}
		t.Conns++
		t.ToDaemon += r.ToDaemon
		t.ToClient += r.ToClient
		for len(l.hourly) > l.maxHourly {
			l.dropOldestHour()
		}
	}
}

func (l *connLog) dropOldestHour() {
	delete(l.hourly, l.order[0])
	l.order[0] = hourKey{}
	l.order = l.order[1:]
}

// prune enforces the retention periods.
func (l *connLog) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rawCut, hourCut := now.Add(-rawKeep).Unix(), now.Add(-hourlyKeep).Unix()
	for l.n > 0 {
		i := (l.next - l.n + len(l.ring)) % len(l.ring)
		if l.ring[i].At >= rawCut {
			break
		}
		l.ring[i] = connRecord{}
		l.n--
	}
	for len(l.order) > 0 && l.order[0].hour < hourCut {
		l.dropOldestHour()
	}
	if cap(l.order) > 2*len(l.order)+1024 {
		l.order = append([]hourKey(nil), l.order...)
	}
}

// read returns the records and totals for handle, or for every handle
// when it is empty, oldest first.
func (l *connLog) read(handle string) ([]connRecord, []hourTotal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var recs []connRecord
	for j := l.n; j > 0; j-- {
		r := l.ring[(l.next-j+len(l.ring))%len(l.ring)]
		if handle == "" || r.Handle == handle {
			recs = append(recs, r)
		}
	}
	var hours []hourTotal
	for _, k := range l.order {
		if handle == "" || k.handle == handle {
			hours = append(hours, *l.hourly[k])
		}
	}
	return recs, hours
}
