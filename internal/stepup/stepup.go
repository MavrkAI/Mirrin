// Package stepup asks for a passkey (Face ID, a fingerprint, a device PIN)
// before a paired device approves something that can't easily be undone.
//
// A stolen phone or a copied cookie holds a device's key, but not its
// owner's face: an approval of a dangerous action from another device needs
// a WebAuthn assertion with user verification, made for exactly that
// approval (its number, the decision and the stored input), in the last two
// minutes, and used once. Enrolling a passkey is gated too, or a thief would
// simply enrol their own: only just after pairing, with a passkey the device
// already has, or once the owner says so on this computer or in their own
// chat.
//
// What it can't do, stated plainly: a site that holds the twin's own name
// and a valid certificate (a "same-origin impostor") runs in the browser as
// the twin, and can ask the phone for a passkey the same way. Certificate
// Transparency watching and the alarm playbook are the answer to that (see
// docs/cloud-design.md §6.5), not this package.
package stepup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// Header carries a step-up session id on the retried request. Its name
// dates from before the rename and stays: a phone may still run the page
// script it cached then.
const Header = "AntBot-Stepup" // rename:keep

// Timings.
const (
	// SessionTTL is how long a step-up ceremony may take.
	SessionTTL = 2 * time.Minute
	// PairingWindow is how long after claiming a pairing offer a device may
	// enrol its first passkey without more proof (once).
	PairingWindow = 15 * time.Minute
	// OwnerGrantTTL is how long the owner's "yes, let it enrol" lasts.
	OwnerGrantTTL = 15 * time.Minute
	// usedKept is how long a finished session is remembered, so a replay
	// hears "already used" rather than "unknown".
	usedKept = 15 * time.Minute
	// perDevice caps the ceremonies one device may have open at once.
	perDevice = 16
	// perDeviceKept caps all the ceremonies one device has in memory,
	// open or remembered after use, so looping begin and finish can't
	// grow them without bound.
	perDeviceKept = 64
)

// Errors. The API turns each into an answer a person can act on.
var (
	ErrNoPasskey      = errors.New("this device has no passkey for this address")
	ErrNoGrant        = errors.New("this device may not enrol a passkey now")
	ErrNeedProof      = errors.New("this device already has a passkey; use it to add another")
	ErrSessionUnknown = errors.New("no such step-up session")
	ErrSessionUsed    = errors.New("this step-up was already used")
	ErrSessionExpired = errors.New("this step-up took too long")
	ErrMismatch       = errors.New("this step-up was for something else")
	ErrAssertion      = errors.New("the passkey check didn't pass")
	ErrHost           = errors.New("passkeys don't work at this address")
	ErrRevoked        = errors.New("this device was disconnected")
)

// Level is how much needs a passkey from another device (reach.step_up).
type Level string

// Levels. There is no "off": dangerous approvals from other devices always
// need a passkey.
const (
	Dangerous Level = "dangerous" // approving a dangerous action
	Write     Level = "write"     // approving anything that changes something
	All       Level = "all"       // every decision, a no included
)

// ParseLevel reads reach.step_up. Anything else (empty, or a value from a
// newer version) is the default, Dangerous: never less than that.
func ParseLevel(s string) Level {
	switch Level(strings.ToLower(strings.TrimSpace(s))) {
	case Write:
		return Write
	case All:
		return All
	}
	return Dangerous
}

// Required reports whether deciding (approve or deny) an approval of this
// risk (read, write, dangerous) from another device needs a passkey. A risk
// it doesn't know counts as dangerous.
func Required(level Level, risk, decision string) bool {
	if level == All {
		return true
	}
	if decision != "approve" {
		return false
	}
	switch risk {
	case "read":
		return false
	case "write":
		return level == Write
	}
	return true
}

// ApprovalChallenge is what a passkey signs to approve (or deny) approval
// id: SHA-256("antbot-approval-v1" 0x00 id(8 bytes, big-endian) decision
// SHA-256(input) nonce). A signature for one approval, one decision or one
// stored input is worth nothing for another.
func ApprovalChallenge(id int64, decision string, input []byte, nonce [32]byte) []byte {
	h := sha256.New()
	h.Write([]byte("antbot-approval-v1"))
	h.Write([]byte{0})
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(id))
	h.Write(n[:])
	h.Write([]byte(decision))
	in := sha256.Sum256(input)
	h.Write(in[:])
	h.Write(nonce[:])
	return h.Sum(nil)
}

