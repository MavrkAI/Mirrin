package tlsmgr

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

type files struct {
	mu        sync.RWMutex
	cert, key string
	pair      *tls.Certificate
	next      crypto.Signer
	warning   error
	now       func() time.Time
	wait      func(context.Context, time.Duration) bool
	managed   bool // Tailscale manages its own next key
}

func Files(cert, key string) Source { f := &files{cert: cert, key: key}; _ = f.reload(); return f }
func (f *files) reload() error {
	p, err := tls.LoadX509KeyPair(f.cert, f.key)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(p.Certificate[0])
	if err != nil {
		return err
	}
	p.Leaf = leaf
	if f.time().Before(leaf.NotBefore) || !f.time().Before(leaf.NotAfter) {
		return fmt.Errorf("the certificate is not valid now; renew your HTTPS certificate")
	}
	var next crypto.Signer
	var nextErr error
	if !f.managed {
		next, err = nextKey(f.key+".next", p.PrivateKey.(crypto.Signer))
		if err != nil {
			nextErr = fmt.Errorf("prepare next HTTPS key: %w", err)
		}
	}
	f.mu.Lock()
	if nextErr == nil {
		f.next = next
	}
	f.pair = &p
	f.mu.Unlock()
	return nextErr
}
func (f *files) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.pair == nil {
		return nil, fmt.Errorf("could not load HTTPS; check reach.cert_file and reach.key_file")
	}
	if !f.time().Before(f.pair.Leaf.NotAfter) {
		return nil, fmt.Errorf("the HTTPS certificate has expired; renew it to restore remote access")
	}
	return f.pair, nil
}
func (f *files) Hostnames() []string {
	p, e := f.GetCertificate(nil)
	if e != nil {
		return nil
	}
	return append([]string(nil), p.Leaf.DNSNames...)
}
func (f *files) SPKIs() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.pair == nil {
		return nil
	}
	h := sha256.Sum256(f.pair.Leaf.RawSubjectPublicKeyInfo)
	pins := []string{base64.RawURLEncoding.EncodeToString(h[:])}
	if f.next != nil {
		pins = append(pins, keyPin(f.next))
	}
	return pins
}
func (f *files) Run(ctx context.Context) error {
	return f.maintain(ctx, time.Minute, func(context.Context) error { return f.reload() })
}
