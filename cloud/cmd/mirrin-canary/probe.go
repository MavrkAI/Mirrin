package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// A probe checks, once a minute from one region, that the canary answers
// over HTTPS through each relay on its own: it dials each relay's addresses
// directly (not whatever DNS picks), verifies the canary's certificate for
// its public name, and asks /healthz, whose answer must name the relay
// dialled. It pages nobody; the watcher reads its rounds.

// keepRounds is how many rounds a probe serves: half an hour at a minute.
const keepRounds = 30

// Round is one probe round.
type Round struct {
	Region string        `json:"region"`
	At     time.Time     `json:"at"`
	Relays []RelayResult `json:"relays"`
}

// RelayResult is one relay in a round. OK is true when any of its
// addresses answered.
type RelayResult struct {
	ID       string       `json:"id"`
	OK       bool         `json:"ok"`
	Addrs    []AddrResult `json:"addrs"`
	NotAfter time.Time    `json:"not_after,omitzero"` // the canary certificate's, as seen through this relay
}

// AddrResult is one address of a relay.
type AddrResult struct {
	Addr  string `json:"addr"`
	OK    bool   `json:"ok"`
	MS    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// ok reports whether relay id answered in this round; a relay missing
// from the round did not.
func (r Round) ok(id string) bool {
	for _, x := range r.Relays {
		if x.ID == id {
			return x.OK
		}
	}
	return false
}

// Results is /v1/results.
type Results struct {
	Region string  `json:"region"`
	Rounds []Round `json:"rounds"` // oldest first
}

// prober runs rounds for one region.
type prober struct {
	cfg   ProbeConfig
	roots *x509.CertPool
	token string
	log   *slog.Logger
	// dial reaches an address; tests point it elsewhere. Nil is a
	// net.Dialer.
	dial func(ctx context.Context, addr string) (net.Conn, error)
	now  func() time.Time

	mu     sync.Mutex
	rounds []Round
}

func newProber(cfg *Config, log *slog.Logger) (*prober, error) {
	roots, err := loadRoots(cfg.Probe.Roots)
	if err != nil {
		return nil, err
	}
	p := &prober{cfg: cfg.Probe, roots: roots, log: log, now: time.Now}
	if cfg.Probe.TokenEnv != "" {
		if p.token = os.Getenv(cfg.Probe.TokenEnv); p.token == "" {
			return nil, fmt.Errorf("probe.token_env: %s is empty", cfg.Probe.TokenEnv)
		}
	}
	return p, nil
}

// round checks every relay's every address, at once, and keeps the result.
func (p *prober) round(ctx context.Context) Round {
	r := Round{Region: p.cfg.Region, At: p.now().UTC().Truncate(time.Second), Relays: make([]RelayResult, len(p.cfg.Relays))}
	var wg sync.WaitGroup
	for i, rel := range p.cfg.Relays {
		r.Relays[i] = RelayResult{ID: rel.ID, Addrs: make([]AddrResult, len(rel.Addrs))}
		for j, addr := range rel.Addrs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				notAfter, err := p.check(ctx, rel.ID, addr)
				ar := AddrResult{Addr: addr, OK: err == nil, MS: time.Since(start).Milliseconds()}
				if err != nil {
					ar.Error = err.Error()
				}
				r.Relays[i].Addrs[j] = ar
				if err == nil {
					p.mu.Lock()
					r.Relays[i].NotAfter = notAfter
					p.mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	for i := range r.Relays {
		for _, a := range r.Relays[i].Addrs {
			r.Relays[i].OK = r.Relays[i].OK || a.OK
		}
	}
	p.mu.Lock()
	p.rounds = append(p.rounds, r)
	if len(p.rounds) > keepRounds {
		p.rounds = append([]Round(nil), p.rounds[len(p.rounds)-keepRounds:]...)
	}
	p.mu.Unlock()
	return r
}

// check is one HTTPS request to the canary through one relay address.
func (p *prober) check(ctx context.Context, relayID, addr string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	dial := p.dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
	}
	raw, err := dial(ctx, addr)
	if err != nil {
		return time.Time{}, fmt.Errorf("connect: %w", shortErr(err))
	}
	defer raw.Close()
	if dl, ok := ctx.Deadline(); ok {
		raw.SetDeadline(dl)
	}
	conn := tls.Client(raw, &tls.Config{ServerName: p.cfg.Host, RootCAs: p.roots, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	if err := conn.HandshakeContext(ctx); err != nil {
		return time.Time{}, fmt.Errorf("tls: %w", shortErr(err))
	}
	leaf := conn.ConnectionState().PeerCertificates[0]
	fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: %s\r\nUser-Agent: mirrin-canary/%s\r\nConnection: close\r\n\r\n", p.cfg.Host, version)
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("http: %w", shortErr(err))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("http: status %d", res.StatusCode)
	}
	var h healthz
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<10)).Decode(&h); err != nil || !h.OK {
		return time.Time{}, errors.New("http: not the canary's answer")
	}
	if h.Relay != relayID {
		return time.Time{}, fmt.Errorf("carried by relay %q, not %q", h.Relay, relayID)
	}
	return leaf.NotAfter, nil
}

// shortErr drops the addresses Go puts in dial errors, which the round
// already names.
func shortErr(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err
	}
	return err
}

func (p *prober) results() Results {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Results{Region: p.cfg.Region, Rounds: append([]Round{}, p.rounds...)}
}

// handler serves /v1/results and /healthz.
func (p *prober) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/results", func(w http.ResponseWriter, r *http.Request) {
		if p.token != "" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(p.token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(p.results())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

// run probes every cfg.Every until ctx ends.
func (p *prober) run(ctx context.Context) {
	tick := time.NewTicker(p.cfg.Every)
	defer tick.Stop()
	for {
		r := p.round(ctx)
		for _, x := range r.Relays {
			if !x.OK {
				p.log.Warn("probe: the canary didn't answer through a relay", "relay", x.ID, "addrs", x.Addrs)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
