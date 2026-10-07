// Package entitle signs and verifies Mirrin Cloud's offline tokens:
// entitlements and deny lists, both PASETO v4.public with a {"kid":…} footer.
// Each purpose has its own keys (ent-* and dl-*), and a token is accepted
// only under a key of its own purpose. It uses the standard library only, so
// the daemon, the relay and the control plane can all share it.
package entitle

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Audience is the aud of every entitlement.
const Audience = "antbot" // rename:keep: a wire value every issued token carries

var (
	// ErrExpired means now is at or past exp.
	ErrExpired = errors.New("entitle: entitlement expired")
	// ErrNotYetValid means an entitlement's nbf or iat, or a deny list's
	// iat, is ahead of now by more than the leeway: a clock is off.
	ErrNotYetValid = errors.New("entitle: not yet valid")
)

// Claims is what an entitlement grants. Times travel as RFC 3339 in UTC,
// to the second.
type Claims struct {
	Iss, Sub, Aud string
	Iat, Nbf, Exp time.Time
	PaidThrough   time.Time
	Gen           int64 // per-handle generation; the higher one wins a hostname
	Plan          string
	Feat          []string
	Handle        string
	Hosts         []string
	Cnf           string // EncodeKey of the device key the token is bound to
	Relays        []Relay
	BackupQuota   int64 // bytes
	WakeCredits   int
}

// Relay is one relay the daemon should tunnel to.
type Relay struct {
	ID  string
	URL string
	IPs []string
}

// wireClaims is the payload exactly as signed.
type wireClaims struct {
	Iss         string      `json:"iss"`
	Sub         string      `json:"sub"`
	Aud         string      `json:"aud"`
	Iat         string      `json:"iat"`
	Nbf         string      `json:"nbf"`
	Exp         string      `json:"exp"`
	Gen         int64       `json:"gen"`
	Plan        string      `json:"plan"`
	Feat        []string    `json:"feat"`
	Handle      string      `json:"handle"`
	Hosts       []string    `json:"hosts"`
	Cnf         string      `json:"cnf"`
	Relays      []wireRelay `json:"relays"`
	BQ          int64       `json:"bq"`
	WK          int         `json:"wk"`
	PaidThrough string      `json:"paid_through"`
}

type wireRelay struct {
	ID  string   `json:"id"`
	URL string   `json:"url"`
	IPs []string `json:"ips"`
}

// Sign issues an entitlement under kid, which must be an ent-* key. Times
// are truncated to the second.
func Sign(c Claims, kid string, priv ed25519.PrivateKey) (string, error) {
	for _, t := range []*time.Time{&c.Iat, &c.Nbf, &c.Exp, &c.PaidThrough} {
		*t = t.UTC().Truncate(time.Second)
	}
	if err := c.check(); err != nil {
		return "", err
	}
	w := wireClaims{
		Iss: c.Iss, Sub: c.Sub, Aud: c.Aud,
		Iat: formatTime(c.Iat), Nbf: formatTime(c.Nbf), Exp: formatTime(c.Exp),
		Gen: c.Gen, Plan: c.Plan, Feat: nonNil(c.Feat), Handle: c.Handle, Hosts: nonNil(c.Hosts),
		Cnf: c.Cnf, Relays: []wireRelay{}, BQ: c.BackupQuota, WK: c.WakeCredits,
		PaidThrough: formatTime(c.PaidThrough),
	}
	for _, r := range c.Relays {
		w.Relays = append(w.Relays, wireRelay{ID: r.ID, URL: r.URL, IPs: nonNil(r.IPs)})
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	return seal(b, entitlementPrefix, kid, priv, MaxTokenSize)
}

// Verify checks an entitlement against keys (normally EntitlementKeys) and
// the clock. It fails with ErrExpired from exp on, and ErrNotYetValid while
// nbf or iat is more than five minutes ahead.
func Verify(tok string, keys map[string]ed25519.PublicKey, now time.Time) (Claims, error) {
	c, err := Inspect(tok, keys)
	if err != nil {
		return Claims{}, err
	}
	if !now.Before(c.Exp) {
		return Claims{}, ErrExpired
	}
	if c.Nbf.After(now.Add(leeway)) || c.Iat.After(now.Add(leeway)) {
		return Claims{}, ErrNotYetValid
	}
	return c, nil
}

// Inspect is Verify without the clock: the signature and shape are checked,
// the times are not. It is for showing an expired entitlement's state, never
// for granting anything.
func Inspect(tok string, keys map[string]ed25519.PublicKey) (Claims, error) {
	m, err := open(tok, entitlementPrefix, keys, MaxTokenSize)
	if err != nil {
		return Claims{}, err
	}
	var w wireClaims
	if err := decodeStrict(m, &w); err != nil {
		return Claims{}, err
	}
	c := Claims{
		Iss: w.Iss, Sub: w.Sub, Aud: w.Aud, Gen: w.Gen, Plan: w.Plan, Feat: w.Feat,
		Handle: w.Handle, Hosts: w.Hosts, Cnf: w.Cnf, BackupQuota: w.BQ, WakeCredits: w.WK,
	}
	for _, f := range []struct {
		name string
		s    string
		t    *time.Time
	}{{"iat", w.Iat, &c.Iat}, {"nbf", w.Nbf, &c.Nbf}, {"exp", w.Exp, &c.Exp}, {"paid_through", w.PaidThrough, &c.PaidThrough}} {
		if *f.t, err = parseTime(f.name, f.s); err != nil {
			return Claims{}, err
		}
	}
	for _, r := range w.Relays {
		c.Relays = append(c.Relays, Relay(r))
	}
	if err := c.check(); err != nil {
		return Claims{}, err
	}
	return c, nil
}

// check holds the rules both Sign and Verify enforce.
func (c Claims) check() error {
	bad := func(what string) error { return fmt.Errorf("%w: %s", ErrMalformed, what) }
	switch {
	case c.Iss == "" || c.Sub == "":
		return bad("iss and sub are required")
	case c.Aud != Audience:
		return bad("aud is not " + Audience)
	case !inRange(c.Iat) || !inRange(c.Nbf) || !inRange(c.Exp) || !inRange(c.PaidThrough):
		return bad("iat, nbf, exp and paid_through are required, between 1970 and 9999")
	case !c.Nbf.Before(c.Exp):
		return bad("nbf is not before exp")
	case c.Gen < 1:
		return bad("gen starts at 1")
	case c.BackupQuota < 0 || c.WakeCredits < 0:
		return bad("negative quota")
	case slices.Contains(c.Feat, "") || slices.Contains(c.Hosts, ""):
		return bad("empty feature or host")
	}
	if _, err := ParseKey(c.Cnf); err != nil {
		return bad("cnf is not a public key")
	}
	for _, r := range c.Relays {
		if r.ID == "" || r.URL == "" {
			return bad("relay without id or url")
		}
	}
	return nil
}

// Key returns the device key the entitlement is bound to.
func (c Claims) Key() (ed25519.PublicKey, error) { return ParseKey(c.Cnf) }

// Has reports whether the entitlement includes feature f.
func (c Claims) Has(f string) bool { return slices.Contains(c.Feat, f) }

// Covers reports whether every hostname is in Hosts.
func (c Claims) Covers(hostnames []string) bool {
	for _, h := range hostnames {
		if !slices.Contains(c.Hosts, h) {
			return false
		}
	}
	return true
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
