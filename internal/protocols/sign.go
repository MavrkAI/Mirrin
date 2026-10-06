package protocols

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/blake2b"

	"github.com/MavrkAI/Mirrin/registry"
)

// The default pack index is signed with minisign: registry/index.json.minisig
// sits beside it, made by the registry-sign workflow, and the public key is
// built in (registry/minisign.pub). An index whose signature doesn't check
// out is not used; search and updates answer from the built-in index instead.

// minisignKey is a minisign public key: its id and the Ed25519 key.
type minisignKey struct {
	id [8]byte
	pk ed25519.PublicKey
}

// registryKey is the built-in key the default index is checked against;
// tests replace it. Without one, only the built-in index is trusted.
var registryKey, _ = parseMinisignKey(registry.PublicKey)

var errNoRegistryKey = errors.New("this version has no key to check the pack index with")

// parseMinisignKey reads a minisign public key file (or just its base64 line).
func parseMinisignKey(text []byte) (*minisignKey, error) {
	for _, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "untrusted comment:") || strings.HasPrefix(line, "#") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(raw) != 2+8+ed25519.PublicKeySize || string(raw[:2]) != "Ed" {
			return nil, errors.New("not a minisign public key")
		}
		k := &minisignKey{pk: ed25519.PublicKey(raw[10:])}
		copy(k.id[:], raw[2:10])
		return k, nil
	}
	return nil, errNoRegistryKey
}

// verifyMinisign checks data against a minisign signature file made with
// key: both the signature over the file (legacy "Ed", or prehashed "ED") and
// the global one over the signature and its trusted comment.
func verifyMinisign(key *minisignKey, data, sigFile []byte) error {
	if key == nil {
		return errNoRegistryKey
	}
	bad := errors.New("the pack index signature doesn't match")
	lines := strings.Split(strings.ReplaceAll(string(sigFile), "\r\n", "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "untrusted comment:") {
		return bad
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(sig) != 2+8+ed25519.SignatureSize {
		return bad
	}
	trusted, ok := strings.CutPrefix(lines[2], "trusted comment: ")
	if !ok {
		return bad
	}
	global, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || len(global) != ed25519.SignatureSize {
		return bad
	}
	if !bytes.Equal(sig[2:10], key.id[:]) {
		return errors.New("the pack index was signed with a different key")
	}
	msg := data
	switch string(sig[:2]) {
	case "Ed":
	case "ED":
		h := blake2b.Sum512(data)
		msg = h[:]
	default:
		return bad
	}
	if !ed25519.Verify(key.pk, msg, sig[10:]) {
		return bad
	}
	if !ed25519.Verify(key.pk, append(append([]byte{}, sig[10:]...), trusted...), global) {
		return bad
	}
	return nil
}
