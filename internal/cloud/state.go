package cloud

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// This machine's link lives in data/cloud, a 0700 directory that exists only
// once the user has run `mirrin cloud link`. Every file in it is 0600:
//
//	device.key          the Ed25519 device key, PKCS #8 PEM; never backed up
//	link.json           which control plane, handle and generation, and what
//	                    the control plane last said about this machine
//	entitlement.paseto  the current entitlement
//	pending-link.json   a checkout started but not finished, so it can be
//	                    resumed rather than paid twice
//	egress.jsonl        the egress ledger (ledger.go)
const (
	dirName         = "cloud"
	keyFile         = "device.key"
	linkFile        = "link.json"
	entitlementFile = "entitlement.paseto"
	pendingFile     = "pending-link.json"

	maxKeyFile  = 4 << 10
	maxLinkFile = 64 << 10
)

// StateKind is where this machine stands with Mirrin Cloud.
type StateKind int

const (
	// None: never linked. The default; nothing is shown and nothing is
	// contacted.
	None StateKind = iota
	// Active: paid and current.
	Active
	// Grace: paid_through has passed but the entitlement has not expired.
	// Everything keeps working; the tray shows one calm line.
	Grace
	// Expired: the entitlement has expired, or the control plane revoked
	// this machine. Relays refuse it.
	Expired
	// Superseded: another machine holds the handle at a higher generation,
	// and this one stands by.
	Superseded
)

func (k StateKind) String() string {
	switch k {
	case None:
		return "none"
	case Active:
		return "active"
	case Grace:
		return "grace"
	case Expired:
		return "expired"
	case Superseded:
		return "superseded"
	}
	return fmt.Sprintf("StateKind(%d)", int(k))
}

// State reads and writes this machine's link. It keeps nothing in memory:
// each call reads the files, so a CLI that unlinks while the daemon runs is
// seen at once. It is safe for concurrent use.
type State struct {
	dir  string
	keys map[string]ed25519.PublicKey
	mu   sync.Mutex // one read-modify-write of link.json at a time
}

// Info is what link.json says, for `mirrin cloud status`.
type Info struct {
	API        string    // the control plane this machine linked with
	Handle     string    // from the entitlement, such as "ember-otter-42"
	Gen        int64     // the handle generation this machine holds
	LinkedAt   time.Time // when the link completed
	CheckedAt  time.Time // when the control plane last answered a link or refresh, by this machine's clock
	Superseded *Supersede
	RevokedAt  time.Time // when the control plane said this machine is revoked
	DeleteAt   time.Time // when the account is due to be deleted, if asked
}

// Supersede records that another machine took the handle.
type Supersede struct {
	Gen int64     `json:"gen"`
	At  time.Time `json:"at"`
}

// linkRecord is link.json.
type linkRecord struct {
	API        string     `json:"api"`
	Handle     string     `json:"handle"`
	Gen        int64      `json:"gen"`
	LinkedAt   time.Time  `json:"linked_at"`
	CheckedAt  time.Time  `json:"checked_at,omitzero"`
	Superseded *Supersede `json:"superseded,omitempty"`
	RevokedAt  time.Time  `json:"revoked_at,omitzero"`
	DeleteAt   time.Time  `json:"delete_at,omitzero"`
}

// OpenState returns this machine's link state under dataDir, verifying
// entitlements against the keys compiled into this build. It creates and
// contacts nothing.
func OpenState(dataDir string) (*State, error) {
	return openState(dataDir, entitle.EntitlementKeys)
}

// OpenStateWithKeys is OpenState verifying against keys instead, for a
// control plane whose keys are not compiled in, such as a test's fake.
func OpenStateWithKeys(dataDir string, keys map[string]ed25519.PublicKey) (*State, error) {
	return openState(dataDir, keys)
}

func openState(dataDir string, keys map[string]ed25519.PublicKey) (*State, error) {
	dir, err := cloudDir(dataDir, false)
	if err != nil {
		return nil, err
	}
	return &State{dir: dir, keys: keys}, nil
}

// Linked reports whether this machine has completed a link.
func (s *State) Linked() bool {
	rec, err := s.record()
	return err == nil && rec != nil
}

// Info returns what link.json says, and false when this machine never
// linked.
func (s *State) Info() (Info, bool, error) {
	rec, err := s.record()
	if err != nil || rec == nil {
		return Info{}, false, err
	}
	return Info{API: rec.API, Handle: rec.Handle, Gen: rec.Gen, LinkedAt: rec.LinkedAt, CheckedAt: rec.CheckedAt,
		Superseded: rec.Superseded, RevokedAt: rec.RevokedAt, DeleteAt: rec.DeleteAt}, true, nil
}