// enrolChallenge is what an existing passkey signs to let its device enrol
// another.
func enrolChallenge(deviceID string, nonce [32]byte) []byte {
	h := sha256.New()
	h.Write([]byte("antbot-enrol-v1"))
	h.Write([]byte{0})
	h.Write([]byte(deviceID))
	h.Write(nonce[:])
	return h.Sum(nil)
}

// RP is the relying party a ceremony is for: the host name the device
// reached the twin at (the RP ID) and the page's origin.
type RP struct {
	ID     string // twin.example.ts.net
	Origin string // https://twin.example.ts.net[:port]
}

// RPFor makes the RP for a request that arrived at host (the Host header,
// already checked against the listener's names), over TLS or not.
func RPFor(host string, tls bool) RP {
	scheme := "http://"
	if tls {
		scheme = "https://"
	}
	h := host
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	return RP{ID: strings.ToLower(strings.TrimSuffix(h, ".")), Origin: scheme + strings.ToLower(host)}
}

func (rp RP) check() error {
	if protocol.ValidateRPID(rp.ID) != nil {
		return ErrHost
	}
	u, err := url.Parse(rp.Origin)
	if err != nil || u.Hostname() != rp.ID || (u.Scheme != "https" && !(u.Scheme == "http" && rp.ID == "localhost")) {
		return ErrHost // a passkey needs https, or http://localhost
	}
	return nil
}

// GrantKind is what allows an enrolment.
type GrantKind string

// Grants.
const (
	GrantPairing GrantKind = "pairing" // within PairingWindow of claiming a pairing offer, once
	GrantPasskey GrantKind = "passkey" // an assertion from a passkey the device already has
	GrantOwner   GrantKind = "owner"   // the owner said so, on this computer or in their chat
)

// EnrolGrant is permission to enrol a passkey, from Grant or
// FinishEnrolProof. It can't be made up: each kind is checked again when
// the passkey is saved.
type EnrolGrant struct {
	Kind  GrantKind
	token string // GrantPasskey: the proof FinishEnrolProof recorded
}

// Enrolment is a passkey just enrolled, for the owner to hear about.
type Enrolment struct {
	Device  devices.Device
	Passkey devices.Passkey // without its credential record
	Grant   GrantKind
	At      time.Time
}

type ceremony int

const (
	approval ceremony = iota + 1
	register
	enrolProof
)

type session struct {
	kind     ceremony
	device   string
	rp       RP
	data     webauthn.SessionData
	expires  time.Time
	used     bool
	usedAt   time.Time
	id       int64
	decision string
	nonce    [32]byte
	grant    EnrolGrant
}

type proof struct {
	device  string
	expires time.Time
}

// Verifier runs step-up ceremonies against the device registry. It is safe
// for concurrent use.
type Verifier struct {
	devs *devices.Store

	mu       sync.Mutex
	now      func() time.Time
	name     string
	allow    func(host string) bool
	sessions map[string]*session
	owner    map[string]time.Time // device id → until when the owner's grant lasts
	proofs   map[string]proof     // passkey-proof grants, by token
	hooks    []func(Enrolment)
}

// PasskeyRevoker is what the certificate alarm's playbook needs: every
// passkey enrolled in a window gone, since an impostor holding the twin's
// name then could have planted one.
type PasskeyRevoker interface {
	RevokePasskeysEnrolled(from, to time.Time) ([]devices.RemovedPasskey, error)
}

var _ PasskeyRevoker = (*Verifier)(nil)

// New is a verifier for the devices in devs. Revoking a device ends its
// ceremonies (the store removes its passkeys).
func New(devs *devices.Store) *Verifier {
	v := &Verifier{devs: devs, now: time.Now, name: "Mirrin", sessions: map[string]*session{}, owner: map[string]time.Time{}, proofs: map[string]proof{}}
	devs.OnChange(func(c devices.Change) {
		if c.Kind == devices.Revoked {
			v.forget(c.Device.ID)
		}
	})
	return v
}

// SetName is the twin's name, which the phone shows when enrolling.
func (v *Verifier) SetName(name string) {
	if name = strings.TrimSpace(name); name != "" {
		v.mu.Lock()
		v.name = name
		v.mu.Unlock()
	}
}

