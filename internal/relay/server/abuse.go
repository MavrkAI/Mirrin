package server

import (
	"encoding/json/v2"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// suspensionsFile, in state_dir, keeps suspensions across restarts: a
// JSON object of handle → until. Deleting a handle from it and restarting
// lifts that suspension early.
const suspensionsFile = "suspensions.json"

const maxSuspensionsFile = 1 << 20

// abuse is the distinct-client heuristic. The relay passes TLS through, so
// connection metadata is the only abuse signal it has. A shared-zone
// phishing page is reached by a crowd; a twin is reached by its owner's
// few devices. A handle that clients from more than limit networks (/24 or
// /48) reach in one UTC day is suspended for review, and the operator is
// told. Only those networks are held, never addresses.
type abuse struct {
	limit      int // 0 is off
	suspendFor time.Duration

	saveMu    sync.Mutex // one write of suspensionsFile at a time
	mu        sync.Mutex
	day       int64                                // the UTC day nets counts
	nets      map[string]map[netip.Prefix]struct{} // handle → networks today
	suspended map[string]time.Time                 // handle → until
}

func newAbuse(c Abuse) *abuse {
	return &abuse{
		limit:      c.DistinctNetsPerDay,
		suspendFor: c.SuspendFor,
		nets:       map[string]map[netip.Prefix]struct{}{},
		suspended:  map[string]time.Time{},
	}
}

// observe records that a client in client's network reached handle. It
// reports true when this is the network that takes the handle over the
// limit, and suspends it; n is how many networks the handle has had today.
func (a *abuse) observe(handle string, client netip.Addr, now time.Time) (tripped bool, n int) {
	if a.limit == 0 {
		return false, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if d := now.UTC().Unix() / 86400; d != a.day {
		a.day = d
		clear(a.nets)
	}
	if a.suspendedLocked(handle, now) {
		return false, 0
	}
	set := a.nets[handle]
	if set == nil {
		set = map[netip.Prefix]struct{}{}
		a.nets[handle] = set
	}
	set[netOf(client)] = struct{}{}
	n = len(set)
	if n <= a.limit {
		return false, n
	}
	delete(a.nets, handle)
	a.suspended[handle] = now.Add(a.suspendFor)
	return true, n
}

// isSuspended reports whether handle is under review.
func (a *abuse) isSuspended(handle string, now time.Time) bool {
	if a.limit == 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.suspendedLocked(handle, now)
}

func (a *abuse) suspendedLocked(handle string, now time.Time) bool {
	until, ok := a.suspended[handle]
	if ok && !now.Before(until) {
		delete(a.suspended, handle)
		return false
	}
	return ok
}

// suspensions returns the suspensions in force.
func (a *abuse) suspensions(now time.Time) map[string]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := maps.Clone(a.suspended)
	maps.DeleteFunc(m, func(_ string, until time.Time) bool { return !now.Before(until) })
	return m
}

// restore installs saved suspensions that have not lapsed.
func (a *abuse) restore(m map[string]time.Time, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for h, until := range m {
		if wire.ValidHostname(h) && now.Before(until) {
			a.suspended[h] = until
		}
	}
}

// loadSuspensions reinstates the suspensions saved in state_dir.
func (s *Server) loadSuspensions() {
	if s.cfg.StateDir == "" || s.abuse.limit == 0 {
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.StateDir, suspensionsFile))
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Warn("relay: reading suspensions", "err", err)
		}
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSuspensionsFile+1))
	var m map[string]time.Time
	if err == nil && len(b) <= maxSuspensionsFile {
		err = json.Unmarshal(b, &m)
	}
	if err != nil || len(b) > maxSuspensionsFile {
		s.log.Warn("relay: reading suspensions", "err", err, "bytes", len(b))
		return
	}
	s.abuse.restore(m, s.now())
}

// saveSuspensions writes the suspensions in force to state_dir.
func (s *Server) saveSuspensions() {
	if s.cfg.StateDir == "" {
		return
	}
	s.abuse.saveMu.Lock()
	defer s.abuse.saveMu.Unlock()
	b, err := json.Marshal(s.abuse.suspensions(s.now()))
	if err != nil {
		s.log.Warn("relay: saving suspensions", "err", err)
		return
	}
	p := filepath.Join(s.cfg.StateDir, suspensionsFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		s.log.Warn("relay: saving suspensions", "err", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		s.log.Warn("relay: saving suspensions", "err", err)
	}
}
