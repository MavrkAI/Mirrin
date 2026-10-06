package wire

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The HAProxy specification has no binary examples, so these headers are
// assembled by hand from section 2.2 of
// https://www.haproxy.org/download/3.0/doc/proxy-protocol.txt, one field
// per line.
var (
	goldenV4 = unhex(`
		0d0a0d0a000d0a515549540a  # signature
		21                        # version 2, PROXY
		11                        # AF_INET, STREAM
		001a                      # 26 bytes follow
		cb007109                  # src 203.0.113.9
		c6336401                  # dst 198.51.100.1
		c822                      # src port 51234
		01bb                      # dst port 443
		02 0006 682e74657374      # PP2_TYPE_AUTHORITY "h.test"
		e0 0002 7231              # PP2_TYPE_MIN_CUSTOM "r1"`)
	goldenV6 = unhex(`
		0d0a0d0a000d0a515549540a  # signature
		21                        # version 2, PROXY
		21                        # AF_INET6, STREAM
		0032                      # 50 bytes follow
		20010db8000000000000000000000009  # src 2001:db8::9
		20010db8000000000000000000000001  # dst 2001:db8::1
		c822                      # src port 51234
		01bb                      # dst port 443
		02 0006 682e74657374      # PP2_TYPE_AUTHORITY "h.test"
		e0 0002 7231              # PP2_TYPE_MIN_CUSTOM "r1"`)
)

// unhex decodes hex written in spaced groups; "#" starts a comment.
func unhex(s string) []byte {
	var b []byte
	for line := range strings.Lines(s) {
		line, _, _ = strings.Cut(line, "#")
		d, err := hex.DecodeString(strings.Join(strings.Fields(line), ""))
		if err != nil {
			panic(err)
		}
		b = append(b, d...)
	}
	return b
}

var (
	v4Header = ProxyHeader{
		Source:    netip.MustParseAddrPort("203.0.113.9:51234"),
		Dest:      netip.MustParseAddrPort("198.51.100.1:443"),
		Authority: "h.test",
		RelayID:   "r1",
	}
	v6Header = ProxyHeader{
		Source:    netip.MustParseAddrPort("[2001:db8::9]:51234"),
		Dest:      netip.MustParseAddrPort("[2001:db8::1]:443"),
		Authority: "h.test",
		RelayID:   "r1",
	}
)

func TestProxyV2Golden(t *testing.T) {
	for _, c := range []struct {
		name   string
		h      ProxyHeader
		golden []byte
	}{{"ipv4", v4Header, goldenV4}, {"ipv6", v6Header, goldenV6}} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteProxyV2(&buf, c.h); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), c.golden) {
				t.Fatalf("encoding:\n got %x\nwant %x", buf.Bytes(), c.golden)
			}
			// The header is read exactly: the ClientHello bytes behind it
			// stay in the reader.
			r := bufio.NewReader(io.MultiReader(bytes.NewReader(c.golden), strings.NewReader("\x16\x03\x01rest")))
			h, err := ReadProxyV2(r)
			if err != nil {
				t.Fatal(err)
			}
			if h != c.h {
				t.Fatalf("decoded %+v, want %+v", h, c.h)
			}
			if rest, _ := io.ReadAll(r); string(rest) != "\x16\x03\x01rest" {
				t.Fatalf("stream after the header: %q", rest)
			}
		})
	}
}

func TestProxyV2Mapped(t *testing.T) {
	// IPv4 in IPv6 form is written as TCP over IPv4 and read back as IPv4.
	h := v4Header
	h.Source = netip.AddrPortFrom(netip.AddrFrom16(h.Source.Addr().As16()), h.Source.Port())
	var buf bytes.Buffer
	if err := WriteProxyV2(&buf, h); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), goldenV4) {
		t.Fatalf("mapped source not written as IPv4: %x", buf.Bytes())
	}
	// Mixed families go as IPv6, and the IPv4 side comes back unmapped.
	mixed := ProxyHeader{Source: netip.MustParseAddrPort("203.0.113.9:1"), Dest: netip.MustParseAddrPort("[2001:db8::1]:443")}
	buf.Reset()
	if err := WriteProxyV2(&buf, mixed); err != nil {
		t.Fatal(err)
	}
	if buf.Bytes()[13] != 0x21 {
		t.Fatalf("mixed families: family byte %#x", buf.Bytes()[13])
	}
	got, err := ReadProxyV2(bufio.NewReader(&buf))
	if err != nil || got != mixed {
		t.Fatalf("mixed round trip: %v %+v", err, got)
	}
}

