// Package devices is the registry of everything paired with the twin: phones,
// tablets, wall screens, other computers' terminals, and the browsers on this
// computer that opened its pages. Each holds its own token, with its own
// scopes, and can be cut off on its own without touching the others.
//
// Tokens look like abt1_<id>_<secret>. Only their SHA-256 is kept, in
// data/devices.json (mode 0600), so the file never holds anything that works
// as a credential. A revoked device stays in the file as a tombstone.
package devices

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Scope is one kind of access a device can have.
type Scope string

// The scopes. Admin covers settings (channels, accounts, memory, devices).
const (
	View    Scope = "view"    // the presence screen, events, screenshots
	Chat    Scope = "chat"    // talking to the twin
	Approve Scope = "approve" // deciding approvals
	Admin   Scope = "admin"   // settings
)

// AllScopes is every scope, in display order.
var AllScopes = []Scope{View, Chat, Approve, Admin}

// Device kinds.
const (
	KindPWA   = "pwa"   // a phone or tablet browser
	KindKiosk = "kiosk" // a wall screen
	KindCLI   = "cli"   // another computer's terminal (mirrin connect)
	KindLocal = "local" // a browser on this computer; works on the loopback listener only
)

// TokenPrefix starts every device token.
const TokenPrefix = "abt1_"

// DefaultScopes is what a device of this kind gets unless asked otherwise.
func DefaultScopes(kind string) []Scope {
	switch kind {
	case KindKiosk:
		return []Scope{View}
	case KindLocal:
		return slices.Clone(AllScopes)
	default:
		return []Scope{View, Chat, Approve}
	}
}

// ValidKind reports whether kind is one a pairing can create.
func ValidKind(kind string) bool {
	return kind == KindPWA || kind == KindKiosk || kind == KindCLI
}

// ParseScopes reads "view,chat,approve".
func ParseScopes(s string) ([]Scope, error) {
	var out []Scope
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		sc := Scope(strings.ToLower(f))
		if !slices.Contains(AllScopes, sc) {
			return nil, fmt.Errorf("%q isn't a scope; use view, chat, approve or admin", f)
		}
		if !slices.Contains(out, sc) {
			out = append(out, sc)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no scopes given; use view, chat, approve or admin")
	}
	return out, nil
}

// Passkey is a WebAuthn credential enrolled on a device (step-up approvals).
// Package stepup fills it in; this package only keeps it.
type Passkey struct {
	ID        string    `json:"id"`         // the credential id, base64url
	PublicKey string    `json:"public_key"` // the COSE public key, base64url
	Created   time.Time `json:"created"`
	// RPID is the host name it was made for: a passkey works only there.
	RPID string `json:"rp_id,omitempty"`
	// Grant says what allowed it to be enrolled: pairing, passkey or owner.
	Grant string `json:"grant,omitempty"`
	// Credential is the whole credential record, in package stepup's
	// format: sign count, flags, AAGUID.
	Credential json.RawMessage `json:"credential,omitempty"`
	LastUsed   time.Time       `json:"last_used,omitzero"`
}