// SetClock replaces the clock (tests).
func (v *Verifier) SetClock(now func() time.Time) {
	v.mu.Lock()
	v.now = now
	v.mu.Unlock()
}

// AllowHosts limits the host names passkeys are made and checked for. The
// API passes only hosts its listener answers to; this is a second fence.
func (v *Verifier) AllowHosts(fn func(host string) bool) {
	v.mu.Lock()
	v.allow = fn
	v.mu.Unlock()
}

// OnEnrol registers fn to hear about every passkey enrolled.
func (v *Verifier) OnEnrol(fn func(Enrolment)) {
	v.mu.Lock()
	v.hooks = append(v.hooks, fn)
	v.mu.Unlock()
}

func (v *Verifier) checkRP(rp RP) error {
	if err := rp.check(); err != nil {
		return err
	}
	v.mu.Lock()
	allow := v.allow
	v.mu.Unlock()
	if allow != nil && !allow(rp.ID) {
		return ErrHost
	}
	return nil
}

// forget drops a device's ceremonies and grants.
func (v *Verifier) forget(deviceID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, s := range v.sessions {
		if s.device == deviceID {
			delete(v.sessions, k)
		}
	}
	delete(v.owner, deviceID)
	for k, p := range v.proofs {
		if p.device == deviceID {
			delete(v.proofs, k)
		}
	}
}

func (v *Verifier) webAuthn(rp RP) (*webauthn.WebAuthn, error) {
	v.mu.Lock()
	name := v.name
	v.mu.Unlock()
	t := webauthn.TimeoutConfig{Enforce: true, Timeout: SessionTTL, TimeoutUVD: SessionTTL}
	return webauthn.New(&webauthn.Config{
		RPID:                  rp.ID,
		RPDisplayName:         name,
		RPOrigins:             []string{rp.Origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
		},
		Timeouts: webauthn.TimeoutsConfig{Login: t, Registration: t},
	})
}

// user is a device as WebAuthn sees it, with its passkeys for one RP.
type user struct {
	dev   devices.Device
	creds []webauthn.Credential
	ids   map[string]string // credential id (raw) → the passkey's id
}

func (u *user) WebAuthnID() []byte                         { return []byte(u.dev.ID) }
func (u *user) WebAuthnName() string                       { return u.dev.Name }
func (u *user) WebAuthnDisplayName() string                { return u.dev.Name }
func (u *user) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// current reads a device afresh: a revoked or unknown one gets nowhere.
func (v *Verifier) current(dev devices.Device, rp RP) (*user, error) {
	d, ok := v.devs.Get(dev.ID)
	if !ok || d.Revoked() {
		return nil, ErrRevoked
	}
	u := &user{dev: d, ids: map[string]string{}}
	for _, pk := range v.devs.Passkeys(d.ID) {
		if pk.RPID != rp.ID {
			continue
		}
		var c webauthn.Credential
		if json.Unmarshal(pk.Credential, &c) != nil || len(c.ID) == 0 {
			continue
		}
		u.creds = append(u.creds, c)
		u.ids[string(c.ID)] = pk.ID
	}
	return u, nil
}

// HasPasskey reports whether the device has a passkey for rp.
func (v *Verifier) HasPasskey(dev devices.Device, rp RP) bool {
	u, err := v.current(dev, rp)
	return err == nil && len(u.creds) > 0
}

// Grant says what would let the device enrol a passkey now: the pairing
// window, or the owner's say-so. ErrNeedProof means it has a passkey here
// already, and must use it (BeginEnrolProof); ErrNoGrant, that nothing
// allows it.
func (v *Verifier) Grant(dev devices.Device, rp RP) (EnrolGrant, error) {
	if _, err := v.current(dev, rp); err != nil {
		return EnrolGrant{}, err
	}
	if v.devs.PairingEnrolOpen(dev.ID, PairingWindow) {
		return EnrolGrant{Kind: GrantPairing}, nil
	}
	v.mu.Lock()
	until, ok := v.owner[dev.ID]
	now := v.now()
	v.mu.Unlock()
	if ok && now.Before(until) {
		return EnrolGrant{Kind: GrantOwner}, nil
	}
	if v.HasPasskey(dev, rp) {
		return EnrolGrant{}, ErrNeedProof
	}
	return EnrolGrant{}, ErrNoGrant
}

