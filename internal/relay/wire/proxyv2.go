package wire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/netip"
)

// PROXY protocol version 2, as HAProxy specifies it in
// https://www.haproxy.org/download/3.0/doc/proxy-protocol.txt section 2.2.
// A relay writes one header at the start of every data stream; the daemon
// reads it before handing the stream on as a connection.

// TLV types the tunnel uses.
const (
	TLVAuthority = 0x02 // PP2_TYPE_AUTHORITY: the client's SNI
	TLVCRC32C    = 0x03 // PP2_TYPE_CRC32C: checked when present, never sent
	TLVRelayID   = 0xE0 // PP2_TYPE_MIN_CUSTOM: the relay's id
)

// MaxProxyHeader bounds a whole header, fixed part and TLVs. The headers a
// relay writes are under 400 bytes.
const MaxProxyHeader = 1024

var (
	// ErrNotProxy means the stream does not start with the v2 signature.
	ErrNotProxy = errors.New("wire: not a PROXY v2 header")
	// ErrProxyHeader means a v2 header is malformed or unsupported.
	ErrProxyHeader = errors.New("wire: bad PROXY v2 header")
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// proxySig is the 12-byte v2 signature.
var proxySig = [12]byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}

const (
	cmdLocal = 0x20 // version 2, LOCAL
	cmdProxy = 0x21 // version 2, PROXY
	tcp4     = 0x11 // AF_INET, STREAM
	tcp6     = 0x21 // AF_INET6, STREAM
	unspec   = 0x00
	maxAuth  = 255
)

// ProxyHeader is what a PROXY v2 header says about one relayed connection.
type ProxyHeader struct {
	// Local is the LOCAL command: a connection the proxy made itself, with
	// no addresses. Relays never send one; a daemon refuses it.
	Local bool
	// Source is the client's address and Dest the address it connected
	// to. IPv4 addresses are never returned in their IPv6-mapped form.
	Source, Dest netip.AddrPort
	Authority    string // TLV 0x02; empty when absent
	RelayID      string // TLV 0xE0; empty when absent
}

// WriteProxyV2 writes h in a single Write. Both addresses IPv4 gives a
// TCP-over-IPv4 header; otherwise it is TCP over IPv6, with any IPv4
// address mapped.
func WriteProxyV2(w io.Writer, h ProxyHeader) error {
	if h.Authority != "" && !validAuthority(h.Authority) {
		return fmt.Errorf("%w: authority", ErrProxyHeader)
	}
	if h.RelayID != "" && !ValidRelayID(h.RelayID) {
		return fmt.Errorf("%w: relay id", ErrProxyHeader)
	}
	b := make([]byte, 16, 16+36+6+len(h.Authority)+len(h.RelayID))
	copy(b, proxySig[:])
	if h.Local {
		b[12], b[13] = cmdLocal, unspec
	} else {
		src, dst := h.Source.Addr(), h.Dest.Addr()
		if !src.IsValid() || !dst.IsValid() || src.Zone() != "" || dst.Zone() != "" {
			return fmt.Errorf("%w: addresses", ErrProxyHeader)
		}
		src, dst = src.Unmap(), dst.Unmap()
		b[12] = cmdProxy
		if src.Is4() && dst.Is4() {
			b[13] = tcp4
			s, d := src.As4(), dst.As4()
			b = append(append(b, s[:]...), d[:]...)
		} else {
			b[13] = tcp6
			s, d := src.As16(), dst.As16()
			b = append(append(b, s[:]...), d[:]...)
		}
		b = binary.BigEndian.AppendUint16(b, h.Source.Port())
		b = binary.BigEndian.AppendUint16(b, h.Dest.Port())
	}
	b = appendTLV(b, TLVAuthority, h.Authority)
	b = appendTLV(b, TLVRelayID, h.RelayID)
	binary.BigEndian.PutUint16(b[14:16], uint16(len(b)-16))
	_, err := w.Write(b)
	return err
}

func appendTLV(b []byte, typ byte, v string) []byte {
	if v == "" {
		return b
	}
	b = append(b, typ)
	b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
	return append(b, v...)
}

