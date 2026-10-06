package server

import (
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/cryptobyte"
)

// Peek limits: the relay reads at most this much of a connection, for at
// most this long, before it knows where the connection goes.
const (
	MaxHello    = 16 << 10
	PeekTimeout = 5 * time.Second
)

// TLS constants the peek needs.
const (
	recordHandshake   = 22
	typeClientHello   = 1
	maxRecordPayload  = 1 << 14 // a plaintext record, RFC 8446 §5.1
	extServerName     = 0
	extALPN           = 16
	sniHostName       = 0
	maxServerNameSize = 255
)

// ErrNotClientHello means the connection did not start with a well-formed
// TLS ClientHello within the limits.
var ErrNotClientHello = errors.New("relay: not a TLS ClientHello")

// PeekClientHello reads a TLS ClientHello from c and returns its
// server_name and the ALPN protocols it offers. replay is every byte read
// from c, the hello and anything after it, which the caller forwards before
// anything else. It reads at most max bytes within d, and handles a hello
// split across TCP segments and TLS records, such as one carrying an
// X25519MLKEM768 key share. It never writes to c.
//
// sni is empty when the hello names no host. It is returned as sent: 1 to
// 255 visible ASCII characters with no trailing dot.
func PeekClientHello(c net.Conn, max int, d time.Duration) (sni string, alpn []string, replay []byte, err error) {
	if err := c.SetReadDeadline(time.Now().Add(d)); err != nil {
		return "", nil, nil, err
	}
	defer c.SetReadDeadline(time.Time{})

	buf := make([]byte, 0, min(max, 2048))
	var hs []byte // handshake bytes, reassembled from records
	next := 0     // start of the first record not yet in hs
	for {
		for len(buf)-next >= 5 {
			h := buf[next : next+5]
			n := int(h[3])<<8 | int(h[4])
			switch {
			case h[0] != recordHandshake:
				return "", nil, buf, fmt.Errorf("%w: record type %d", ErrNotClientHello, h[0])
			case h[1] != 3:
				return "", nil, buf, fmt.Errorf("%w: record version %#02x%02x", ErrNotClientHello, h[1], h[2])
			case n == 0 || n > maxRecordPayload:
				return "", nil, buf, fmt.Errorf("%w: record length %d", ErrNotClientHello, n)
			}
			if len(buf)-next-5 < n {
				break
			}
			hs = append(hs, buf[next+5:next+5+n]...)
			next += 5 + n
			if len(hs) < 4 {
				continue
			}
			if hs[0] != typeClientHello {
				return "", nil, buf, fmt.Errorf("%w: handshake type %d", ErrNotClientHello, hs[0])
			}
			body := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
			if 4+body > max {
				return "", nil, buf, fmt.Errorf("%w: %d-byte hello", ErrNotClientHello, 4+body)
			}
			if len(hs) >= 4+body {
				sni, alpn, err = parseClientHello(hs[4 : 4+body])
				return sni, alpn, buf, err
			}
		}
		if len(buf) >= max {
			return "", nil, buf, fmt.Errorf("%w: no hello in %d bytes", ErrNotClientHello, max)
		}
		if len(buf) == cap(buf) {
			grown := make([]byte, len(buf), min(2*cap(buf), max))
			copy(grown, buf)
			buf = grown
		}
		n, err := c.Read(buf[len(buf):min(cap(buf), max)])
		buf = buf[:len(buf)+n]
		if err != nil {
			return "", nil, buf, err
		}
	}
}

// parseClientHello reads a ClientHello body (RFC 8446 §4.1.2) by the same
// rules as crypto/tls, which parses it again on the daemon: every length
// must fit, nothing may trail, no extension may repeat, and there is at
// most one host_name. The relay is stricter only about the name itself.
func parseClientHello(b []byte) (sni string, alpn []string, err error) {
	bad := func(what string) error { return fmt.Errorf("%w: %s", ErrNotClientHello, what) }
	s := cryptobyte.String(b)
	var (
		version                        uint16
		random                         []byte
		sessionID, suites, compression cryptobyte.String
	)
	if !s.ReadUint16(&version) || !s.ReadBytes(&random, 32) ||
		!s.ReadUint8LengthPrefixed(&sessionID) ||
		!s.ReadUint16LengthPrefixed(&suites) || len(suites)%2 != 0 ||
		!s.ReadUint8LengthPrefixed(&compression) {
		return "", nil, bad("hello header")
	}
	if s.Empty() {
		return "", nil, nil // no extensions at all
	}
	var exts cryptobyte.String
	if !s.ReadUint16LengthPrefixed(&exts) || !s.Empty() {
		return "", nil, bad("extensions")
	}
	seen := make(map[uint16]bool)
	for !exts.Empty() {
		var typ uint16
		var data cryptobyte.String
		if !exts.ReadUint16(&typ) || !exts.ReadUint16LengthPrefixed(&data) {
			return "", nil, bad("extension")
		}
		if seen[typ] {
			return "", nil, bad(fmt.Sprintf("duplicate extension %d", typ))
		}
		seen[typ] = true
		switch typ {
		case extServerName:
			if sni, err = parseServerName(data); err != nil {
				return "", nil, err
			}
		case extALPN:
			if alpn, err = parseALPN(data); err != nil {
				return "", nil, err
			}
		}
	}
	return sni, alpn, nil
}

// parseServerName reads the server_name extension (RFC 6066 §3).
func parseServerName(data cryptobyte.String) (string, error) {
	var list cryptobyte.String
	if !data.ReadUint16LengthPrefixed(&list) || list.Empty() || !data.Empty() {
		return "", fmt.Errorf("%w: server_name", ErrNotClientHello)
	}
	var sni string
	for !list.Empty() {
		var typ uint8
		var name cryptobyte.String
		if !list.ReadUint8(&typ) || !list.ReadUint16LengthPrefixed(&name) || name.Empty() {
			return "", fmt.Errorf("%w: server_name entry", ErrNotClientHello)
		}
		if typ != sniHostName {
			continue
		}
		if sni != "" {
			return "", fmt.Errorf("%w: two host_names", ErrNotClientHello)
		}
		if !validServerName(name) {
			return "", fmt.Errorf("%w: host_name", ErrNotClientHello)
		}
		sni = string(name)
	}
	return sni, nil
}

// validServerName accepts 1 to 255 visible ASCII characters with no
// trailing dot, which RFC 6066 forbids and crypto/tls refuses.
func validServerName(b []byte) bool {
	if len(b) == 0 || len(b) > maxServerNameSize || b[len(b)-1] == '.' {
		return false
	}
	for _, c := range b {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// parseALPN reads the application_layer_protocol_negotiation extension
// (RFC 7301 §3.1).
func parseALPN(data cryptobyte.String) ([]string, error) {
	var list cryptobyte.String
	if !data.ReadUint16LengthPrefixed(&list) || list.Empty() || !data.Empty() {
		return nil, fmt.Errorf("%w: ALPN", ErrNotClientHello)
	}
	var out []string
	for !list.Empty() {
		var p cryptobyte.String
		if !list.ReadUint8LengthPrefixed(&p) || p.Empty() {
			return nil, fmt.Errorf("%w: ALPN entry", ErrNotClientHello)
		}
		out = append(out, string(p))
	}
	return out, nil
}

// lowerASCII lower-cases A-Z only. strings.ToLower also folds non-ASCII
// letters, such as the Kelvin sign into "k", which a name comparison must
// not do.
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
