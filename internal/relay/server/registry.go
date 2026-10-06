package server

import (
	"crypto/ed25519"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// Tunnel is one daemon's authenticated tunnel: the names it carries, the
// rank that decides who holds them, and the session the relay opens their
// client streams on.
type Tunnel struct {
	Hostnames     []string          // lower-case names, sorted
	Handle        string            // hosted: the entitlement's handle; self-host: the first hostname
	Gen           int64             // handle generation; 0 in self-host mode
	Iat           time.Time         // the entitlement's iat; in self-host mode, when the tunnel came up
	Key           ed25519.PublicKey // the device key the hello proved
	StatusKeyHash []byte            // SHA-256 of the daemon's status key

	daemon  netip.Addr // for logs, which only ever show its /24 or /48
	exp     time.Time  // when the entitlement lapses; zero in self-host mode
	sess    *yamux.Session
	ready   chan struct{}     // closed once sess is set
	done    chan struct{}     // closed once the tunnel has ended
	end     chan wire.Control // the first message sent here ends the tunnel
	drained chan struct{}     // closed once a drain has been sent
	heard   atomic.Int64      // unix nanoseconds the relay last read from the daemon
	lost    bool              // the daemon stopped answering keepalives; runTunnel's goroutine only
	live    atomic.Int64      // open client streams
	limit   int64
	bucket  *bucket
}

func newTunnel() *Tunnel {
	return &Tunnel{ready: make(chan struct{}), done: make(chan struct{}), end: make(chan wire.Control, 1), drained: make(chan struct{})}
}

// kick asks the tunnel to end, telling the daemon why with c. Only the
// first kick counts.
func (t *Tunnel) kick(c wire.Control) {
	select {
	case t.end <- c:
	default:
	}
}

// acquire takes a place for one more client stream.
func (t *Tunnel) acquire() bool {
	if t.live.Add(1) > t.limit {
		t.live.Add(-1)
		return false
	}
	return true
}

func (t *Tunnel) release() { t.live.Add(-1) }

// Refusals from Registry.Attach.
var (
	// ErrSuperseded means a tunnel with a higher generation holds a name.
	ErrSuperseded = errors.New("relay: a higher generation holds the name")
	// ErrSupersededRetry means a tunnel with the same generation and a
	// newer entitlement holds a name.
	ErrSupersededRetry = errors.New("relay: a newer entitlement holds the name")
)

// Registry maps hostnames to the tunnel that carries them. For each name
// the tunnel with the higher (Gen, Iat) wins; a tie goes to the newcomer,
// which is how a daemon that reconnects replaces its own stale tunnel.
type Registry interface {
	// Attach makes t the carrier of all its hostnames, or of none. It
	// fails with ErrSuperseded if a holder of any of them has a higher
	// Gen, else with ErrSupersededRetry if one has the same Gen and a
	// later Iat. Otherwise it returns the tunnels t displaced; each has
	// lost every name it held, and the caller ends it. (A tunnel carries a
	// set of names, so it can displace more than one.)
	Attach(t *Tunnel) ([]*Tunnel, error)
	// Lookup returns the tunnel carrying host, a lower-case name.
	Lookup(host string) (*Tunnel, bool)
	// Detach removes t from every name it still holds.
	Detach(t *Tunnel)
}

// NewRegistry returns an empty in-memory Registry.
func NewRegistry() Registry { return &registry{hosts: map[string]*Tunnel{}} }

type registry struct {
	mu    sync.RWMutex
	hosts map[string]*Tunnel
}

func (r *registry) Attach(t *Tunnel) ([]*Tunnel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var displaced []*Tunnel
	var err error
	for _, h := range t.Hostnames {
		old, ok := r.hosts[h]
		if !ok || old == t {
			continue
		}
		switch {
		case old.Gen > t.Gen:
			return nil, ErrSuperseded
		case old.Gen == t.Gen && old.Iat.After(t.Iat):
			err = ErrSupersededRetry // unless another holder has a higher Gen
		case !slices.Contains(displaced, old):
			displaced = append(displaced, old)
		}
	}
	if err != nil {
		return nil, err
	}
	for _, old := range displaced {
		r.drop(old)
	}
	for _, h := range t.Hostnames {
		r.hosts[h] = t
	}
	return displaced, nil
}

func (r *registry) Lookup(host string) (*Tunnel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.hosts[host]
	return t, ok
}

func (r *registry) Detach(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drop(t)
}

func (r *registry) drop(t *Tunnel) {
	for _, h := range t.Hostnames {
		if r.hosts[h] == t {
			delete(r.hosts, h)
		}
	}
}
