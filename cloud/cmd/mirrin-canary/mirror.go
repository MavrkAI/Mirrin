package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/sigv4"
)

// The mirror copies the control plane's signed deny list to a public
// bucket (R2), which relays read when the control plane can't be reached
// (docs/cloud-design.md §6.4). It only ever publishes a list that verifies
// under a dl-* key and whose seq is not lower than the last it published,
// so a broken or rolled-back control plane can't shrink the list relays
// fall back to. The relays keep the highest seq they have seen anyway.

type mirror struct {
	cfg   MirrorConfig
	keys  map[string]ed25519.PublicKey
	creds sigv4.Creds
	http  *http.Client
	log   *slog.Logger
	now   func() time.Time

	seeded bool      // seq has been read from the bucket
	seq    int64     // the last seq published
	at     time.Time // when
}

func newMirror(cfg *Config, log *slog.Logger) (*mirror, error) {
	keys, err := cfg.dlKeys()
	if err != nil {
		return nil, err
	}
	m := &mirror{cfg: cfg.Mirror, keys: keys, log: log, now: time.Now, http: &http.Client{Timeout: 30 * time.Second},
		creds: sigv4.Creds{AccessKeyID: os.Getenv(cfg.Mirror.AccessKeyEnv), SecretAccessKey: os.Getenv(cfg.Mirror.SecretKeyEnv)}}
	if m.creds.AccessKeyID == "" || m.creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("mirror: %s and %s must hold the bucket's write credentials", cfg.Mirror.AccessKeyEnv, cfg.Mirror.SecretKeyEnv)
	}
	return m, nil
}

// step fetches the list and publishes it when its seq is new, or when the
// copy is older than Refresh (so the mirror's own age says it is alive).
// It reports whether it published.
func (m *mirror) step(ctx context.Context) (bool, error) {
	if !m.seeded {
		if err := m.seed(ctx); err != nil {
			return false, fmt.Errorf("reading the mirrored list first: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.Source, nil)
	if err != nil {
		return false, err
	}
	res, err := m.http.Do(req)
	if err != nil {
		return false, shortErr(err)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, entitle.MaxDenyListSize+2))
	res.Body.Close()
	if err != nil {
		return false, err
	}
	if res.StatusCode != http.StatusOK {
		return false, fmt.Errorf("the control plane answered %d", res.StatusCode)
	}
	tok := strings.TrimSpace(string(b))
	now := m.now()
	d, err := entitle.VerifyDenyList(tok, m.keys, now)
	if err != nil {
		return false, fmt.Errorf("not publishing a deny list that doesn't verify: %w", err)
	}
	switch {
	case d.Seq < m.seq:
		return false, fmt.Errorf("the control plane serves seq %d, lower than the %d already mirrored; not publishing it", d.Seq, m.seq)
	case d.Seq == m.seq && now.Sub(m.at) < m.cfg.Refresh:
		return false, nil
	}
	if err := m.put(ctx, []byte(tok+"\n"), now); err != nil {
		return false, err
	}
	m.seq, m.at = d.Seq, now
	return true, nil
}

// seed reads the list already in the bucket, so a restarted mirror never
// publishes a lower seq than the one there.
func (m *mirror) seed(ctx context.Context) error {
	req, err := m.request(ctx, http.MethodGet, nil, m.now())
	if err != nil {
		return err
	}
	res, err := m.http.Do(req)
	if err != nil {
		return shortErr(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, entitle.MaxDenyListSize+2))
	if err != nil {
		return err
	}
	switch res.StatusCode {
	case http.StatusNotFound:
	case http.StatusOK:
		// A list that doesn't verify (an old key, say) is replaced.
		if d, err := entitle.VerifyDenyList(strings.TrimSpace(string(b)), m.keys, m.now()); err == nil {
			m.seq = d.Seq
		}
	default:
		return fmt.Errorf("the bucket answered %d", res.StatusCode)
	}
	m.seeded = true
	return nil
}

// request is a signed request for the mirror's object.
func (m *mirror) request(ctx context.Context, method string, body []byte, now time.Time) (*http.Request, error) {
	u, err := url.Parse(strings.TrimSuffix(m.cfg.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	u.Path = "/" + m.cfg.Bucket + "/" + m.cfg.Key
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	} else {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		req.Header.Set("Cache-Control", "public, max-age=30")
	}
	if err := sigv4.Sign(req, m.creds, m.cfg.Region, "s3", now, sigv4.PayloadHash(body)); err != nil {
		return nil, err
	}
	return req, nil
}

// put stores body at the mirror's key.
func (m *mirror) put(ctx context.Context, body []byte, now time.Time) error {
	req, err := m.request(ctx, http.MethodPut, body, now)
	if err != nil {
		return err
	}
	res, err := m.http.Do(req)
	if err != nil {
		return shortErr(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("the bucket answered %d", res.StatusCode)
	}
	return nil
}

func (m *mirror) run(ctx context.Context) {
	tick := time.NewTicker(m.cfg.Every)
	defer tick.Stop()
	for {
		if did, err := m.step(ctx); err != nil {
			m.log.Error("mirror: deny list not mirrored", "err", err)
		} else if did {
			m.log.Info("mirror: deny list published", "seq", m.seq)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
