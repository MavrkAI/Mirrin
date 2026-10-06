package server

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/cryptobyte"
)

// rfc8448Hellos reads the ClientHello records stored verbatim from RFC 8448
// in testdata: each block after a "# [name]" line, as its hex octets.
func rfc8448Hellos(t testing.TB) map[string][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/rfc8448-clienthello.txt")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	declared := map[string]int{} // the RFC's own "(N octets)"
	var name string
	for line := range strings.SplitSeq(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "# ["); ok {
			name = rest[:strings.IndexByte(rest, ']')]
			continue
		}
		if name == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if before, after, ok := strings.Cut(line, " octets):"); ok {
			n, err := strconv.Atoi(before[strings.LastIndexByte(before, '(')+1:])
			if err != nil {
				t.Fatal(err)
			}
			declared[name], line = n, after
		}
		for _, f := range strings.Fields(line) {
			if v, err := hex.DecodeString(f); err == nil && len(f) == 2 {
				out[name] = append(out[name], v...)
			}
		}
	}
	if len(out) != 2 {
		t.Fatalf("read %d RFC 8448 hellos, want 2", len(out))
	}
	for n, rec := range out {
		if len(rec) != declared[n] {
			t.Fatalf("%s: %d octets, the RFC says %d", n, len(rec), declared[n])
		}
	}
	return out
}

// feed returns a conn whose peer writes chunks one at a time, pausing
// between them, then waits.
func feed(t testing.TB, pause time.Duration, chunks ...[]byte) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() {
		for _, c := range chunks {
			if _, err := b.Write(c); err != nil {
				return
			}
			time.Sleep(pause)
		}
	}()
	return noWrite{Conn: a, t: t}
}

// noWrite fails the test if the relay ever writes while peeking.
type noWrite struct {
	net.Conn
	t testing.TB
}

func (c noWrite) Write(p []byte) (int, error) {
	c.t.Errorf("PeekClientHello wrote %d bytes", len(p))
	return 0, errors.New("no writes")
}

// Official vectors: both RFC 8448 ClientHellos name "server", whole or a
// byte at a time.
func TestPeekRFC8448(t *testing.T) {
	for name, rec := range rfc8448Hellos(t) {
		for _, split := range []bool{false, true} {
			var chunks [][]byte
			if split {
				for i := range rec {
					chunks = append(chunks, rec[i:i+1])
				}
			} else {
				chunks = [][]byte{rec}
			}
			sni, alpn, replay, err := PeekClientHello(feed(t, 0, chunks...), MaxHello, 5*time.Second)
			if err != nil || sni != "server" || alpn != nil || !bytes.Equal(replay, rec) {
				t.Errorf("%s (split %v): sni %q alpn %v replay %d/%d bytes, err %v", name, split, sni, alpn, len(replay), len(rec), err)
			}
		}
	}
}

// Hellos from crypto/tls clients, including the X25519MLKEM768 key share
// that makes a hello larger than one typical TCP segment.
func TestPeekGoHellos(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *tls.Config
		sni  string
		alpn []string
	}{
		{"h2", &tls.Config{ServerName: "h.test", NextProtos: []string{"h2", "http/1.1"}}, "h.test", []string{"h2", "http/1.1"}},
		{"http/1.1", &tls.Config{ServerName: "H.Test", NextProtos: []string{"http/1.1"}}, "H.Test", []string{"http/1.1"}},
		{"acme-tls/1", &tls.Config{ServerName: "h.test", NextProtos: []string{"acme-tls/1"}}, "h.test", []string{"acme-tls/1"}},
		{"mlkem", &tls.Config{ServerName: "ember-otter-42.mirrin.link", CurvePreferences: []tls.CurveID{tls.X25519MLKEM768}}, "ember-otter-42.mirrin.link", nil},
		{"no sni", &tls.Config{InsecureSkipVerify: true}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := clientHello(t, tc.cfg)
			if tc.name == "mlkem" && len(rec) < 1216 {
				t.Fatalf("hello is %d bytes; no ML-KEM share?", len(rec))
			}
			sni, alpn, replay, err := PeekClientHello(feed(t, 0, rec), MaxHello, 5*time.Second)
			if err != nil || sni != tc.sni || !slices.Equal(alpn, tc.alpn) || !bytes.Equal(replay, rec) {
				t.Fatalf("sni %q alpn %v err %v", sni, alpn, err)
			}
		})
	}
}

