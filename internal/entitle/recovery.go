package entitle

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"time"
)

// The recovery key comes from the owner's 12 backup words and never
// leaves the machine the words were typed on (docs/cloud-design.md §9). It
// signs two things for Mirrin Cloud, and the control plane checks them
// against the recovery public key the namespace was bound with:
//
//   - binding a backup namespace to the account of the device that sends
//     it, once (POST /v1/backup/namespaces), and
//   - a recovery: moving the account to a new device key on a new machine
//     (POST /v1/recover).
//
// Each message starts with its own context line, so a signature for one
// can never pass as the other, or as a handover marker.

// Namespace is the backup namespace of a recovery public key: the first 26
// characters of the lower-case base32 of its SHA-256.
func Namespace(recoveryPub ed25519.PublicKey) string {
	sum := sha256.Sum256(recoveryPub)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:26]
}

// BindMessage is what the recovery key signs to bind namespace ns to the
// account of the device whose key is devicePub.
func BindMessage(ns, recoveryPub, devicePub string) []byte {
	return []byte("antbot-backup-namespace-v1\n" + ns + "\n" + recoveryPub + "\n" + devicePub)
}

// RecoverMessage is what the recovery key signs to move the account that
// holds ns to devicePub, naming the ACME account its handle's CAA record
// should pin from now ("" pins none until the new machine names one), at
// ts (RFC 3339, to the second).
func RecoverMessage(ns, devicePub, acmeAccount string, ts time.Time) []byte {
	return []byte("antbot-recover-v1\n" + ns + "\n" + devicePub + "\n" + acmeAccount + "\n" + ts.UTC().Format(time.RFC3339))
}
