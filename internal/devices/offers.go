package devices

import (
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"time"
)

// Pairing: the owner mints an offer (mirrin pair, or "Add your phone"), the
// new device claims it with the offer's secret, and gets its own token.
//
//   - An offer works once and for OfferTTL.
//   - Its secret is 32 random bytes that travel in the link's #fragment, so
//     it never reaches a server log; only its hash is kept.
//   - Five wrong secrets burn that one offer. There is deliberately no global
//     lockout: a stranger guessing at one address must never be able to stop
//     the owner pairing.
//   - Claims are limited per IP address (ClaimsPerMinute).

// OfferTTL is how long a pairing offer works.
const OfferTTL = 10 * time.Minute

// TicketTTL is how long an install ticket works.
const TicketTTL = 30 * time.Minute

// ClaimsPerMinute is how many pairing attempts one IP address gets a minute.
const ClaimsPerMinute = 10

// badSecretsBurn is how many wrong secrets an offer takes before it is burned.
const badSecretsBurn = 5

// Pairing errors.
var (
	ErrOfferUnknown = errors.New("no such pairing offer")
	ErrOfferUsed    = errors.New("this pairing link was already used")
	ErrOfferExpired = errors.New("this pairing link has expired")
	ErrOfferBurned  = errors.New("this pairing link had too many wrong tries")
	ErrOfferRevoked = errors.New("this pairing link was replaced or withdrawn")
	ErrBadSecret    = errors.New("that isn't this link's secret")
	ErrRateLimited  = errors.New("too many pairing attempts from this address")
	ErrWrongKind    = errors.New("this pairing link is for another kind of device")
	ErrTicketGone   = errors.New("this install ticket was used or has expired")
)

// Offer is a pairing offer as the owner sees it once: the secret is not
// stored anywhere.
type Offer struct {
	ID      string    `json:"id"`
	Secret  string    `json:"secret"`
	Kind    string    `json:"kind"`
	Scopes  []Scope   `json:"scopes"`
	Expires time.Time `json:"expires"`
}

type offer struct {
	hash    string
	kind    string
	scopes  []Scope
	expires time.Time
	used    bool
	gone    bool // withdrawn by CancelOffer
	bad     int
}

