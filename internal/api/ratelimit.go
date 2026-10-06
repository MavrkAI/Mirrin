package api

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Limits controls remote requests per IP. Zero values use conservative defaults.
type Limits struct {
	UnauthenticatedPerMinute float64
	UnauthenticatedBurst     float64
	AuthenticatedPerMinute   float64
	AuthenticatedBurst       float64
}

func (l Limits) buckets() (*remoteLimiter, *remoteLimiter) {
	if l.UnauthenticatedPerMinute <= 0 {
		l.UnauthenticatedPerMinute = 10
	}
	if l.UnauthenticatedBurst <= 0 {
		l.UnauthenticatedBurst = 10
	}
	if l.AuthenticatedPerMinute <= 0 {
		l.AuthenticatedPerMinute = 600
	}
	if l.AuthenticatedBurst <= 0 {
		l.AuthenticatedBurst = 100
	}
	return newRemoteLimiter(l.UnauthenticatedPerMinute/60, l.UnauthenticatedBurst), newRemoteLimiter(l.AuthenticatedPerMinute/60, l.AuthenticatedBurst)
}

// Bound the table as well as the request rate: new source IPs must not grow
// memory without limit. Forwarded headers never participate in this key.
type remoteLimiter struct {
	mu          sync.Mutex
	rate, burst float64
	b           map[string]*bucket
	now         func() time.Time
}

func newRemoteLimiter(rate, burst float64) *remoteLimiter {
	return &remoteLimiter{rate: rate, burst: burst, b: make(map[string]*bucket), now: time.Now}
}
func (l *remoteLimiter) allow(addr string) (bool, time.Duration) {
	ip, _, err := net.SplitHostPort(addr)
	if err != nil {
		ip = addr
	}
	if a, e := netip.ParseAddr(ip); e == nil {
		ip = a.Unmap().String()
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.b[ip]
	if b == nil {
		if len(l.b) >= 10000 {
			for k, v := range l.b {
				if now.Sub(v.at).Seconds() >= l.burst/l.rate {
					delete(l.b, k)
				}
			}
			if len(l.b) >= 10000 {
				return false, time.Minute
			}
		}
		b = &bucket{tokens: l.burst, at: now}
		l.b[ip] = b
	}
	b.tokens = min(l.burst, b.tokens+max(0, now.Sub(b.at).Seconds())*l.rate)
	b.at = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}
