// Package storage is where Mirrin Cloud keeps backup objects: ciphertext
// only, under ns/<namespace>/<name>. The control plane never moves the
// bytes itself. It hands the daemon a short-lived presigned URL for one
// PUT, GET or DELETE of one key, and checks with a HEAD what arrived before
// it counts an object as stored (docs/cloud-design.md §9).
//
// R2 is the production provider (r2.go, signed with internal/sigv4); Fake
// (fake.go) is a disk-backed stand-in with HMAC-signed URLs for --dev mode
// and tests, so nothing here needs a vendor account to build or test.
package storage

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Operations a presigned URL can be for.
const (
	OpPut    = "put"
	OpGet    = "get"
	OpDelete = "delete"
)

// Signed is a presigned request: the daemon sends Method to URL with
// Headers, before Expires.
type Signed struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Expires time.Time         `json:"expires"`
}

// Obj is one stored object.
type Obj struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Provider stores objects. Keys come only from Key, so a provider never
// sees a name a daemon made up.
type Provider interface {
	// Presign returns a URL for one op on key, good for ttl. A put URL is
	// for exactly size bytes where the provider can enforce it; the
	// control plane checks the size with Head either way.
	Presign(ctx context.Context, op, key string, size int64, ttl time.Duration) (Signed, error)
	// Head returns the size of the object at key, or ErrNotFound.
	Head(ctx context.Context, key string) (int64, error)
	// List returns every object whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Obj, error)
	// DeletePrefix removes every object whose key starts with prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// ErrNotFound is Head's answer for a key that holds nothing.
var ErrNotFound = errors.New("storage: not found")

// Object names are the backup engine's: a snapshot or a handover marker,
// with a time and eight hex digits, and nothing else.
var nameRE = regexp.MustCompile(`^(snap|handover)-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}\.age$`)

// ValidName reports whether name is an object name the control plane
// accepts.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Namespaces are 26 characters of lowercase base32.
var nsRE = regexp.MustCompile(`^[a-z2-7]{26}$`)

// ValidNamespace reports whether ns is a namespace.
func ValidNamespace(ns string) bool { return nsRE.MatchString(ns) }

// Prefix is where a namespace's objects are: ns/<ns>/.
func Prefix(ns string) string { return "ns/" + ns + "/" }

// Key is the one key an object of a namespace is kept under:
// ns/<ns>/<name>. It fails for anything but a namespace and a valid name.
func Key(ns, name string) (string, error) {
	if !ValidNamespace(ns) || !ValidName(name) {
		return "", errors.New("storage: bad namespace or object name")
	}
	return Prefix(ns) + name, nil
}

// NameOf is the object name in key, the part after the namespace's prefix.
func NameOf(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[i+1:]
	}
	return key
}

// methodOf is the HTTP method for an op.
func methodOf(op string) (string, error) {
	switch op {
	case OpPut:
		return "PUT", nil
	case OpGet:
		return "GET", nil
	case OpDelete:
		return "DELETE", nil
	}
	return "", errors.New("storage: op must be put, get or delete")
}