// ReadProxyV2 reads one header from r and nothing after it; the stream's
// data follows in r. It accepts the PROXY command over TCP on IPv4 or IPv6
// and the LOCAL command, skips TLVs it does not use, refuses duplicates of
// those it does, and checks a CRC32C TLV when one is present.
func ReadProxyV2(r *bufio.Reader) (ProxyHeader, error) {
	var fixed [16]byte
	if _, err := io.ReadFull(r, fixed[:12]); err != nil {
		return ProxyHeader{}, err
	}
	if [12]byte(fixed[:12]) != proxySig {
		return ProxyHeader{}, ErrNotProxy
	}
	if _, err := io.ReadFull(r, fixed[12:]); err != nil {
		return ProxyHeader{}, unexpected(err)
	}
	n := int(binary.BigEndian.Uint16(fixed[14:16]))
	if 16+n > MaxProxyHeader {
		return ProxyHeader{}, fmt.Errorf("%w: %d bytes", ErrProxyHeader, 16+n)
	}
	b := make([]byte, 16+n)
	copy(b, fixed[:])
	if _, err := io.ReadFull(r, b[16:]); err != nil {
		return ProxyHeader{}, unexpected(err)
	}
	return parseProxyV2(b)
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// parseProxyV2 parses a whole header: the fixed 16 bytes and the length
// they announce.
func parseProxyV2(b []byte) (ProxyHeader, error) {
	var h ProxyHeader
	var addrLen int
	switch b[12] {
	case cmdProxy:
		switch b[13] {
		case tcp4:
			addrLen = 12
		case tcp6:
			addrLen = 36
		default:
			return h, fmt.Errorf("%w: family/transport %#02x", ErrProxyHeader, b[13])
		}
	case cmdLocal:
		// The receiver ignores LOCAL addresses but must skip them.
		h.Local = true
		switch b[13] >> 4 {
		case 0:
			addrLen = 0
		case 1:
			addrLen = 12
		case 2:
			addrLen = 36
		case 3:
			addrLen = 216
		default:
			return h, fmt.Errorf("%w: family %#02x", ErrProxyHeader, b[13])
		}
		if b[13]&0x0f > 2 {
			return h, fmt.Errorf("%w: transport %#02x", ErrProxyHeader, b[13])
		}
	default:
		return h, fmt.Errorf("%w: version/command %#02x", ErrProxyHeader, b[12])
	}
	body := b[16:]
	if len(body) < addrLen {
		return h, fmt.Errorf("%w: short address block", ErrProxyHeader)
	}
	if !h.Local {
		if addrLen == 12 {
			h.Source = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[0:4])), binary.BigEndian.Uint16(body[8:10]))
			h.Dest = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[4:8])), binary.BigEndian.Uint16(body[10:12]))
		} else {
			h.Source = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[0:16])).Unmap(), binary.BigEndian.Uint16(body[32:34]))
			h.Dest = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[16:32])).Unmap(), binary.BigEndian.Uint16(body[34:36]))
		}
	}
	var seen [256]bool
	crcAt := -1
	for off := 16 + addrLen; off < len(b); {
		if len(b)-off < 3 {
			return h, fmt.Errorf("%w: truncated TLV", ErrProxyHeader)
		}
		typ, l := b[off], int(binary.BigEndian.Uint16(b[off+1:off+3]))
		v := off + 3
		if len(b)-v < l {
			return h, fmt.Errorf("%w: truncated TLV %#02x", ErrProxyHeader, typ)
		}
		val := b[v : v+l]
		switch typ {
		case TLVAuthority, TLVRelayID, TLVCRC32C:
			if seen[typ] {
				return h, fmt.Errorf("%w: duplicate TLV %#02x", ErrProxyHeader, typ)
			}
			seen[typ] = true
		}
		switch typ {
		case TLVAuthority:
			if !validAuthority(string(val)) {
				return h, fmt.Errorf("%w: authority", ErrProxyHeader)
			}
			h.Authority = string(val)
		case TLVRelayID:
			if !ValidRelayID(string(val)) {
				return h, fmt.Errorf("%w: relay id", ErrProxyHeader)
			}
			h.RelayID = string(val)
		case TLVCRC32C:
			if l != 4 {
				return h, fmt.Errorf("%w: CRC32C length %d", ErrProxyHeader, l)
			}
			crcAt = v
		}
		off = v + l
	}
	if crcAt >= 0 {
		want := binary.BigEndian.Uint32(b[crcAt:])
		c := crc32.New(castagnoli)
		c.Write(b[:crcAt])
		c.Write([]byte{0, 0, 0, 0})
		c.Write(b[crcAt+4:])
		if c.Sum32() != want {
			return h, fmt.Errorf("%w: CRC32C mismatch", ErrProxyHeader)
		}
	}
	return h, nil
}

// validAuthority accepts an SNI as it travels: 1 to 255 visible ASCII
// characters.
func validAuthority(s string) bool {
	if len(s) == 0 || len(s) > maxAuth {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