func TestProxyV2Local(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteProxyV2(&buf, ProxyHeader{Local: true}); err != nil {
		t.Fatal(err)
	}
	if want := unhex("0d0a0d0a000d0a515549540a 20 00 0000"); !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got %x, want %x", buf.Bytes(), want)
	}
	// A LOCAL header's addresses are skipped, whatever the family.
	in := unhex("0d0a0d0a000d0a515549540a 20 11 000c 01020304 05060708 0001 0002")
	h, err := ReadProxyV2(bufio.NewReader(bytes.NewReader(in)))
	if err != nil || h != (ProxyHeader{Local: true}) {
		t.Fatalf("LOCAL with addresses: %v %+v", err, h)
	}
}

// crc32c fills in a PP2_TYPE_CRC32C TLV appended to hdr, as a sender does.
func withCRC(hdr []byte) []byte {
	b := append(bytes.Clone(hdr), TLVCRC32C, 0, 4, 0, 0, 0, 0)
	binary.BigEndian.PutUint16(b[14:16], uint16(len(b)-16))
	binary.BigEndian.PutUint32(b[len(b)-4:], crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)))
	return b
}

func TestProxyV2CRC32C(t *testing.T) {
	checkRFC3720Vectors(t)
	good := withCRC(goldenV4)
	if h, err := ReadProxyV2(bufio.NewReader(bytes.NewReader(good))); err != nil || h != v4Header {
		t.Fatalf("valid CRC32C refused: %v", err)
	}
	for i := range good {
		if i >= 14 && i < 16 {
			continue // changing the length changes what is read, not the CRC
		}
		bad := bytes.Clone(good)
		bad[i] ^= 0x01
		if _, err := ReadProxyV2(bufio.NewReader(bytes.NewReader(bad))); err == nil {
			t.Fatalf("bit flip at byte %d accepted", i)
		}
	}
}

// checkRFC3720Vectors pins the polynomial the PROXY spec names (CRC32c as
// in RFC 4960 appendix B) against the CRC-32C examples of RFC 3720
// appendix B.4, stored verbatim in testdata.
func checkRFC3720Vectors(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile("testdata/rfc3720-b4.txt")
	if err != nil {
		t.Fatal(err)
	}
	var crcs [][]byte
	var pdu []byte
	inPDU := false
	for line := range strings.Lines(string(raw)) {
		f := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "#"): // our provenance header
		case len(f) == 5 && f[0] == "CRC:":
			crcs = append(crcs, unhex(strings.Join(f[1:], "")))
			inPDU = false
		case strings.Contains(line, "SCSI Read (10) Command PDU"):
			inPDU = true
		case inPDU && len(f) == 5 && f[0] != "Byte:" && strings.HasSuffix(f[0], ":"):
			pdu = append(pdu, unhex(strings.Join(f[1:], ""))...)
		}
	}
	inc, dec := make([]byte, 32), make([]byte, 32)
	for i := range inc {
		inc[i], dec[i] = byte(i), byte(31-i)
	}
	inputs := [][]byte{make([]byte, 32), bytes.Repeat([]byte{0xff}, 32), inc, dec, pdu}
	if len(crcs) != len(inputs) || len(pdu) != 48 {
		t.Fatalf("parsed %d CRCs and a %d-byte PDU from the RFC text", len(crcs), len(pdu))
	}
	for i, in := range inputs {
		if got, want := crc32.Checksum(in, castagnoli), binary.LittleEndian.Uint32(crcs[i]); got != want {
			t.Errorf("RFC 3720 B.4 example %d: CRC-32C %#08x, want %#08x", i+1, got, want)
		}
	}
}