// AllowEnrolment is the owner's say-so, given on this computer or in their
// own chat: the device may enrol one passkey in the next OwnerGrantTTL.
func (v *Verifier) AllowEnrolment(deviceID string) error {
	d, ok := v.devs.Get(deviceID)
	if !ok || d.Revoked() {
		return devices.ErrNotFound
	}
	v.mu.Lock()
	v.owner[deviceID] = v.now().Add(OwnerGrantTTL)
	v.mu.Unlock()
	return nil
}

// valid reports whether a grant still holds for the device.
func (v *Verifier) valid(dev string, g EnrolGrant) bool {
	if g.Kind == GrantPairing {
		return v.devs.PairingEnrolOpen(dev, PairingWindow)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.heldLocked(dev, g, v.now())
}

// heldLocked is valid for the grants the verifier keeps. Callers hold mu.
func (v *Verifier) heldLocked(dev string, g EnrolGrant, now time.Time) bool {
	switch g.Kind {
	case GrantOwner:
		until, ok := v.owner[dev]
		return ok && now.Before(until)
	case GrantPasskey:
		p, ok := v.proofs[g.token]
		return ok && p.device == dev && now.Before(p.expires)
	}
	return false
}

func newSessionID() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic("stepup: no randomness: " + err.Error())
	}
	return "su_" + base64.RawURLEncoding.EncodeToString(b)
}

func newNonce() (n [32]byte) {
	if _, err := rand.Read(n[:]); err != nil {
		panic("stepup: no randomness: " + err.Error())
	}
	return n
}

// open records a ceremony. Callers hold mu.
func (v *Verifier) openLocked(s *session) (string, error) {
	now := v.now()
	n, kept := 0, 0
	for k, o := range v.sessions {
		switch {
		case o.used && now.Sub(o.usedAt) > usedKept, !o.used && now.After(o.expires.Add(usedKept)):
			delete(v.sessions, k)
		case o.device == s.device && !o.used && now.Before(o.expires):
			n++
			kept++
		case o.device == s.device:
			kept++ // finished (or lapsed), remembered against a replay
		}
	}
	for k, p := range v.proofs {
		if now.After(p.expires) {
			delete(v.proofs, k)
		}
	}
	for k, until := range v.owner {
		if now.After(until) {
			delete(v.owner, k)
		}
	}
	if n >= perDevice || kept >= perDeviceKept {
		return "", errors.New("too many passkey checks at once; wait a minute and try again")
	}
	s.expires = now.Add(SessionTTL)
	id := newSessionID()
	v.sessions[id] = s
	return id, nil
}

// take spends a session: it is used from here on, whatever happens next.
func (v *Verifier) take(id string, dev devices.Device, kind ceremony) (*session, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.sessions[id]
	switch {
	case s == nil:
		return nil, ErrSessionUnknown
	case s.device != dev.ID || s.kind != kind:
		// Another device's, or another kind of check: not spent, so
		// someone who learns the id can't burn the owner's check.
		return nil, ErrSessionUnknown
	case s.used:
		return nil, ErrSessionUsed
	}
	s.used, s.usedAt = true, v.now()
	switch {
	case s.usedAt.After(s.expires):
		return nil, ErrSessionExpired
	}
	return s, nil
}

func marshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}

// BeginApproval starts the check for deciding approval id (decision is
// approve or deny) from a device: the options for navigator.credentials.get
// (PublicKeyCredentialRequestOptions) and the session id to send back.
// input is the approval's stored input, which the challenge is bound to.
func (v *Verifier) BeginApproval(rp RP, dev devices.Device, id int64, decision string, input []byte) (json.RawMessage, string, error) {
	if err := v.checkRP(rp); err != nil {
		return nil, "", err
	}
	u, err := v.current(dev, rp)
	if err != nil {
		return nil, "", err
	}
	if len(u.creds) == 0 {
		return nil, "", ErrNoPasskey
	}
	wa, err := v.webAuthn(rp)
	if err != nil {
		return nil, "", err
	}
	nonce := newNonce()
	opts, data, err := wa.BeginLogin(u, webauthn.WithChallenge(ApprovalChallenge(id, decision, input, nonce)), webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	v.mu.Lock()
	sid, err := v.openLocked(&session{kind: approval, device: dev.ID, rp: rp, data: *data, id: id, decision: decision, nonce: nonce})
	v.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	b, err := marshal(opts.Response)
	return b, sid, err
}

// FinishApproval checks the assertion in body (the PublicKeyCredential as
// JSON) for session sid: it must come from this device, for approval id and
// this decision, while the stored input is still input, with the user
// verified, within SessionTTL, and only once.
func (v *Verifier) FinishApproval(dev devices.Device, sid string, id int64, decision string, input []byte, body io.Reader) error {
	s, err := v.take(sid, dev, approval)
	if err != nil {
		return err
	}
	if s.id != id || s.decision != decision {
		return ErrMismatch
	}
	want := ApprovalChallenge(id, decision, input, s.nonce)
	got, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s.data.Challenge, "="))
	if err != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		return ErrMismatch // the stored input changed since the check began
	}
	_, err = v.assert(dev, s, body)
	return err
}

