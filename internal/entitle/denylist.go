package entitle

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"time"
)

// MaxDenyListSize bounds a deny list token. It is larger than MaxTokenSize
// because the list grows with recoveries and suspensions: 8 KiB would hold
// about 90 keys, and a list that outgrows its cap cannot be delivered, which
// silently stops revocation. 256 KiB holds a few thousand entries. An entry
// only has to outlive the entitlements it cancels (at most 35 days), so the
// control plane can prune older ones. docs/cloud-design.md records the cap.
const MaxDenyListSize = 256 << 10

// DenyList cancels handles and device keys before their entitlements expire.
// Relays poll it; a higher Seq replaces a lower one, and a lower one is
// ignored.
type DenyList struct {
	Seq     int64
	Iat     time.Time
	Handles []Entry // Value is a handle
	Keys    []Entry // Value is EncodeKey of a device key
}

// Entry is one denied handle or key, and why.
type Entry struct {
	Value string
	Why   string
}

// wireDenyList is the payload exactly as signed:
// {seq, iat, handles:[{h,why}], keys:[{k,why}]}.
type wireDenyList struct {
	Seq     int64        `json:"seq"`
	Iat     string       `json:"iat"`
	Handles []wireHandle `json:"handles"`
	Keys    []wireKey    `json:"keys"`
}

type wireHandle struct {
	H   string `json:"h"`
	Why string `json:"why"`
}

type wireKey struct {
	K   string `json:"k"`
	Why string `json:"why"`
}

// SignDenyList signs d under kid, which must be a dl-* key. Iat is truncated
// to the second.
func SignDenyList(d DenyList, kid string, priv ed25519.PrivateKey) (string, error) {
	d.Iat = d.Iat.UTC().Truncate(time.Second)
	if err := d.check(); err != nil {
		return "", err
	}
	w := wireDenyList{Seq: d.Seq, Iat: formatTime(d.Iat), Handles: []wireHandle{}, Keys: []wireKey{}}
	for _, e := range d.Handles {
		w.Handles = append(w.Handles, wireHandle{H: e.Value, Why: e.Why})
	}
	for _, e := range d.Keys {
		w.Keys = append(w.Keys, wireKey{K: e.Value, Why: e.Why})
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	return seal(b, denyListPrefix, kid, priv, MaxDenyListSize)
}

// VerifyDenyList checks a deny list against keys (normally DenyListKeys). A
// list dated more than five minutes ahead of now is refused with
// ErrNotYetValid, so a relay can tell a clock problem from a bad list.
// Ordering by Seq is the caller's job.
func VerifyDenyList(tok string, keys map[string]ed25519.PublicKey, now time.Time) (DenyList, error) {
	m, err := open(tok, denyListPrefix, keys, MaxDenyListSize)
	if err != nil {
		return DenyList{}, err
	}
	var w wireDenyList
	if err := decodeStrict(m, &w); err != nil {
		return DenyList{}, err
	}
	d := DenyList{Seq: w.Seq}
	if d.Iat, err = parseTime("iat", w.Iat); err != nil {
		return DenyList{}, err
	}
	for _, h := range w.Handles {
		d.Handles = append(d.Handles, Entry{Value: h.H, Why: h.Why})
	}
	for _, k := range w.Keys {
		d.Keys = append(d.Keys, Entry{Value: k.K, Why: k.Why})
	}
	if err := d.check(); err != nil {
		return DenyList{}, err
	}
	if d.Iat.After(now.Add(leeway)) {
		return DenyList{}, fmt.Errorf("%w: deny list iat is in the future", ErrNotYetValid)
	}
	return d, nil
}

func (d DenyList) check() error {
	bad := func(what string) error { return fmt.Errorf("%w: deny list: %s", ErrMalformed, what) }
	if d.Seq < 1 {
		return bad("seq starts at 1")
	}
	if !inRange(d.Iat) {
		return bad("iat is required")
	}
	for _, e := range d.Handles {
		if e.Value == "" {
			return bad("empty handle")
		}
	}
	for _, e := range d.Keys {
		if _, err := ParseKey(e.Value); err != nil {
			return bad("entry is not a public key")
		}
	}
	return nil
}

// DeniesHandle reports whether handle h is on the list.
func (d DenyList) DeniesHandle(h string) bool {
	for _, e := range d.Handles {
		if e.Value == h {
			return true
		}
	}
	return false
}

// DeniesKey reports whether the device key is on the list.
func (d DenyList) DeniesKey(pub ed25519.PublicKey) bool {
	k := EncodeKey(pub)
	for _, e := range d.Keys {
		if e.Value == k {
			return true
		}
	}
	return false
}
