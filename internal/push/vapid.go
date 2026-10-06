package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const DefaultSubject = "https://github.com/MavrkAI/Mirrin"

type VAPID struct{ key *ecdsa.PrivateKey }

func (v *VAPID) PublicKey() string {
	k, _ := v.key.PublicKey.ECDH()
	return raw.EncodeToString(k.Bytes())
}
func LoadOrCreateVAPID(path string) (*VAPID, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, errors.New("invalid VAPID key")
		}
		k, e := x509.ParseECPrivateKey(block.Bytes)
		if e != nil {
			return nil, e
		}
		if k.Curve != elliptic.P256() {
			return nil, errors.New("VAPID requires P-256")
		}
		if e = os.Chmod(path, 0600); e != nil {
			return nil, e
		}
		return &VAPID{k}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".vapid-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	// Publish only a complete key. A concurrent creator wins without ever
	// replacing the key that existing subscriptions use.
	if err = os.Link(f.Name(), path); errors.Is(err, os.ErrExist) {
		return LoadOrCreateVAPID(path)
	}
	if err != nil {
		return nil, err
	}
	return &VAPID{k}, nil
}
func (v *VAPID) Header(endpoint, subject string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", errors.New("invalid push origin")
	}
	if subject == "" {
		subject = DefaultSubject
	}
	s, err := url.Parse(subject)
	if err != nil || s.Scheme != "https" || s.Host == "" || s.User != nil {
		return "", errors.New("push subject must be an HTTPS URL")
	}
	aud := "https://" + strings.ToLower(u.Hostname())
	if u.Port() != "" && u.Port() != "443" {
		aud = u.Scheme + "://" + u.Host
	}
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": now.Add(12 * time.Hour).Unix(), "sub": subject})
	token := raw.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + raw.EncodeToString(claims)
	digest := sha256.Sum256([]byte(token))
	r, sig, err := ecdsa.Sign(rand.Reader, v.key, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	sig.FillBytes(signature[32:])
	return "vapid t=" + token + "." + raw.EncodeToString(signature) + ", k=" + v.PublicKey(), nil
}
