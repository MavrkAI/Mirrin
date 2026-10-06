// Package dns writes each handle's records in the tenant zone: A and AAAA
// for the relays, and a CAA set that lets only the handle's own ACME
// account, by TLS-ALPN-01, obtain a certificate (docs/cloud-design.md
// §6.2). Route 53 is the provider; Fake stands in for it in --dev mode and
// tests. Records are written at link, relink and ACME-account change, never
// on the renewal path.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Provider writes and removes a handle's records. Both calls are
// idempotent.
type Provider interface {
	// UpsertHandle makes the handle's A, AAAA and CAA sets exactly these.
	UpsertHandle(ctx context.Context, handle string, v4, v6 []netip.Addr, caa []CAA) error
	// DeleteHandle removes the handle's records; none is not an error.
	DeleteHandle(ctx context.Context, handle string) error
}

// CAA is one CAA record (RFC 8659).
type CAA struct {
	Flag  uint8
	Tag   string // issue, issuewild or iodef
	Value string
}

// String is the record's presentation form: 0 issue "letsencrypt.org".
func (c CAA) String() string {
	return fmt.Sprintf("%d %s %q", c.Flag, c.Tag, c.Value)
}

// CA is the one certificate authority a handle's CAA names.
const CA = "letsencrypt.org"

// HandleCAA is the CAA set for a handle. With an ACME account, only that
// Let's Encrypt account may issue, and only by TLS-ALPN-01; without one,
// nobody may issue until the daemon names its account. Wildcards are never
// allowed. iodef, if set, is where CAs report refused requests.
func HandleCAA(acmeAccount, iodef string) ([]CAA, error) {
	issue := ";"
	if acmeAccount != "" {
		if !safeValue(acmeAccount) {
			return nil, fmt.Errorf("dns: ACME account %q cannot go into a CAA record", acmeAccount)
		}
		issue = CA + "; accounturi=" + acmeAccount + "; validationmethods=tls-alpn-01"
	}
	out := []CAA{{Tag: "issue", Value: issue}, {Tag: "issuewild", Value: ";"}}
	if iodef != "" {
		if !safeValue(iodef) || !strings.HasPrefix(iodef, "mailto:") && !strings.HasPrefix(iodef, "https://") {
			return nil, fmt.Errorf("dns: iodef %q is not a mailto: or https: URL fit for CAA", iodef)
		}
		out = append(out, CAA{Tag: "iodef", Value: iodef})
	}
	return out, nil
}

// safeValue accepts printable ASCII without quotes, backslashes, semicolons
// or spaces, which would change the record's meaning.
func safeValue(s string) bool {
	if s == "" || len(s) > 512 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x21 || c > 0x7e || c == '"' || c == '\\' || c == ';' {
			return false
		}
	}
	return true
}

// Label checks that handle is one DNS label of a-z, 0-9 and inner hyphens.
func Label(handle string) error {
	if len(handle) == 0 || len(handle) > 63 || handle[0] == '-' || handle[len(handle)-1] == '-' ||
		strings.Trim(handle, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return fmt.Errorf("dns: %q is not a lowercase DNS label", handle)
	}
	return nil
}

// checkAddrs insists v4 holds IPv4 addresses and v6 IPv6 ones.
func checkAddrs(v4, v6 []netip.Addr) error {
	for _, a := range v4 {
		if !a.Is4() {
			return fmt.Errorf("dns: %s is not an IPv4 address", a)
		}
	}
	for _, a := range v6 {
		if !a.Is6() || a.Is4In6() || a.Zone() != "" {
			return fmt.Errorf("dns: %s is not an IPv6 address", a)
		}
	}
	if len(v4) == 0 && len(v6) == 0 {
		return errors.New("dns: a handle needs at least one relay address")
	}
	return nil
}