// Current returns the entitlement and the state at now. A token that does
// not verify, or is bound to another key than this machine's, counts as
// Expired: linked, but good for nothing.
func (s *State) Current(now time.Time) (entitle.Claims, StateKind) {
	rec, err := s.record()
	if err != nil || rec == nil {
		return entitle.Claims{}, None
	}
	tok, err := s.Entitlement()
	if err != nil || tok == "" {
		return entitle.Claims{}, Expired
	}
	c, err := entitle.Inspect(tok, s.keys)
	if err != nil {
		return entitle.Claims{}, Expired
	}
	if rec.Superseded != nil && rec.Superseded.Gen > c.Gen {
		return c, Superseded
	}
	if !rec.RevokedAt.IsZero() || !s.boundHere(c) {
		return c, Expired
	}
	if _, err := entitle.Verify(tok, s.keys, now); err != nil {
		return c, Expired // expired, or so far ahead that this clock is wrong
	}
	if now.Before(c.PaidThrough) {
		return c, Active
	}
	return c, Grace
}

// Has reports whether the entitlement grants feature at now: only while
// Active or in Grace.
func (s *State) Has(now time.Time, feature string) bool {
	c, k := s.Current(now)
	return (k == Active || k == Grace) && c.Has(feature)
}

// Entitlement returns the stored token, or "" when there is none.
func (s *State) Entitlement() (string, error) {
	b, err := readFileMax(filepath.Join(s.dir, entitlementFile), entitle.MaxTokenSize+2)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// boundHere reports whether c is bound to this machine's device key.
func (s *State) boundHere(c entitle.Claims) bool {
	priv, err := loadKey(s.dir)
	if err != nil {
		return false
	}
	k, err := c.Key()
	return err == nil && subtle.ConstantTimeCompare(k, priv.Public().(ed25519.PublicKey)) == 1
}

// record reads link.json, or returns nil when this machine is not linked:
// it never linked, or its device key is gone. A link.json left without a key
// (an unlink in another process that raced a refresh) is good for nothing,
// and counts as no link, so `mirrin cloud link` can start again.
func (s *State) record() (*linkRecord, error) {
	b, err := readFileMax(filepath.Join(s.dir, linkFile), maxLinkFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec, err := parseLinkRecord(b)
	if err != nil {
		return nil, err
	}
	if ok, err := s.hasKey(); err != nil || !ok {
		return nil, err
	}
	return rec, nil
}

// hasKey reports whether the device key file exists.
func (s *State) hasKey() (bool, error) {
	_, err := os.Lstat(filepath.Join(s.dir, keyFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// stored returns the claims of the stored entitlement if it verifies, the
// times aside.
func (s *State) stored() (entitle.Claims, bool) {
	tok, err := s.Entitlement()
	if err != nil || tok == "" {
		return entitle.Claims{}, false
	}
	c, err := entitle.Inspect(tok, s.keys)
	return c, err == nil
}

// heldGen is the handle generation this machine holds: the higher of
// link.json's and the stored entitlement's.
func (s *State) heldGen(rec *linkRecord) int64 {
	if c, ok := s.stored(); ok && c.Gen > rec.Gen {
		return c.Gen
	}
	return rec.Gen
}

// parseLinkRecord reads link.json strictly.
func parseLinkRecord(b []byte) (*linkRecord, error) {
	var rec linkRecord
	if err := json.Unmarshal(b, &rec, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("cloud: %s: %w", linkFile, err)
	}
	api, err := parseAPI(rec.API)
	if err != nil || api != rec.API {
		return nil, fmt.Errorf("cloud: %s: api %q is not a control plane origin", linkFile, clip(rec.API, 80))
	}
	if rec.Gen < 0 || rec.Superseded != nil && rec.Superseded.Gen < 1 {
		return nil, fmt.Errorf("cloud: %s: bad generation", linkFile)
	}
	return &rec, nil
}

// update applies fn to link.json under the lock and writes it back. It
// fails with ErrNotLinked when there is no link, so an answer that arrives
// after `mirrin cloud unlink` does not bring the link back.
func (s *State) update(fn func(*linkRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.record()
	if err != nil {
		return err
	}
	if rec == nil {
		return ErrNotLinked
	}
	fn(rec)
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, linkFile), b)
}

// saveEntitlement stores a verified token, records the link it belongs to,
// and notes now (this machine's clock) as when the control plane last
// answered; the next refresh is timed from that, never from the token's
// iat. A token that would move the machine to another handle, back a
// generation, or back in time (an iat before the stored token's) is
// refused: only a new link or a recovery changes the handle or generation.
// A token at the generation of a recorded supersede, or later, ends it: the
// control plane has handed the handle back to this machine.
func (s *State) saveEntitlement(api, tok string, c entitle.Claims, now time.Time, create bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.record()
	if err != nil {
		return err
	}
	now = now.UTC().Truncate(time.Second)
	if rec == nil {
		if !create {
			return ErrNotLinked
		}
		rec = &linkRecord{LinkedAt: now}
	} else {
		held, ok := s.stored()
		if c.Gen < rec.Gen || rec.Handle != "" && c.Handle != rec.Handle || ok && (c.Gen < held.Gen || c.Iat.Before(held.Iat)) {
			return fmt.Errorf("cloud: refused an entitlement for %q at generation %d issued %s: this machine holds %q at generation %d issued %s",
				c.Handle, c.Gen, c.Iat.UTC().Format(time.RFC3339), rec.Handle, max(rec.Gen, held.Gen), held.Iat.UTC().Format(time.RFC3339))
		}
	}
	rec.API, rec.Handle, rec.Gen = api, c.Handle, c.Gen
	rec.CheckedAt = now
	rec.RevokedAt = time.Time{}
	if rec.Superseded != nil && c.Gen >= rec.Superseded.Gen {
		rec.Superseded = nil
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// An unlink in another process may have removed the key since record
	// read it; writing now would leave a link without one.
	if ok, err := s.hasKey(); err != nil || !ok {
		return errors.Join(ErrNotLinked, err)
	}
	if err := writeFileAtomic(filepath.Join(s.dir, entitlementFile), []byte(tok+"\n")); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, linkFile), b)
}

// pendingLink is pending-link.json.
type pendingLink struct {
	API         string    `json:"api"`
	ID          string    `json:"id"`
	CheckoutURL string    `json:"checkout_url"`
	Started     time.Time `json:"started"`
}

// pending reads pending-link.json, or returns nil when there is none.
func (s *State) pending() (*pendingLink, error) {
	b, err := readFileMax(filepath.Join(s.dir, pendingFile), maxLinkFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p pendingLink
	if err := json.Unmarshal(b, &p, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("cloud: %s: %w", pendingFile, err)
	}
	if err := checkID(p.ID); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *State) savePending(p pendingLink) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, pendingFile), b)
}

func (s *State) clearPending() error {
	if err := os.Remove(filepath.Join(s.dir, pendingFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// forget removes the link: the device key, the entitlement and link.json.
// The egress ledger stays; it is the user's record of what was sent.
func (s *State) forget() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, f := range []string{linkFile, entitlementFile, pendingFile, keyFile} {
		if err := os.Remove(filepath.Join(s.dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// cloudDir is dataDir/cloud, created 0700 when create is set.
func cloudDir(dataDir string, create bool) (string, error) {
	if dataDir == "" {
		return "", errors.New("cloud: no data directory")
	}
	dir := filepath.Join(dataDir, dirName)
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// loadKey reads the device key. A key that others can read is refused, as
// ssh refuses one, rather than used.
func loadKey(dir string) (ed25519.PrivateKey, error) {
	p := filepath.Join(dir, keyFile)
	fi, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotLinked
	}
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("cloud: %s is readable by others (mode %v); run chmod 600 on it", p, fi.Mode().Perm())
	}
	b, err := readFileMax(p, maxKeyFile)
	if err != nil {
		return nil, err
	}
	return parseKey(b)
}

// parseKey reads exactly one PKCS #8 PEM block holding an Ed25519 key.
func parseKey(b []byte) (ed25519.PrivateKey, error) {
	bad := errors.New("cloud: device key is not one Ed25519 PKCS #8 PEM block")
	blk, rest := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" || len(blk.Headers) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, bad
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, bad
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, bad
	}
	return priv, nil
}

// createKey makes the device key unless one exists, and returns whichever
// is there. It never replaces a key: a new file is linked into place, which
// fails if another process got there first.
func createKey(dataDir string) (ed25519.PrivateKey, error) {
	dir, err := cloudDir(dataDir, true)
	if err != nil {
		return nil, err
	}
	if k, err := loadKey(dir); !errors.Is(err, ErrNotLinked) {
		return k, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	tmp, err := writeTemp(dir, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, filepath.Join(dir, keyFile)); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return loadKey(dir)
}

// readFileMax reads a file of at most max bytes.
func readFileMax(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("cloud: %s is larger than %d bytes", p, max)
	}
	return b, nil
}

// writeFileAtomic replaces p with b, 0600, through a synced temporary file.
func writeFileAtomic(p string, b []byte) error {
	tmp, err := writeTemp(filepath.Dir(p), b)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeTemp writes b to a new 0600 file in dir, synced, and returns its path.
func writeTemp(dir string, b []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// saveRecovered records a recovery that brought no entitlement (payment has
// lapsed): the machine is linked, at the handle and generation, and
// Expired, so it can still fetch its backups.
func (s *State) saveRecovered(api, handle string, gen int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, err := s.record(); err != nil || rec != nil {
		return errors.Join(err, errors.New("cloud: this machine is already linked"))
	}
	if ok, err := s.hasKey(); err != nil || !ok {
		return errors.Join(ErrNotLinked, err)
	}
	now = now.UTC().Truncate(time.Second)
	b, err := json.Marshal(&linkRecord{API: api, Handle: handle, Gen: gen, LinkedAt: now, CheckedAt: now})
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, linkFile), b)
}
