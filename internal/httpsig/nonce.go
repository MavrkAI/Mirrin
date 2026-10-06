package httpsig

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrReplay means the nonce was already used by that key.
	ErrReplay = errors.New("httpsig: nonce already used")
	// ErrNonceCacheFull means the cache refused a new nonce; the request is
	// refused rather than let through unchecked.
	ErrNonceCacheFull = errors.New("httpsig: nonce cache full")
	// ErrNonceKeyFull means one key holds as many live nonces as a key may.
	// It wraps ErrNonceCacheFull.
	ErrNonceKeyFull = fmt.Errorf("%w for this key", ErrNonceCacheFull)
)

// NonceCache is the server's memory of nonces, which is what makes a
// signature single-use. Verify calls Use only for requests that are
// otherwise valid, so only holders of a key that lookup accepts can fill
// it. A lookup must therefore accept registered keys only: anyone can mint
// a key, so one that accepts self-asserted keys lets anyone fill the cache.
// MemoryNonceCache's per-key limit stops one key crowding out the rest, not
// a crowd of new keys.
type NonceCache interface {
	// Use records that keyID used nonce, and refuses the pair again with
	// ErrReplay through until, inclusive. Verify accepts the signature at
	// every instant up to and including until, so an entry dropped early, or
	// stored rounded down, reopens replay; a cache that keeps coarser times
	// rounds until up. now is the caller's clock. Use must be safe for
	// concurrent use.
	Use(keyID, nonce string, now, until time.Time) error
}

// MemoryNonceCache is a NonceCache for a single server process. A restart
// forgets it, which reopens replay only for signatures still inside their
// window (ten minutes at most).
type MemoryNonceCache struct {
	mu     sync.Mutex
	max    int
	perKey int
	m      map[nonceKey]time.Time // until
	n      map[string]int         // entries per key
	swept  time.Time              // entries with an until before this may be gone
}

type nonceKey struct{ keyID, nonce string }

// NewMemoryNonceCache holds at most max nonces in all and perKey for any one
// key; 0 means 100,000 and 1,000. At the control plane's ten-minute window,
// 1,000 is a request every 0.6 s, sustained, from one device.
func NewMemoryNonceCache(max, perKey int) *MemoryNonceCache {
	if max <= 0 {
		max = 100_000
	}
	if perKey <= 0 {
		perKey = 1_000
	}
	return &MemoryNonceCache{max: max, perKey: perKey, m: map[nonceKey]time.Time{}, n: map[string]int{}}
}

// Use implements NonceCache.
func (c *MemoryNonceCache) Use(keyID, nonce string, now, until time.Time) error {
	k := nonceKey{keyID, nonce}
	c.mu.Lock()
	defer c.mu.Unlock()
	full := len(c.m) >= c.max || c.n[keyID] >= c.perKey
	if since := now.Sub(c.swept); since > time.Minute || full && since > time.Second {
		c.sweep(now)
	}
	// A sweep may already have dropped this signature's entry, if it was
	// used before. That happens only when now is stale (a slow request whose
	// clock was read long before it got here), so refuse it as late.
	if until.Before(c.swept) {
		return ErrStale
	}
	t, ok := c.m[k]
	if ok && !now.After(t) {
		return ErrReplay
	}
	if !ok {
		if len(c.m) >= c.max {
			return ErrNonceCacheFull
		}
		if c.n[keyID] >= c.perKey {
			return ErrNonceKeyFull
		}
		c.n[keyID]++
	}
	c.m[k] = until
	return nil
}

// sweep drops the entries that have passed their until time.
func (c *MemoryNonceCache) sweep(now time.Time) {
	for k, t := range c.m {
		if now.After(t) {
			delete(c.m, k)
			c.n[k.keyID]--
			if c.n[k.keyID] <= 0 {
				delete(c.n, k.keyID)
			}
		}
	}
	c.swept = now
}

// Len is the number of nonces held, expired ones included until swept.
func (c *MemoryNonceCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
