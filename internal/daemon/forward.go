package daemon

import (
	"slices"
	"sync"
	"time"
)

// forwardFor is how long the owner can answer a forwarded approval where it
// reached them.
const forwardFor = 24 * time.Hour

// forwards remembers, per owner chat, the conversations whose messages were
// delivered there because their own channel couldn't take them. An approval
// asked in one of those conversations can then be answered where the owner
// actually saw it (answerable in approvals.go). Channels whose sender can be
// forged never count (forgeable).
type forwards struct {
	mu sync.Mutex
	to map[string]map[string]time.Time // where it arrived -> the conversation it was for -> when
}

func (f *forwards) add(to, from string) {
	if to == from {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.to == nil {
		f.to = map[string]map[string]time.Time{}
	}
	if f.to[to] == nil {
		f.to[to] = map[string]time.Time{}
	}
	f.to[to][from] = time.Now()
}

// from lists the conversations forwarded to key in the last forwardFor.
func (f *forwards) from(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k, at := range f.to[key] {
		if time.Since(at) > forwardFor {
			delete(f.to[key], k)
			continue
		}
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
