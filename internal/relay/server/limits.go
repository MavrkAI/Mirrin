package server

import (
	"net/netip"
	"sync"
	"time"
)

// bucket is a token bucket of bytes that every stream of one tunnel draws
// from, in both directions. A draw may overdraw; the drawer then waits out
// the debt, so the tunnel averages its rate.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64
	tokens float64
	last   time.Time
}

// newBucket returns a bucket for bps bits per second, or nil (no limit)
// for 0.
func newBucket(bps int64) *bucket {
	if bps <= 0 {
		return nil
	}
	rate := float64(bps) / 8
	burst := max(rate/4, 64<<10)
	return &bucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// wait takes n bytes, sleeping as long as the bucket is in debt. It
// returns false if done closes first.
func (b *bucket) wait(n int, done <-chan struct{}) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	now := time.Now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	b.tokens -= float64(n)
	var d time.Duration
	if b.tokens < 0 {
		d = time.Duration(-b.tokens / b.rate * float64(time.Second))
	}
	b.mu.Unlock()
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-done:
		return false
	}
}

// maxWindowKeys bounds a window's memory; once full it refuses new
// addresses until the minute turns.
const maxWindowKeys = 1 << 18

// window allows limit events per address per minute, in fixed windows.
type window struct {
	mu    sync.Mutex
	limit int // 0 is unlimited
	start time.Time
	n     map[netip.Prefix]int
}

func newWindow(limit int) *window { return &window{limit: limit, n: map[netip.Prefix]int{}} }

// allow counts one event for a and reports whether it is within the limit.
func (w *window) allow(a netip.Addr, now time.Time) bool {
	if w.limit == 0 {
		return true
	}
	k := addrKey(a)
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.Sub(w.start) >= time.Minute || now.Before(w.start) {
		clear(w.n)
		w.start = now
	}
	c, ok := w.n[k]
	if c >= w.limit || !ok && len(w.n) >= maxWindowKeys {
		return false
	}
	w.n[k] = c + 1
	return true
}

// addrKey is what a per-address limit counts: an IPv4 address, or the /64
// of an IPv6 one, since a single host holds a whole /64.
func addrKey(a netip.Addr) netip.Prefix {
	a = a.Unmap().WithZone("")
	bits := 64
	if a.Is4() {
		bits = 32
	}
	p, _ := a.Prefix(bits)
	return p
}

// netOf is the client network the abuse heuristic counts and the logs
// show: the /24 of an IPv4 address or the /48 of an IPv6 one. Nothing
// finer is ever logged.
func netOf(a netip.Addr) netip.Prefix {
	a = a.Unmap().WithZone("")
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, _ := a.Prefix(bits)
	return p
}

// perAddr counts what each address holds at once, such as connections
// still being peeked at. An address is forgotten when it holds nothing.
type perAddr struct {
	mu    sync.Mutex
	limit int
	n     map[netip.Prefix]int
}

func newPerAddr(limit int) *perAddr { return &perAddr{limit: limit, n: map[netip.Prefix]int{}} }

func (p *perAddr) acquire(a netip.Addr) bool {
	k := addrKey(a)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n[k] >= p.limit {
		return false
	}
	p.n[k]++
	return true
}

func (p *perAddr) release(a netip.Addr) {
	k := addrKey(a)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n[k]--; p.n[k] <= 0 {
		delete(p.n, k)
	}
}
