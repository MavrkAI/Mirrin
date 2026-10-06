package server

import (
	"crypto/tls"
	"sync"
	"time"
)

// certLogEvery bounds how often a failing control certificate is logged.
const certLogEvery = time.Minute

// certWatch tells the operator when the control name's certificate cannot
// be had: ACME issuance failing (DNS not pointed here yet, CAA, port 443
// unreachable, a rate limit) or a certificate source gone bad. Otherwise
// the only trace is the TLS alert a client gets. The first failure is
// logged at once, then at most one line every certLogEvery while failures
// go on, and one line when a certificate is served again. No client
// address is logged.
type certWatch struct {
	mu      sync.Mutex
	failing bool
	logged  time.Time // when the last failure line was written
	since   int       // failures since then
}

// fail records a failure. It reports whether to log it, and how many
// failures the line stands for.
func (w *certWatch) fail(now time.Time) (n int, log bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.since++
	if w.failing && now.Sub(w.logged) < certLogEvery {
		return 0, false
	}
	n = w.since
	w.failing, w.logged, w.since = true, now, 0
	return n, true
}

// ok records a success and reports whether it ends a run of failures.
func (w *certWatch) ok() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	was := w.failing
	w.failing, w.since = false, 0
	return was
}

// watchCert logs and counts what the control name's certificate source
// did for one handshake.
func (s *Server) watchCert(c *tls.Certificate, err error) {
	if err != nil {
		s.m.certErrors.Add(1)
		if n, log := s.certWatch.fail(s.now()); log {
			s.log.Error("relay: control certificate", "name", s.cfg.ControlHostname, "err", err, "failures", n)
		}
		return
	}
	if c != nil && c.Leaf != nil {
		s.m.certExpiry.Store(c.Leaf.NotAfter.Unix())
	}
	if s.certWatch.ok() {
		s.log.Info("relay: control certificate served again", "name", s.cfg.ControlHostname)
	}
}
