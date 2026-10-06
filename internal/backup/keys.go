package backup

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"filippo.io/age"
)

// Key derivation (docs/backup-format.md has the golden vectors):
//
//	K    = the phrase's 128 bits of entropy (not the BIP-39 PBKDF2 seed)
//	seed = HKDF-SHA256(K, salt "antbot-backup-v1", info, 32 bytes)
//
// info "age-pq-seed" gives the age identity snapshots are encrypted to: the
// hybrid ML-KEM-768 + X25519 key the pinned filippo.io/age (v1.3) supports,
// so a backup made today stays private against a future quantum computer.
// info "age-x25519" gives a classic X25519 age identity, which also opens a
// snapshot whose config names an age1… recipient. info "recovery-ed25519"
// gives the Ed25519 recovery key, which signs handover markers (and will
// bind the Mirrin Cloud namespace).
const kdfSalt = "antbot-backup-v1"

const (
	infoPQ       = "age-pq-seed"
	infoX25519   = "age-x25519"
	infoRecovery = "recovery-ed25519"
)

func (p Phrase) derive(info string) []byte {
	k, err := hkdf.Key(sha256.New, p.k[:], []byte(kdfSalt), info, 32)
	if err != nil {
		panic("backup: hkdf: " + err.Error()) // only for lengths HKDF can't make
	}
	return k
}

// AgeIdentity is the post-quantum (ML-KEM-768 + X25519) age identity that
// opens this phrase's snapshots.
func (p Phrase) AgeIdentity() (*age.HybridIdentity, error) {
	return age.ParseHybridIdentity(strings.ToUpper(bech32Encode("AGE-SECRET-KEY-PQ-", p.derive(infoPQ))))
}

// X25519Identity is the classic age identity from the same words.
func (p Phrase) X25519Identity() (*age.X25519Identity, error) {
	return age.ParseX25519Identity(strings.ToUpper(bech32Encode("AGE-SECRET-KEY-", p.derive(infoX25519))))
}

// Identities are every age identity the words give, for decrypting.
func (p Phrase) Identities() []age.Identity {
	var ids []age.Identity
	if id, err := p.AgeIdentity(); err == nil {
		ids = append(ids, id)
	}
	if id, err := p.X25519Identity(); err == nil {
		ids = append(ids, id)
	}
	return ids
}

// Recipient is the public key backups are encrypted to (age1pq1…).
func (p Phrase) Recipient() string {
	id, err := p.AgeIdentity()
	if err != nil {
		return ""
	}
	return id.Recipient().String()
}

// Opens reports whether these words open backups encrypted to recipient.
func (p Phrase) Opens(recipient string) bool {
	if recipient == "" {
		return false
	}
	if recipient == p.Recipient() {
		return true
	}
	id, err := p.X25519Identity()
	return err == nil && recipient == id.Recipient().String()
}

// RecoveryKey is the Ed25519 recovery key.
func (p Phrase) RecoveryKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(p.derive(infoRecovery))
}

// RecoveryPub is the recovery public key as config keeps it: base64url,
// no padding.
func (p Phrase) RecoveryPub() string {
	return base64.RawURLEncoding.EncodeToString(p.RecoveryKey().Public().(ed25519.PublicKey))
}

// Namespace names this phrase's backups: the first 26 characters of the
// lower-case base32 SHA-256 of the recovery public key.
func (p Phrase) Namespace() string {
	return namespace(p.RecoveryKey().Public().(ed25519.PublicKey))
}

func namespace(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:26]
}

// ParseRecoveryPub reads a recovery public key as config keeps it.
func ParseRecoveryPub(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("backup.recovery_pub in config.yaml isn't a recovery key; run `mirrin backup init` again")
	}
	return ed25519.PublicKey(b), nil
}

// NamespaceOf is the namespace for a recovery public key from config.
func NamespaceOf(recoveryPub string) (string, error) {
	pub, err := ParseRecoveryPub(recoveryPub)
	if err != nil {
		return "", err
	}
	return namespace(pub), nil
}

// ParseRecipient reads an age recipient: age1pq1… (post-quantum) or age1….
func ParseRecipient(s string) (age.Recipient, error) {
	s = strings.TrimSpace(s)
	var (
		r   age.Recipient
		err error
	)
	switch {
	case s == "":
		return nil, errors.New("backups aren't set up yet; run `mirrin backup init`")
	case strings.HasPrefix(s, "age1pq1"):
		r, err = age.ParseHybridRecipient(s)
	default:
		r, err = age.ParseX25519Recipient(s)
	}
	if err != nil {
		return nil, fmt.Errorf("backup.recipient in config.yaml isn't an age key (%v); run `mirrin backup init` again", err)
	}
	return r, nil
}
