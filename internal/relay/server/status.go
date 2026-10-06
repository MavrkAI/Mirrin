package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// statusPerMin bounds status requests per address.
const statusPerMin = 60

// statusKeep is how long an offline daemon's last_seen is kept: as long
// as an entitlement can last.
const statusKeep = 35 * 24 * time.Hour

// statusStore answers GET /v1/status/<name>?k=: whether a daemon is
// tunnelled here and since when, or when it was last seen. The name is a
// hosted handle or any hostname a tunnel carries. The relay keeps only
// SHA-256 of the status key, from the hello, and a wrong key gets the same
// 404 as a name it has never seen.
type statusStore struct {
	mu sync.Mutex
	m  map[string]*statusEntry
}

type statusEntry struct {
	hash  [32]byte
	owner *Tunnel   // the tunnel holding the name; nil when offline
	at    time.Time // online: since; offline: last seen
}

func newStatusStore() *statusStore { return &statusStore{m: map[string]*statusEntry{}} }

// statusNames are the names a tunnel's status answers to.
func statusNames(t *Tunnel) []string {
	if t.Handle != "" && t.Handle != t.Hostnames[0] {
		return append([]string{t.Handle}, t.Hostnames...)
	}
	return t.Hostnames
}

// up records t as online for its names, under its status key.
func (s *statusStore) up(t *Tunnel, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range statusNames(t) {
		s.m[n] = &statusEntry{hash: [32]byte(t.StatusKeyHash), owner: t, at: now}
	}
}

// down records t as gone from every name it still owns.
func (s *statusStore) down(t *Tunnel, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range statusNames(t) {
		if e := s.m[n]; e != nil && e.owner == t {
			e.owner, e.at = nil, now
		}
	}
}

// lookup answers for name if k is its status key.
func (s *statusStore) lookup(name string, k []byte) (online bool, at time.Time, ok bool) {
	h := sha256.Sum256(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[name]
	if e == nil || subtle.ConstantTimeCompare(h[:], e.hash[:]) != 1 {
		return false, time.Time{}, false
	}
	return e.owner != nil, e.at, true
}

// prune forgets daemons offline for longer than statusKeep.
func (s *statusStore) prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n, e := range s.m {
		if e.owner == nil && now.Sub(e.at) > statusKeep {
			delete(s.m, n)
		}
	}
}

type statusJSON struct {
	Online   bool   `json:"online"`
	Since    string `json:"since,omitzero"`
	LastSeen string `json:"last_seen,omitzero"`
}

var statusKeyEncoding = base64.RawURLEncoding.Strict()

// serveStatus is GET /v1/status/{handle}?k=<b64url 32 bytes>. Any origin
// may ask: the answer is only for whoever holds the key.
func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json")
	if !s.statusRate.allow(remoteAddr(r.RemoteAddr).Addr(), s.now()) {
		h.Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate_limited"}`))
		return
	}
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not_found"}`))
	}
	name := lowerASCII(r.PathValue("handle"))
	ks := r.URL.Query().Get("k")
	if !wire.ValidHostname(name) || len(ks) != statusKeyEncoding.EncodedLen(wire.StatusKeySize) {
		notFound()
		return
	}
	k, err := statusKeyEncoding.DecodeString(ks)
	if err != nil || len(k) != wire.StatusKeySize {
		notFound()
		return
	}
	online, at, ok := s.status.lookup(name, k)
	if !ok {
		notFound()
		return
	}
	resp := statusJSON{Online: online}
	if online {
		resp.Since = at.UTC().Format(time.RFC3339)
	} else {
		resp.LastSeen = at.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(resp)
	w.Write(b)
}