type ticket struct {
	device  string
	token   string
	expires time.Time
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewOffer mints a single-use pairing offer for a device of kind with scopes
// (the kind's defaults when empty), valid for ttl (OfferTTL when zero).
func (s *Store) NewOffer(kind string, scopes []Scope, ttl time.Duration) (Offer, error) {
	if !ValidKind(kind) {
		return Offer{}, errors.New("a pairing is for a pwa (phone or tablet), a kiosk (wall screen) or a cli (another computer)")
	}
	if len(scopes) == 0 {
		scopes = DefaultScopes(kind)
	}
	for _, sc := range scopes {
		if !slices.Contains(AllScopes, sc) {
			return Offer{}, errors.New("unknown scope " + string(sc))
		}
	}
	if ttl <= 0 {
		ttl = OfferTTL
	}
	secret := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.gcOffers(now)
	id := "of_" + strings.ToLower(b32.EncodeToString(randomBytes(7)))[:10]
	for s.offers[id] != nil {
		id = "of_" + strings.ToLower(b32.EncodeToString(randomBytes(7)))[:10]
	}
	o := &offer{hash: HashToken(secret), kind: kind, scopes: slices.Clone(scopes), expires: now.Add(ttl)}
	s.offers[id] = o
	return Offer{ID: id, Secret: secret, Kind: kind, Scopes: slices.Clone(scopes), Expires: o.expires}, nil
}

// CancelOffer withdraws an unclaimed offer, so its code no longer pairs
// anything: "Add your phone" does this to the codes it stopped showing. It
// reports whether an unclaimed offer was withdrawn; a claimed or unknown
// one is left alone.
func (s *Store) CancelOffer(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.offers[id]
	if o == nil || o.used || o.gone {
		return false
	}
	o.gone = true
	return true
}

// gcOffers forgets offers an hour past their end (until then a late claim
// hears "expired", not "unknown"), and stale tickets and attempt records.
// Callers hold mu.
func (s *Store) gcOffers(now time.Time) {
	for id, o := range s.offers {
		if now.Sub(o.expires) > time.Hour {
			delete(s.offers, id)
		}
	}
	for id, t := range s.tickets {
		if now.After(t.expires) {
			delete(s.tickets, id)
		}
	}
	for ip, ts := range s.attempts {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > time.Minute {
			delete(s.attempts, ip)
		}
	}
}

// allow records a claim attempt from ip and reports whether it is within the
// per-IP limit. Callers hold mu.
func (s *Store) allow(ip string, now time.Time) bool {
	ts := s.attempts[ip]
	keep := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= ClaimsPerMinute {
		s.attempts[ip] = keep
		return false
	}
	s.attempts[ip] = append(keep, now)
	return true
}

// RetryAfter is how long ip must wait before its next claim is allowed.
func (s *Store) RetryAfter(ip string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.attempts[ip]
	if len(ts) < ClaimsPerMinute {
		return 0
	}
	wait := time.Minute - s.now().Sub(ts[0])
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// Claim spends an offer: with the right secret, a new device is paired and
// its token returned (once). kind, when given, must be the offer's kind. via
// says how the device reached the twin (lan, tailscale, relay…).
func (s *Store) Claim(offerID, secret, name, kind, via, ip string) (Device, string, error) {
	s.mu.Lock()
	now := s.now()
	s.gcOffers(now)
	if !s.allow(ip, now) {
		s.mu.Unlock()
		return Device{}, "", ErrRateLimited
	}
	o := s.offers[offerID]
	switch {
	case o == nil:
		s.mu.Unlock()
		return Device{}, "", ErrOfferUnknown
	case o.used:
		s.mu.Unlock()
		return Device{}, "", ErrOfferUsed
	case o.gone:
		s.mu.Unlock()
		return Device{}, "", ErrOfferRevoked
	case o.bad >= badSecretsBurn:
		s.mu.Unlock()
		return Device{}, "", ErrOfferBurned
	case now.After(o.expires):
		s.mu.Unlock()
		return Device{}, "", ErrOfferExpired
	}
	if subtle.ConstantTimeCompare([]byte(HashToken(secret)), []byte(o.hash)) != 1 {
		o.bad++
		s.mu.Unlock()
		return Device{}, "", ErrBadSecret
	}
	if kind != "" && kind != o.kind {
		s.mu.Unlock()
		return Device{}, "", ErrWrongKind
	}
	o.used = true
	s.mu.Unlock()
	d, tok, err := s.add(name, o.kind, o.scopes, via, ip, false, true) // claimed: may enrol a passkey soon after
	if err != nil {
		s.mu.Lock()
		o.used = false // nothing was paired; the owner's link still works
		s.mu.Unlock()
		return Device{}, "", err
	}
	return d, tok, nil
}

// NewTicket holds a device's token for TicketTTL so a Home Screen web app
// (whose storage is separate from the browser's) can collect it once. It is
// kept in memory only.
func (s *Store) NewTicket(deviceID, token string) string {
	id := "it_" + base64.RawURLEncoding.EncodeToString(randomBytes(18))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[id] = &ticket{device: deviceID, token: token, expires: s.now().Add(TicketTTL)}
	return id
}

// RedeemTicket returns the device and token a ticket holds, once.
func (s *Store) RedeemTicket(id string) (Device, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t *ticket
	var key string
	for k, v := range s.tickets { // constant-time over every live ticket
		if subtle.ConstantTimeCompare([]byte(k), []byte(id)) == 1 {
			t, key = v, k
		}
	}
	if t == nil || s.now().After(t.expires) {
		return Device{}, "", ErrTicketGone
	}
	delete(s.tickets, key)
	d := s.devs[t.device]
	if d == nil || d.Revoked() {
		return Device{}, "", ErrTicketGone
	}
	return d.Public(), t.token, nil
}