// Acceptance: a ClientHello with an X25519MLKEM768 key share split across 3
// TCP writes parses.
func TestPeekMLKEMAcrossThreeTCPWrites(t *testing.T) {
	rec := clientHello(t, &tls.Config{ServerName: "h.test", NextProtos: []string{"h2"}, CurvePreferences: []tls.CurveID{tls.X25519MLKEM768}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer c.Close()
		third := len(rec) / 3
		for _, part := range [][]byte{rec[:third], rec[third : 2*third], rec[2*third:]} {
			c.Write(part)
			time.Sleep(30 * time.Millisecond) // separate segments, separate reads
		}
		time.Sleep(time.Second)
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	reads := &countReads{Conn: c}
	sni, alpn, replay, err := PeekClientHello(reads, MaxHello, 5*time.Second)
	if err != nil || sni != "h.test" || !slices.Equal(alpn, []string{"h2"}) || !bytes.Equal(replay, rec) {
		t.Fatalf("sni %q alpn %v err %v", sni, alpn, err)
	}
	if reads.n < 3 {
		t.Fatalf("hello arrived in %d reads, want 3", reads.n)
	}
}

type countReads struct {
	net.Conn
	n int
}

func (c *countReads) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.n++
	}
	return n, err
}

// A hello split across several TLS records reassembles.
func TestPeekHelloAcrossRecords(t *testing.T) {
	rec := clientHello(t, &tls.Config{ServerName: "h.test", CurvePreferences: []tls.CurveID{tls.X25519MLKEM768}})
	msg := rec[5:]
	var split []byte
	for len(msg) > 0 {
		n := min(len(msg), 500)
		split = append(split, 0x16, 0x03, 0x01, byte(n>>8), byte(n))
		split = append(split, msg[:n]...)
		msg = msg[n:]
	}
	sni, _, replay, err := PeekClientHello(feed(t, time.Millisecond, split[:7], split[7:900], split[900:]), MaxHello, 5*time.Second)
	if err != nil || sni != "h.test" || !bytes.Equal(replay, split) {
		t.Fatalf("sni %q err %v", sni, err)
	}
}

// Bytes after the hello in the same read are part of replay.
func TestPeekKeepsTrailingBytes(t *testing.T) {
	rec := clientHello(t, &tls.Config{ServerName: "h.test"})
	all := append(bytes.Clone(rec), "\x14\x03\x03\x00\x01\x01"...)
	_, _, replay, err := PeekClientHello(feed(t, 0, all), MaxHello, 5*time.Second)
	if err != nil || !bytes.Equal(replay, all) {
		t.Fatalf("replay %d bytes, want %d (%v)", len(replay), len(all), err)
	}
}

// hello builds a ClientHello record with the given extensions, each as
// {type, body}.
func hello(exts ...[2][]byte) []byte {
	var b cryptobyte.Builder
	b.AddUint8(typeClientHello)
	b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddUint16(0x0303)
		b.AddBytes(make([]byte, 32))
		b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) {})
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddUint16(0x1301) })
		b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddUint8(0) })
		if exts != nil {
			b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
				for _, e := range exts {
					b.AddBytes(e[0])
					b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes(e[1]) })
				}
			})
		}
	})
	msg := b.BytesOrPanic()
	return append([]byte{0x16, 0x03, 0x01, byte(len(msg) >> 8), byte(len(msg))}, msg...)
}

func ext(typ uint16, body []byte) [2][]byte { return [2][]byte{{byte(typ >> 8), byte(typ)}, body} }

func sniExt(names ...string) [2][]byte {
	var b cryptobyte.Builder
	b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		for _, n := range names {
			b.AddUint8(0)
			b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes([]byte(n)) })
		}
	})
	return ext(extServerName, b.BytesOrPanic())
}

func alpnExt(protos ...string) [2][]byte {
	var b cryptobyte.Builder
	b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		for _, p := range protos {
			b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes([]byte(p)) })
		}
	})
	return ext(extALPN, b.BytesOrPanic())
}

