package server

import (
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limits (docs/cloud-api.md §4, 429 rate_limited). Every bucket lives in
// memory only; an address is held only while its bucket is not full, and is
// never written anywhere.
const (
	// preAuthPerMinute bounds what one client sends to the link routes
	// before its signature is checked. A daemon polls a link every 2 s while
	// its owner pays, 30 times a minute.
	preAuthPerMinute = 120
	// A device's nonces and everyone else's are kept apart, so keys anyone
	// can mint cannot fill the cache devices need. A key that is not a
	// device's only starts and polls links: 300 nonces in ten minutes.
	otherNonces       = 50_000
	otherNoncesPerKey = 400
	// maxForwarded is how many X-Forwarded-For entries are read, from the
	// right.
	maxForwarded = 16
)

// limiter is a token bucket per key: perMin tokens, refilled at perMin a
// minute. A zero limit allows everything.
type limiter[K comparable] struct {
	perMin int
	mu     sync.Mutex
	b      map[K]*bucket
	swept  time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter[K comparable](perMin int) *limiter[K] {
	return &limiter[K]{perMin: perMin, b: map[K]*bucket{}}
}

// allow takes a token for k.
func (l *limiter[K]) allow(k K, now time.Time) bool {
	if l.perMin <= 0 {
		return true
	}
	rate := float64(l.perMin) / 60
	burst := float64(l.perMin)
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > time.Minute {
		for k, b := range l.b {
			if b.tokens+now.Sub(b.at).Seconds()*rate >= burst {
				delete(l.b, k)
			}
		}
		l.swept = now
	}
	b, ok := l.b[k]
	if !ok {
		b = &bucket{tokens: burst, at: now}
		l.b[k] = b
	}
	b.tokens = min(burst, b.tokens+max(0, now.Sub(b.at).Seconds())*rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// retryAfter is how long until the bucket has a token again, in whole
// seconds.
func (l *limiter[K]) retryAfter() int {
	if l.perMin <= 0 {
		return 1
	}
	return max(1, (60+l.perMin-1)/l.perMin)
}

// clientAddr is the address a request came from: the peer's, or, when the
// peer is a trusted proxy, the last address in X-Forwarded-For that is not
// one (proxies append, so what a client wrote itself is further left). It
// is invalid when the peer's address does not parse.
func (s *Server) clientAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	peer := ap.Addr().Unmap().WithZone("")
	if !s.trustedProxy(peer) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0 && i >= len(hops)-maxForwarded; i-- {
		h := strings.TrimSpace(hops[i])
		a, err := netip.ParseAddr(h)
		if err != nil {
			hp, perr := netip.ParseAddrPort(h)
			if perr != nil {
				return peer // the proxy wrote something else: count it as the proxy's
			}
			a = hp.Addr()
		}
		if a = a.Unmap().WithZone(""); !s.trustedProxy(a) {
			return a
		}
	}
	return peer
}

func (s *Server) trustedProxy(a netip.Addr) bool {
	for _, p := range s.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientPrefix is what one client holds: an IPv4 address, or an IPv6 /48,
// the most one site is usually given. An invalid address is one prefix.
func clientPrefix(a netip.Addr) netip.Prefix {
	if !a.IsValid() {
		return netip.Prefix{}
	}
	a = a.Unmap().WithZone("")
	bits := 32
	if a.Is6() {
		bits = 48
	}
	p, _ := a.Prefix(bits)
	return p
}

// tooMany answers 429 rate_limited.
func tooMany(w http.ResponseWriter, after int, message string) {
	w.Header().Set("Retry-After", strconv.Itoa(after))
	reply(w, http.StatusTooManyRequests, map[string]any{"error": "rate_limited", "message": message, "retry_after": after})
}

// limitPreAuth refuses a client that sends the link routes more than linking
// takes, before its signature is checked: these are the routes a key nobody
// knows may call.
func (s *Server) limitPreAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.preAuth.allow(clientPrefix(s.clientAddr(r)), s.now()) {
			tooMany(w, s.preAuth.retryAfter(), "Too many requests from here; try again shortly.")
			return
		}
		next(w, r)
	}
}