// assert checks an assertion against the session and records the
// passkey's use.
func (v *Verifier) assert(dev devices.Device, s *session, body io.Reader) (string, error) {
	u, err := v.current(dev, s.rp)
	if err != nil {
		return "", err
	}
	if len(u.creds) == 0 {
		return "", ErrNoPasskey
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(io.LimitReader(body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrAssertion, err)
	}
	wa, err := v.webAuthn(s.rp)
	if err != nil {
		return "", err
	}
	cred, err := wa.ValidateLogin(u, s.data, parsed)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrAssertion, err)
	}
	if !parsed.Response.AuthenticatorData.Flags.UserVerified() {
		return "", fmt.Errorf("%w: the user wasn't verified", ErrAssertion)
	}
	if cred.Authenticator.CloneWarning {
		return "", fmt.Errorf("%w: the passkey's counter went backwards, as a copied one's would", ErrAssertion)
	}
	pkID := u.ids[string(cred.ID)]
	if rec, err := json.Marshal(cred); err == nil {
		_ = v.devs.UsedPasskey(dev.ID, pkID, rec)
	}
	return pkID, nil
}

// BeginEnrolProof starts the check with an existing passkey that lets a
// device enrol another.
func (v *Verifier) BeginEnrolProof(rp RP, dev devices.Device) (json.RawMessage, string, error) {
	if err := v.checkRP(rp); err != nil {
		return nil, "", err
	}
	u, err := v.current(dev, rp)
	if err != nil {
		return nil, "", err
	}
	if len(u.creds) == 0 {
		return nil, "", ErrNoPasskey
	}
	wa, err := v.webAuthn(rp)
	if err != nil {
		return nil, "", err
	}
	nonce := newNonce()
	opts, data, err := wa.BeginLogin(u, webauthn.WithChallenge(enrolChallenge(dev.ID, nonce)), webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	v.mu.Lock()
	sid, err := v.openLocked(&session{kind: enrolProof, device: dev.ID, rp: rp, data: *data, nonce: nonce})
	v.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	b, err := marshal(opts.Response)
	return b, sid, err
}

// FinishEnrolProof checks that assertion, and returns the grant it gives.
func (v *Verifier) FinishEnrolProof(dev devices.Device, sid string, body io.Reader) (EnrolGrant, error) {
	s, err := v.take(sid, dev, enrolProof)
	if err != nil {
		return EnrolGrant{}, err
	}
	got, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s.data.Challenge, "="))
	if err != nil || !bytes.Equal(got, enrolChallenge(dev.ID, s.nonce)) {
		return EnrolGrant{}, ErrMismatch
	}
	if _, err := v.assert(dev, s, body); err != nil {
		return EnrolGrant{}, err
	}
	tok := newSessionID()
	v.mu.Lock()
	v.proofs[tok] = proof{device: dev.ID, expires: v.now().Add(SessionTTL)}
	v.mu.Unlock()
	return EnrolGrant{Kind: GrantPasskey, token: tok}, nil
}

// BeginRegistration starts enrolling a passkey on a device, with a grant
// from Grant or FinishEnrolProof: the options for navigator.credentials.create
// (PublicKeyCredentialCreationOptions) and the session id.
func (v *Verifier) BeginRegistration(rp RP, dev devices.Device, grant EnrolGrant) (json.RawMessage, string, error) {
	if err := v.checkRP(rp); err != nil {
		return nil, "", err
	}
	u, err := v.current(dev, rp)
	if err != nil {
		return nil, "", err
	}
	if !v.valid(dev.ID, grant) {
		return nil, "", ErrNoGrant
	}
	wa, err := v.webAuthn(rp)
	if err != nil {
		return nil, "", err
	}
	opts, data, err := wa.BeginRegistration(u, webauthn.WithExclusions(webauthn.Credentials(u.creds).CredentialDescriptors()))
	if err != nil {
		return nil, "", err
	}
	v.mu.Lock()
	sid, err := v.openLocked(&session{kind: register, device: dev.ID, rp: rp, data: *data, grant: grant})
	v.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	b, err := marshal(opts.Response)
	return b, sid, err
}