func TestPeekSynthetic(t *testing.T) {
	ok := []struct {
		name string
		rec  []byte
		sni  string
		alpn []string
	}{
		{"no extensions", hello(), "", nil},
		{"empty extensions", hello([][2][]byte{}...), "", nil},
		{"sni and alpn", hello(sniExt("h.test"), alpnExt("h2", "http/1.1")), "h.test", []string{"h2", "http/1.1"}},
		{"other name types skipped", hello(ext(extServerName, []byte{0, 8, 9, 0, 1, 'x', 0, 0, 1, 'y'})), "y", nil},
		{"unknown extension", hello(ext(0xfe0d, []byte("ech")), sniExt("h.test")), "h.test", nil},
	}
	for _, tc := range ok {
		sni, alpn, _, err := PeekClientHello(feed(t, 0, tc.rec), MaxHello, time.Second)
		if err != nil || sni != tc.sni || !slices.Equal(alpn, tc.alpn) {
			t.Errorf("%s: sni %q alpn %v err %v", tc.name, sni, alpn, err)
		}
	}

	valid := hello(sniExt("h.test"))
	bad := []struct {
		name string
		rec  []byte
	}{
		{"http", []byte("GET / HTTP/1.1\r\nHost: h.test\r\n\r\n")},
		{"application data record", append([]byte{0x17}, valid[1:]...)},
		{"SSLv2 record version", append([]byte{0x16, 0x02}, valid[2:]...)},
		{"empty record", []byte{0x16, 0x03, 0x01, 0x00, 0x00}},
		{"record over 16 KiB", []byte{0x16, 0x03, 0x01, 0x40, 0x01}},
		{"not a ClientHello", append(bytes.Clone(valid[:5]), append([]byte{2}, valid[6:]...)...)},
		{"duplicate extension", hello(sniExt("h.test"), ext(0x0017, nil), ext(0x0017, nil))},
		{"duplicate server_name", hello(sniExt("h.test"), sniExt("h.test"))},
		{"two host_names", hello(sniExt("a.test", "b.test"))},
		{"trailing dot", hello(sniExt("h.test."))},
		{"space in name", hello(sniExt("h .test"))},
		{"non-ASCII name", hello(sniExt("hK.test"))},
		{"empty name", hello(sniExt(""))},
		{"empty server_name list", hello(ext(extServerName, []byte{0, 0}))},
		{"server_name with trailing bytes", hello(ext(extServerName, append(sniExt("h.test")[1], 0)))},
		{"empty ALPN list", hello(ext(extALPN, []byte{0, 0}))},
		{"empty ALPN protocol", hello(alpnExt(""))},
		{"extensions overrun", func() []byte {
			r := hello(sniExt("h.test"))
			r[50], r[51] = 0xff, 0xff // the extensions' length, past the end
			return r
		}()},
		{"odd cipher suites", func() []byte {
			r := hello()
			r[45] = 3 // the cipher suites' length
			return r
		}()},
	}
	for _, tc := range bad {
		_, _, _, err := PeekClientHello(feed(t, 0, tc.rec), MaxHello, 200*time.Millisecond)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// A hello that does not fit in max is refused, and no more than max bytes
// are ever read.
func TestPeekMax(t *testing.T) {
	rec := clientHello(t, &tls.Config{ServerName: "h.test", CurvePreferences: []tls.CurveID{tls.X25519MLKEM768}})
	for _, max := range []int{100, len(rec) - 1} {
		_, _, replay, err := PeekClientHello(feed(t, 0, rec, rec), max, time.Second)
		if !errors.Is(err, ErrNotClientHello) || len(replay) > max {
			t.Errorf("max %d: err %v after %d bytes", max, err, len(replay))
		}
	}
	// A record stream that never finishes a hello stops at max.
	junk := bytes.Repeat([]byte{0x16, 0x03, 0x01, 0x00, 0x01, 0x01}, 4000)
	_, _, replay, err := PeekClientHello(feed(t, 0, junk), MaxHello, time.Second)
	if err == nil || len(replay) > MaxHello {
		t.Fatalf("err %v after %d bytes", err, len(replay))
	}
}

// A client that stalls mid-hello is cut at the deadline; one that hangs up
// gets an unexpected EOF.
func TestPeekDeadlineAndEOF(t *testing.T) {
	rec := clientHello(t, &tls.Config{ServerName: "h.test"})
	start := time.Now()
	_, _, replay, err := PeekClientHello(feed(t, time.Hour, rec[:40]), MaxHello, 100*time.Millisecond)
	if !errors.Is(err, os.ErrDeadlineExceeded) || len(replay) != 40 || time.Since(start) > 2*time.Second {
		t.Fatalf("stall: err %v, %d bytes, %v", err, len(replay), time.Since(start))
	}

	a, b := net.Pipe()
	go func() { b.Write(rec[:40]); b.Close() }()
	_, _, _, err = PeekClientHello(a, MaxHello, time.Second)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("hang-up: err %v", err)
	}
}

// The captured OpenSSL 3.6 (X25519MLKEM768 share) and LibreSSL hellos in
// the fuzz corpus are real clients asking for h.test.
func TestPeekCapturedHellos(t *testing.T) {
	for _, tc := range []struct {
		file   string
		alpn   []string
		minLen int
	}{
		{"openssl-3.6.4-mlkem", []string{"h2", "http/1.1"}, 1216},
		{"curl-8.7.1-libressl-3.3.6", []string{"h2", "http/1.1"}, 0},
	} {
		b, err := os.ReadFile("testdata/fuzz/FuzzPeekClientHello/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		lit, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(lines[1], "[]byte("), ")"))
		if err != nil {
			t.Fatal(err)
		}
		rec := []byte(lit)
		sni, alpn, _, err := PeekClientHello(feed(t, 0, rec), MaxHello, time.Second)
		if err != nil || sni != "h.test" || !slices.Equal(alpn, tc.alpn) || len(rec) < tc.minLen {
			t.Errorf("%s: sni %q alpn %v err %v (%d bytes)", tc.file, sni, alpn, err, len(rec))
		}
	}
}
