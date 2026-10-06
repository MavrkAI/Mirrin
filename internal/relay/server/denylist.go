package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// A hosted relay polls the deny list every denyEvery, and one refresh,
// primary and mirror together, takes at most denyTimeout: the primary gets
// denyPrimaryShare of it when there is a mirror, and the mirror what is
// left. So a key or handle is refused within denyEvery+denyTimeout (55 s)
// of the list changing, even while the primary hangs.
const (
	denyEvery        = 45 * time.Second
	denyTimeout      = 10 * time.Second
	denyPrimaryShare = 0.6
)

// denyCacheFile, in state_dir, keeps the last good list across restarts,
// so a relay that restarts while the control plane is down still refuses
// what it refused before.
const denyCacheFile = "denylist.paseto"

// denyState is the deny list in force: the highest seq that verified.
type denyState struct {
	mu      sync.RWMutex
	seq     int64
	handles map[string]string // handle → why
	keys    map[string]string // EncodeKey(key) → why
}

// apply installs l if its seq is higher than the list in force, and
// reports whether it did. A lower or equal seq is stale and ignored.
func (d *denyState) apply(l entitle.DenyList) bool {
	handles := make(map[string]string, len(l.Handles))
	for _, e := range l.Handles {
		handles[e.Value] = e.Why
	}
	keys := make(map[string]string, len(l.Keys))
	for _, e := range l.Keys {
		keys[e.Value] = e.Why
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if l.Seq <= d.seq {
		return false
	}
	d.seq, d.handles, d.keys = l.Seq, handles, keys
	return true
}

// denies reports whether handle or key is on the list, and why.
func (d *denyState) denies(handle string, key ed25519.PublicKey) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if why, ok := d.handles[handle]; ok && handle != "" {
		return why, true
	}
	why, ok := d.keys[entitle.EncodeKey(key)]
	return why, ok
}

// pollDenyList keeps the deny list current: the cached list first, then
// denylist_url every denyEvery, with mirror_url when that fails.
func (s *Server) pollDenyList() {
	defer s.wg.Done()
	s.loadCachedDenyList()
	every := s.opts.denyEvery
	if every <= 0 {
		every = denyEvery
	}
	ticks := s.opts.denyTicks
	if ticks == nil {
		t := time.NewTicker(every)
		defer t.Stop()
		ticks = t.C
	}
	for {
		s.refreshDenyList()
		if s.opts.denyRefreshed != nil {
			s.opts.denyRefreshed()
		}
		select {
		case <-ticks:
		case <-s.ctx.Done():
			return
		}
	}
}

// refreshDenyList fetches, verifies and applies the list once, within
// s.denyTimeout.
func (s *Server) refreshDenyList() {
	ctx, cancel := context.WithTimeout(s.ctx, s.denyTimeout)
	defer cancel()
	urls := []string{s.cfg.DenylistURL}
	if s.cfg.MirrorURL != "" {
		urls = append(urls, s.cfg.MirrorURL)
	}
	for i, u := range urls {
		fctx, fcancel := ctx, context.CancelFunc(func() {})
		if i == 0 && len(urls) > 1 {
			fctx, fcancel = context.WithTimeout(ctx, time.Duration(float64(s.denyTimeout)*denyPrimaryShare))
		}
		tok, err := s.fetchDenyList(fctx, u)
		fcancel()
		var l entitle.DenyList
		if err == nil {
			l, err = entitle.VerifyDenyList(tok, s.pol.dlKeys, s.now())
		}
		if err != nil {
			s.m.denyErrors.Add(1)
			s.log.Warn("relay: deny list", "url", u, "err", err)
			continue
		}
		s.m.denyAt.Store(s.now().Unix())
		if s.installDenyList(l) {
			s.saveDenyList(tok)
		}
		return
	}
}

// installDenyList applies l and, if it is new, ends every tunnel it
// denies.
func (s *Server) installDenyList(l entitle.DenyList) bool {
	if !s.deny.apply(l) {
		return false
	}
	s.m.denySeq.Store(l.Seq)
	s.log.Info("relay: deny list", "seq", l.Seq, "handles", len(l.Handles), "keys", len(l.Keys))
	s.mu.Lock()
	defer s.mu.Unlock()
	for t := range s.tunnels {
		if why, denied := s.deny.denies(t.Handle, t.Key); denied {
			t.kick(wire.Control{T: wire.ControlNotice, Message: deniedMessage(why)})
		}
	}
	return true
}

func (s *Server) fetchDenyList(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "mirrin-relay/"+s.version)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const max = entitle.MaxDenyListSize + 4096
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", err
	}
	if len(b) > max {
		return "", errors.New("deny list too large")
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *Server) loadCachedDenyList() {
	if s.cfg.StateDir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.StateDir, denyCacheFile))
	if err != nil || len(b) > entitle.MaxDenyListSize+4096 {
		return
	}
	l, err := entitle.VerifyDenyList(strings.TrimSpace(string(b)), s.pol.dlKeys, s.now())
	if err != nil {
		s.log.Warn("relay: cached deny list", "err", err)
		return
	}
	s.installDenyList(l)
}

func (s *Server) saveDenyList(tok string) {
	if s.cfg.StateDir == "" {
		return
	}
	p := filepath.Join(s.cfg.StateDir, denyCacheFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		s.log.Warn("relay: saving deny list", "err", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		s.log.Warn("relay: saving deny list", "err", err)
	}
}

// defaultHTTPClient fetches the deny list: a bounded time, and redirects
// only to https.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || len(via) >= 3 {
				return errors.New("relay: refusing redirect")
			}
			return nil
		},
	}
}