// FinishRegistration checks the new credential in body and enrols it, if
// the grant still holds; the grant is used up. Everyone listening (OnEnrol)
// hears about it.
func (v *Verifier) FinishRegistration(dev devices.Device, sid string, body io.Reader) (devices.Passkey, error) {
	s, err := v.take(sid, dev, register)
	if err != nil {
		return devices.Passkey{}, err
	}
	u, err := v.current(dev, s.rp)
	if err != nil {
		return devices.Passkey{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(io.LimitReader(body, 64<<10))
	if err != nil {
		return devices.Passkey{}, fmt.Errorf("%w: %v", ErrAssertion, err)
	}
	wa, err := v.webAuthn(s.rp)
	if err != nil {
		return devices.Passkey{}, err
	}
	cred, err := wa.CreateCredential(u, s.data, parsed)
	if err != nil {
		return devices.Passkey{}, fmt.Errorf("%w: %v", ErrAssertion, err)
	}
	if !cred.Flags.UserVerified {
		return devices.Passkey{}, fmt.Errorf("%w: the user wasn't verified", ErrAssertion)
	}
	rec, err := json.Marshal(cred)
	if err != nil {
		return devices.Passkey{}, err
	}
	enc := base64.RawURLEncoding.EncodeToString
	pk := devices.Passkey{ID: enc(cred.ID), PublicKey: enc(cred.PublicKey), RPID: s.rp.ID, Grant: string(s.grant.Kind), Credential: rec}
	// The grant is used up as the passkey is saved, so two enrolments racing
	// on one grant can't both get through: the owner's and a passkey's here,
	// the pairing window's in the store, with the save.
	v.mu.Lock()
	now := v.now()
	window := time.Duration(0)
	var restore func()
	switch s.grant.Kind {
	case GrantPairing:
		window = PairingWindow
	case GrantOwner, GrantPasskey:
		if !v.heldLocked(dev.ID, s.grant, now) {
			v.mu.Unlock()
			return devices.Passkey{}, ErrNoGrant
		}
		if s.grant.Kind == GrantOwner {
			until := v.owner[dev.ID]
			delete(v.owner, dev.ID)
			restore = func() { v.owner[dev.ID] = until }
		} else {
			p := v.proofs[s.grant.token]
			delete(v.proofs, s.grant.token)
			restore = func() { v.proofs[s.grant.token] = p }
		}
	default:
		v.mu.Unlock()
		return devices.Passkey{}, ErrNoGrant
	}
	hooks := append([]func(Enrolment){}, v.hooks...)
	v.mu.Unlock()
	pk.Created = now
	saved, err := v.devs.AddPasskey(dev.ID, pk, window)
	if err != nil {
		if restore != nil {
			v.mu.Lock()
			restore() // nothing was enrolled: the grant still stands
			v.mu.Unlock()
		}
		if errors.Is(err, devices.ErrEnrolClosed) {
			return devices.Passkey{}, ErrNoGrant
		}
		return devices.Passkey{}, err
	}
	shown := pk
	shown.Credential = nil
	e := Enrolment{Device: saved, Passkey: shown, Grant: s.grant.Kind, At: now}
	for _, h := range hooks {
		h(e)
	}
	return shown, nil
}

// RevokePasskeysEnrolled removes every passkey enrolled from `from` to `to`
// (zero: until now), ends the ceremonies of the devices that had them, and
// withdraws every enrolment grant the owner gave. The devices stay paired.
func (v *Verifier) RevokePasskeysEnrolled(from, to time.Time) ([]devices.RemovedPasskey, error) {
	removed, err := v.devs.RemovePasskeysEnrolled(from, to)
	if err != nil {
		return nil, err
	}
	for _, r := range removed {
		v.forget(r.Device.ID)
	}
	v.mu.Lock()
	clear(v.owner)
	clear(v.proofs)
	v.mu.Unlock()
	return removed, nil
}