// Device is one paired device.
type Device struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	Scopes    []Scope    `json:"scopes"`
	TokenHash string     `json:"token_sha256,omitempty"`
	Passkeys  []Passkey  `json:"passkeys,omitempty"`
	Via       string     `json:"via,omitempty"` // how it paired: loopback, lan, tailscale, relay
	Created   time.Time  `json:"created"`
	LastSeen  time.Time  `json:"last_seen,omitzero"`
	LastIP    string     `json:"last_ip,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// SharedKey marks a device that first came in with the master key (an
	// old-style pairing code, or a screen set up before devices had keys of
	// their own) and was moved onto a key of its own. Whoever holds it may
	// still hold the master key, so cutting it off means changing that key
	// too.
	SharedKey bool `json:"shared_key,omitempty"`
	// ClaimedAt is when the device claimed a pairing offer; zero for one
	// that got its key another way. A passkey may be enrolled without more
	// proof only shortly after, and only once (PairEnrolUsed).
	ClaimedAt     time.Time `json:"claimed_at,omitzero"`
	PairEnrolUsed bool      `json:"pair_enrol_used,omitempty"`
	// PasskeyCount is how many passkeys it has, filled in on the copies the
	// store hands out, which never carry the passkeys themselves.
	PasskeyCount int `json:"passkey_count,omitempty"`
}

// Has reports whether the device holds scope s.
func (d Device) Has(s Scope) bool { return slices.Contains(d.Scopes, s) }

// Local reports whether the device is a browser on this computer, which only
// the loopback listener accepts.
func (d Device) Local() bool { return d.Kind == KindLocal }

// Revoked reports whether the device was cut off.
func (d Device) Revoked() bool { return d.RevokedAt != nil }

// Public is the device without its token hash, for pages and CLIs.
func (d Device) Public() Device {
	d.TokenHash = ""
	d.Scopes = slices.Clone(d.Scopes)
	d.PasskeyCount = len(d.Passkeys)
	d.Passkeys = nil
	return d
}

// ChangeKind says what happened to a device.
type ChangeKind string

// Changes a hook hears about.
const (
	Added   ChangeKind = "added"
	Revoked ChangeKind = "revoked"
	Renamed ChangeKind = "renamed"
	// PasskeyAdded: a passkey was enrolled on the device.
	PasskeyAdded ChangeKind = "passkey_added"
	// PasskeysRemoved: some of its passkeys were removed; it stays paired.
	PasskeysRemoved ChangeKind = "passkeys_removed"
)

// Change is one event in the registry.
type Change struct {
	Kind   ChangeKind
	Device Device
}

// Errors.
var (
	ErrNotFound  = errors.New("no such device")
	ErrAmbiguous = errors.New("more than one device starts with that")
)

// Limits on the browsers of this computer, which the menu's links pair
// silently: kept while used, forgotten after half a year unused, and never
// more than maxLocal at once.
const (
	maxLocal    = 32
	localMaxAge = 180 * 24 * time.Hour
)

// touchEvery is how often a device's last-seen time is written to disk.
const touchEvery = time.Minute

// Store is the registry. It is safe for concurrent use.
type Store struct {
	mu        sync.Mutex
	path      string // "" keeps everything in memory
	devs      map[string]*Device
	offers    map[string]*offer
	tickets   map[string]*ticket
	attempts  map[string][]time.Time // pairing claims per IP in the last minute
	hooks     []func(Change)
	lastSaved time.Time
	now       func() time.Time
}

type file struct {
	Version int       `json:"version"`
	Devices []*Device `json:"devices"`
}

// NewMemory is a registry that lives only as long as the process.
func NewMemory() *Store {
	return &Store{devs: map[string]*Device{}, offers: map[string]*offer{}, tickets: map[string]*ticket{}, attempts: map[string][]time.Time{}, now: time.Now}
}

// Path is where a twin keeps its registry.
func Path(dataDir string) string { return filepath.Join(dataDir, "devices.json") }

// Open loads the registry at path, or starts an empty one if there is none.
// A file that can't be read is an error, never silently replaced: every
// paired device would lose access.
func Open(path string) (*Store, error) {
	s := NewMemory()
	s.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s is damaged (%v); move it aside to start with no paired devices", path, err)
	}
	for _, d := range f.Devices {
		if d == nil || !validID(d.ID) {
			continue // hand-edited: no token can name it anyway
		}
		s.devs[d.ID] = d
	}
	// Tighten a file someone loosened by hand.
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		_ = os.Chmod(path, 0o600)
	}
	return s, nil
}

// File is where the registry is kept ("" when it lives in memory).
func (s *Store) File() string { return s.path }

// SetClock replaces the clock (tests).
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// OnChange registers fn to hear about added, revoked and renamed devices. It
// runs after the change is saved, outside the lock.
func (s *Store) OnChange(fn func(Change)) {
	s.mu.Lock()
	s.hooks = append(s.hooks, fn)
	s.mu.Unlock()
}

func (s *Store) fire(c Change) {
	s.mu.Lock()
	hooks := slices.Clone(s.hooks)
	s.mu.Unlock()
	for _, h := range hooks {
		h(c)
	}
}

// save writes the registry: a temp file, then a rename, so a crash never
// leaves half a file. Callers hold mu.
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	f := file{Version: 1, Devices: make([]*Device, 0, len(s.devs))}
	for _, d := range s.devs {
		f.Devices = append(f.Devices, d)
	}
	sort.Slice(f.Devices, func(i, j int) bool {
		if !f.Devices[i].Created.Equal(f.Devices[j].Created) {
			return f.Devices[i].Created.Before(f.Devices[j].Created)
		}
		return f.Devices[i].ID < f.Devices[j].ID
	})
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".devices-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	s.lastSaved = s.now()
	return nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("devices: no randomness: " + err.Error()) // crypto/rand never fails on supported platforms
	}
	return b
}

// HashToken is the hex SHA-256 kept in place of a token or secret.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func newID() string { return hex.EncodeToString(randomBytes(8)) }

// validID reports whether id has the shape newID gives: 16 hex characters.
func validID(id string) bool {
	if len(id) != 16 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func newToken(id string) string {
	return TokenPrefix + id + "_" + base64.RawURLEncoding.EncodeToString(randomBytes(32))
}

// splitToken returns the device id inside a well-formed token.
func splitToken(tok string) (string, bool) {
	rest, ok := strings.CutPrefix(tok, TokenPrefix)
	if !ok {
		return "", false
	}
	id, secret, ok := strings.Cut(rest, "_")
	if !ok || !validID(id) || len(secret) != 43 {
		return "", false
	}
	return id, true
}

// LooksLikeToken reports whether s has the shape of a device token.
func LooksLikeToken(s string) bool {
	_, ok := splitToken(s)
	return ok
}

// CleanName makes a device name safe to show: one line, printable, at most
// 60 characters. Empty stays empty.
func CleanName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if r := []rune(name); len(r) > 60 {
		name = strings.TrimSpace(string(r[:60]))
	}
	return name
}

// DefaultName names a device when nobody did.
func DefaultName(kind string) string {
	switch kind {
	case KindKiosk:
		return "Wall screen"
	case KindCLI:
		return "Another computer"
	case KindLocal:
		return "A browser on this computer"
	default:
		return "Phone or tablet"
	}
}

// Add pairs a new device and returns it with its token, which is shown once
// and never stored.
func (s *Store) Add(name, kind string, scopes []Scope, via, ip string) (Device, string, error) {
	return s.add(name, kind, scopes, via, ip, false, false)
}

// AddShared moves a device that reached the twin with the master key onto a
// key of its own, with its kind's default scopes (see Device.SharedKey).
func (s *Store) AddShared(name, kind, via, ip string) (Device, string, error) {
	return s.add(name, kind, nil, via, ip, true, false)
}

func (s *Store) add(name, kind string, scopes []Scope, via, ip string, shared, claimed bool) (Device, string, error) {
	if len(scopes) == 0 {
		scopes = DefaultScopes(kind)
	}
	for _, sc := range scopes {
		if !slices.Contains(AllScopes, sc) {
			return Device{}, "", fmt.Errorf("unknown scope %q", sc)
		}
	}
	if name = CleanName(name); name == "" {
		name = DefaultName(kind)
	}
	s.mu.Lock()
	now := s.now()
	id := newID()
	for s.devs[id] != nil {
		id = newID()
	}
	tok := newToken(id)
	d := &Device{ID: id, Name: name, Kind: kind, Scopes: slices.Clone(scopes), TokenHash: HashToken(tok), Via: via, Created: now, LastSeen: now, LastIP: ip, SharedKey: shared}
	if claimed {
		d.ClaimedAt = now
	}
	s.devs[id] = d
	if kind == KindLocal {
		s.pruneLocal(now)
	}
	err := s.save()
	if err != nil {
		delete(s.devs, id)
		s.mu.Unlock()
		return Device{}, "", err
	}
	out := *d
	s.mu.Unlock()
	s.fire(Change{Kind: Added, Device: out.Public()})
	return out.Public(), tok, nil
}

// AddLocal pairs a browser on this computer (opened from the menu's links).
func (s *Store) AddLocal(name string) (Device, string, error) {
	return s.Add(name, KindLocal, DefaultScopes(KindLocal), "loopback", "")
}

// pruneLocal forgets this computer's browsers that haven't been seen in half
// a year, and the least recently seen past maxLocal. They are never shown as
// devices, so they are removed, not tombstoned. Callers hold mu.
func (s *Store) pruneLocal(now time.Time) {
	var live []*Device
	for id, d := range s.devs {
		if d.Kind != KindLocal {
			continue
		}
		if d.Revoked() || now.Sub(lastUse(d)) > localMaxAge {
			delete(s.devs, id)
			continue
		}
		live = append(live, d)
	}
	if len(live) <= maxLocal {
		return
	}
	sort.Slice(live, func(i, j int) bool { return lastUse(live[i]).Before(lastUse(live[j])) })
	for _, d := range live[:len(live)-maxLocal] {
		delete(s.devs, d.ID)
	}
}

func lastUse(d *Device) time.Time {
	if d.LastSeen.After(d.Created) {
		return d.LastSeen
	}
	return d.Created
}

// dummyHash keeps the time Authenticate takes the same whether or not the id
// exists.
var dummyHash = HashToken("abt1_0000000000000000_none")

// Authenticate returns the device a token belongs to. The comparison is
// constant-time, and a revoked device never authenticates.
func (s *Store) Authenticate(tok string) (Device, bool) {
	id, ok := splitToken(tok)
	if !ok {
		return Device{}, false
	}
	got := HashToken(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devs[id]
	want := dummyHash
	if d != nil && d.TokenHash != "" {
		want = d.TokenHash
	}
	match := subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	if d == nil || d.Revoked() || d.TokenHash == "" || !match {
		return Device{}, false
	}
	return d.Public(), true
}

// Touch records that a device was just used, from ip. The time is written to
// disk at most once a minute (or when the address changes).
func (s *Store) Touch(id, ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devs[id]
	if d == nil || d.Revoked() {
		return
	}
	now := s.now()
	moved := ip != "" && ip != d.LastIP
	d.LastSeen = now
	if ip != "" {
		d.LastIP = ip
	}
	if moved || now.Sub(s.lastSaved) >= touchEvery {
		_ = s.save()
	}
}

// Get returns one device.
func (s *Store) Get(id string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devs[id]
	if d == nil {
		return Device{}, false
	}
	return d.Public(), true
}

// Find returns the one device (not revoked, not a local browser) whose id
// starts with prefix, so an owner can type a few characters of it.
func (s *Store) Find(prefix string) (Device, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if len(prefix) < 4 {
		return Device{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var hit *Device
	for id, d := range s.devs {
		if d.Revoked() || d.Kind == KindLocal || !strings.HasPrefix(id, prefix) {
			continue
		}
		if hit != nil {
			return Device{}, ErrAmbiguous
		}
		hit = d
	}
	if hit == nil {
		return Device{}, ErrNotFound
	}
	return hit.Public(), nil
}

// List returns every device, oldest first, revoked ones included.
func (s *Store) List() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.devs))
	for _, d := range s.devs {
		out = append(out, d.Public())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Revoke cuts a device off at once. Its token, passkeys and ticket stop
// working; the entry stays as a tombstone so a restored backup can't bring it
// back. Revoking a revoked device is not an error.
func (s *Store) Revoke(id string) (Device, error) {
	s.mu.Lock()
	d := s.devs[id]
	if d == nil {
		s.mu.Unlock()
		return Device{}, ErrNotFound
	}
	if d.Revoked() {
		out := d.Public()
		s.mu.Unlock()
		return out, nil
	}
	now := s.now()
	prev := *d
	d.RevokedAt = &now
	d.TokenHash = ""
	d.Passkeys = nil
	for k, t := range s.tickets {
		if t.device == id {
			delete(s.tickets, k)
		}
	}
	if err := s.save(); err != nil {
		*d = prev
		s.mu.Unlock()
		return Device{}, err
	}
	out := d.Public()
	s.mu.Unlock()
	s.fire(Change{Kind: Revoked, Device: out})
	return out, nil
}

// Rename gives a device a new name.
func (s *Store) Rename(id, name string) (Device, error) {
	name = CleanName(name)
	if name == "" {
		return Device{}, errors.New("a device needs a name")
	}
	s.mu.Lock()
	d := s.devs[id]
	if d == nil || d.Revoked() {
		s.mu.Unlock()
		return Device{}, ErrNotFound
	}
	old := d.Name
	d.Name = name
	if err := s.save(); err != nil {
		d.Name = old
		s.mu.Unlock()
		return Device{}, err
	}
	out := d.Public()
	s.mu.Unlock()
	s.fire(Change{Kind: Renamed, Device: out})
	return out, nil
}

// Passkeys. The store keeps them and never lets a copy of a device carry
// them (Public); package stepup makes and checks them.

// Passkey errors.
var (
	ErrEnrolClosed    = errors.New("this device can't enrol a passkey without more proof")
	ErrPasskeyUnknown = errors.New("no such passkey on this device")
	ErrPasskeyExists  = errors.New("this passkey is already set up on this device")
)

// Passkeys returns a device's passkeys (none for a revoked one).
func (s *Store) Passkeys(id string) []Passkey {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devs[id]
	if d == nil || d.Revoked() {
		return nil
	}
	out := make([]Passkey, len(d.Passkeys))
	for i, pk := range d.Passkeys {
		pk.Credential = slices.Clone(pk.Credential)
		out[i] = pk
	}
	return out
}

// PairingEnrolOpen reports whether a device claimed a pairing offer within
// window and hasn't yet used that to enrol a passkey.
func (s *Store) PairingEnrolOpen(id string, window time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devs[id]
	return d != nil && s.pairingOpen(d, window)
}

// pairingOpen: callers hold mu.
func (s *Store) pairingOpen(d *Device, window time.Duration) bool {
	if d.Revoked() || d.ClaimedAt.IsZero() || d.PairEnrolUsed {
		return false
	}
	age := s.now().Sub(d.ClaimedAt)
	return age >= 0 && age <= window
}

// AddPasskey enrols pk on a device. With pairingWindow set, the enrolment
// is the one a fresh pairing allows: it must still be open (see
// PairingEnrolOpen), and it is used up. A passkey whose id the device
// already has is replaced.
func (s *Store) AddPasskey(id string, pk Passkey, pairingWindow time.Duration) (Device, error) {
	s.mu.Lock()
	d := s.devs[id]
	if d == nil || d.Revoked() {
		s.mu.Unlock()
		return Device{}, ErrNotFound
	}
	if pairingWindow > 0 && !s.pairingOpen(d, pairingWindow) {
		s.mu.Unlock()
		return Device{}, ErrEnrolClosed
	}
	if slices.ContainsFunc(d.Passkeys, func(p Passkey) bool { return p.ID == pk.ID }) {
		// Replacing it would reset its sign count, and with it the check
		// for a cloned authenticator.
		s.mu.Unlock()
		return Device{}, ErrPasskeyExists
	}
	prev := *d
	prev.Passkeys = slices.Clone(d.Passkeys)
	if pk.Created.IsZero() {
		pk.Created = s.now()
	}
	d.Passkeys = append(slices.Clone(d.Passkeys), pk)
	if pairingWindow > 0 {
		d.PairEnrolUsed = true
	}
	if err := s.save(); err != nil {
		*d = prev
		s.mu.Unlock()
		return Device{}, err
	}
	out := d.Public()
	s.mu.Unlock()
	s.fire(Change{Kind: PasskeyAdded, Device: out})
	return out, nil
}

// UsedPasskey records a passkey's use: its updated credential record (the
// sign count moves on) and when. It is saved at once, so a cloned
// authenticator's replayed count is noticed even after a restart.
func (s *Store) UsedPasskey(id, passkeyID string, credential json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devs[id]
	if d == nil || d.Revoked() {
		return ErrNotFound
	}
	for i := range d.Passkeys {
		if d.Passkeys[i].ID == passkeyID {
			prev := d.Passkeys[i]
			d.Passkeys[i].Credential = slices.Clone(credential)
			d.Passkeys[i].LastUsed = s.now()
			if err := s.save(); err != nil {
				d.Passkeys[i] = prev
				return err
			}
			return nil
		}
	}
	return ErrPasskeyUnknown
}

// RemovedPasskey is one passkey taken away by RemovePasskeysEnrolled.
type RemovedPasskey struct {
	Device  Device    `json:"device"`
	ID      string    `json:"id"`
	RPID    string    `json:"rp_id,omitempty"`
	Created time.Time `json:"created"`
}

// RemovePasskeysEnrolled removes every passkey enrolled from `from` up to
// `to` (a zero `to` means up to now), on every device, and returns them,
// oldest first. The devices stay paired. It is what an alarm does about
// passkeys an impostor could have planted while it held the twin's name.
func (s *Store) RemovePasskeysEnrolled(from, to time.Time) ([]RemovedPasskey, error) {
	s.mu.Lock()
	if to.IsZero() {
		to = s.now()
	}
	var removed []RemovedPasskey
	prev := map[string][]Passkey{}
	for _, d := range s.devs {
		var keep []Passkey
		for _, pk := range d.Passkeys {
			if !pk.Created.Before(from) && !pk.Created.After(to) {
				removed = append(removed, RemovedPasskey{ID: pk.ID, RPID: pk.RPID, Created: pk.Created, Device: Device{ID: d.ID}})
				continue
			}
			keep = append(keep, pk)
		}
		if len(keep) != len(d.Passkeys) {
			prev[d.ID] = d.Passkeys
			d.Passkeys = keep
		}
	}
	if len(prev) == 0 {
		s.mu.Unlock()
		return nil, nil
	}
	if err := s.save(); err != nil {
		for id, pks := range prev {
			s.devs[id].Passkeys = pks
		}
		s.mu.Unlock()
		return nil, err
	}
	changed := make([]Device, 0, len(prev))
	for id := range prev {
		changed = append(changed, s.devs[id].Public())
	}
	for i := range removed {
		removed[i].Device = s.devs[removed[i].Device.ID].Public()
	}
	s.mu.Unlock()
	sort.Slice(removed, func(i, j int) bool { return removed[i].Created.Before(removed[j].Created) })
	for _, d := range changed {
		s.fire(Change{Kind: PasskeysRemoved, Device: d})
	}
	return removed, nil
}
