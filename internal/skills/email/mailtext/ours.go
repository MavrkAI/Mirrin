package mailtext

import (
	"strings"
	"sync"
)

// oursMax is how many Message-IDs of mail composed here are remembered.
const oursMax = 512

var ours struct {
	sync.Mutex
	ids  map[string]bool
	ring []string
}

// remember notes a Message-ID this process composed.
func remember(id string) {
	id = bare(id)
	ours.Lock()
	defer ours.Unlock()
	if ours.ids == nil {
		ours.ids = map[string]bool{}
	}
	if ours.ids[id] {
		return
	}
	if len(ours.ring) >= oursMax {
		delete(ours.ids, ours.ring[0])
		ours.ring = ours.ring[1:]
	}
	ours.ids[id] = true
	ours.ring = append(ours.ring, id)
}

// Ours reports whether the twin composed the message with this Message-ID
// (with or without angle brackets) since it started: mail it sent its own
// mailbox is its own words coming back, not the owner writing. The IDs are
// kept only in memory: a reply the twin sent in the ten minutes before a
// restart, when the owner's address is the mailbox's own, can pass SelfSent
// afterwards and be answered once. That is accepted: it is one extra reply
// in the owner's own thread, not mail from anyone else taken as theirs.
func Ours(id string) bool {
	id = bare(id)
	if id == "" {
		return false
	}
	ours.Lock()
	defer ours.Unlock()
	return ours.ids[id]
}

func bare(id string) string { return strings.Trim(strings.TrimSpace(id), "<>") }