func TestReadProxyV2Rejects(t *testing.T) {
	set := func(i int, v byte) []byte { b := bytes.Clone(goldenV4); b[i] = v; return b }
	withLen := func(b []byte, n int) []byte {
		b = bytes.Clone(b)
		binary.BigEndian.PutUint16(b[14:16], uint16(n))
		return b
	}
	withTLV := func(tlv string) []byte {
		b := append(bytes.Clone(goldenV4), unhex(tlv)...)
		return withLen(b, len(b)-16)
	}
	cases := map[string]struct {
		in   []byte
		want error
	}{
		"v1 text header":      {[]byte("PROXY TCP4 203.0.113.9 198.51.100.1 51234 443\r\n"), ErrNotProxy},
		"bad signature":       {set(11, 0x0b), ErrNotProxy},
		"version 1":           {set(12, 0x11), ErrProxyHeader},
		"version 3":           {set(12, 0x31), ErrProxyHeader},
		"command 2":           {set(12, 0x22), ErrProxyHeader},
		"UDP over IPv4":       {set(13, 0x12), ErrProxyHeader},
		"UNIX stream":         {set(13, 0x31), ErrProxyHeader},
		"PROXY with UNSPEC":   {set(13, 0x00), ErrProxyHeader},
		"LOCAL family 4":      {unhex("0d0a0d0a000d0a515549540a 20 41 0000"), ErrProxyHeader},
		"LOCAL transport 3":   {unhex("0d0a0d0a000d0a515549540a 20 13 000c 010203040506070800010002"), ErrProxyHeader},
		"short address block": {withLen(goldenV4[:16+11], 11), ErrProxyHeader},
		"IPv6 too short":      {withLen(set(13, 0x21), 26), ErrProxyHeader},
		"truncated TLV head":  {withTLV("04 00"), ErrProxyHeader},
		"truncated TLV body":  {withTLV("04 0005 0102"), ErrProxyHeader},
		"duplicate authority": {withTLV("02 0001 78"), ErrProxyHeader},
		"duplicate relay id":  {withTLV("e0 0002 7232"), ErrProxyHeader},
		"CRC32C of 3 bytes":   {withTLV("03 0003 000000"), ErrProxyHeader},
		"wrong CRC32C":        {withTLV("03 0004 00000000"), ErrProxyHeader},
		"too long":            {withLen(goldenV4, MaxProxyHeader-15), ErrProxyHeader},
		"truncated":           {goldenV4[:30], io.ErrUnexpectedEOF},
		"truncated fixed":     {goldenV4[:14], io.ErrUnexpectedEOF},
		"empty":               {nil, io.EOF},
	}
	// Invalid values in TLVs the tunnel uses.
	auth := func(v string) []byte {
		h := ProxyHeader{Source: v4Header.Source, Dest: v4Header.Dest}
		var buf bytes.Buffer
		WriteProxyV2(&buf, h)
		b := append(buf.Bytes(), TLVAuthority, 0, byte(len(v)))
		b = append(b, v...)
		return withLen(b, len(b)-16)
	}
	cases["space in authority"] = struct {
		in   []byte
		want error
	}{auth("h test"), ErrProxyHeader}
	cases["empty authority"] = struct {
		in   []byte
		want error
	}{auth(""), ErrProxyHeader}
	cases["NUL in authority"] = struct {
		in   []byte
		want error
	}{auth("h\x00"), ErrProxyHeader}
	for name, c := range cases {
		_, err := ReadProxyV2(bufio.NewReader(bytes.NewReader(c.in)))
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func TestReadProxyV2SkipsUnknownTLVs(t *testing.T) {
	b := append(bytes.Clone(goldenV4), unhex("04 0003 000000  01 0002 6832  e1 0001 ff  f0 0000")...)
	binary.BigEndian.PutUint16(b[14:16], uint16(len(b)-16))
	h, err := ReadProxyV2(bufio.NewReader(bytes.NewReader(b)))
	if err != nil || h != v4Header {
		t.Fatalf("%v %+v", err, h)
	}
}

func TestWriteProxyV2Rejects(t *testing.T) {
	for name, h := range map[string]ProxyHeader{
		"no source":         {Dest: v4Header.Dest},
		"zoned address":     {Source: netip.MustParseAddrPort("[fe80::1%en0]:1"), Dest: v6Header.Dest},
		"bad relay id":      {Source: v4Header.Source, Dest: v4Header.Dest, RelayID: "R1"},
		"long authority":    {Source: v4Header.Source, Dest: v4Header.Dest, Authority: strings.Repeat("a", 256)},
		"space authority":   {Source: v4Header.Source, Dest: v4Header.Dest, Authority: "a b"},
		"unicode authority": {Source: v4Header.Source, Dest: v4Header.Dest, Authority: "é.test"},
	} {
		if err := WriteProxyV2(io.Discard, h); !errors.Is(err, ErrProxyHeader) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// The largest header this package writes stays well under the limit.
	var buf bytes.Buffer
	big := v6Header
	big.Authority = strings.Repeat("a", 255)
	big.RelayID = strings.Repeat("r", 32)
	if err := WriteProxyV2(&buf, big); err != nil || buf.Len() > 536 {
		t.Fatalf("largest header: %v, %d bytes", err, buf.Len())
	}
}

// The package stays standard-library only: the daemon and the relay both
// import it.
func TestStdlibOnly(t *testing.T) {
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	out, err := exec.Command(gotool, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range strings.Fields(string(out)) {
		if p != "github.com/MavrkAI/Mirrin/internal/relay/wire" {
			t.Errorf("non-stdlib dependency: %s", p)
		}
	}
}
