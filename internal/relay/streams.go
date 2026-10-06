package relay

import (
	"io"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// A yamux stream buffers up to 256 KiB that the peer sends before any
// window update, and Close only half-closes it: yamux keeps it until the
// peer closes its half too, resets it, or StreamCloseTimeout passes. So a
// stream costs memory from the moment the daemon takes it until yamux lets
// go of it, not until the daemon is done with it. These limits count it
// for that whole time.
const (
	// maxStreams caps the streams one session holds at once. A relay's
	// max_streams, from its welcome or a limits message, can lower it.
	maxStreams = 256
	// acceptBacklog is how many streams yamux queues for the daemon to
	// take; it resets any beyond that. It is yamux's default, which a
	// relay's yamux client assumes of its peer.
	acceptBacklog = 256
)

// streamLimit counts a session's data streams. At the limit the session
// takes no more streams from yamux, so new ones wait in yamux's backlog
// until one is released.
type streamLimit struct {
	mu    sync.Mutex
	live  int
	limit int
	wake  chan struct{} // one waiter: the session's accept loop
}

func newStreamLimit(announced int) *streamLimit {
	s := &streamLimit{wake: make(chan struct{}, 1)}
	s.setLimit(announced)
	return s
}

// setLimit applies a relay's max_streams, within maxStreams.
func (s *streamLimit) setLimit(announced int) {
	s.mu.Lock()
	s.limit = min(max(announced, 1), maxStreams)
	s.mu.Unlock()
	s.signal()
}

// acquire waits for room for one more stream. It reports false if done
// closes first.
func (s *streamLimit) acquire(done <-chan struct{}) bool {
	for {
		s.mu.Lock()
		if s.live < s.limit {
			s.live++
			s.mu.Unlock()
			return true
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-done:
			return false
		}
	}
}

func (s *streamLimit) release() {
	s.mu.Lock()
	s.live--
	s.mu.Unlock()
	s.signal()
}

func (s *streamLimit) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// linger waits, after the daemon has closed st, until yamux has let go of
// it: the relay closed its half or reset it, the session ended, or yamux
// reset it after closeTimeout. Bytes that arrive meanwhile are discarded,
// so they do not pile up. yamux's own timer ends the read; the deadline is
// a backstop.
func linger(st *yamux.Stream, closeTimeout time.Duration) {
	st.SetReadDeadline(time.Now().Add(2 * closeTimeout))
	io.Copy(io.Discard, st)
}
