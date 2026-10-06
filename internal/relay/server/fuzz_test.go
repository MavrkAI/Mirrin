package server

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"slices"
	"testing"
	"time"
)

// Nightly CI runs this for 5 minutes (.github/workflows/fuzz.yml). Seeds:
// the RFC 8448 hellos, crypto/tls hellos (with and without an
// X25519MLKEM768 share, ALPN and SNI), synthetic ones, and captured
// OpenSSL and LibreSSL hellos in testdata/fuzz.
//
// Properties:
//   - replay is a prefix of the input of at most MaxHello bytes;
//   - the answer does not depend on how the bytes arrive;
//   - a returned name is one the router can compare;
//   - whenever crypto/tls, which parses the hello again on the daemon,
//     accepts a hello within the relay's limits, the relay accepts it
//     too and finds the same server_name and ALPN list.
func FuzzPeekClientHello(f *testing.F) {
	for _, rec := range rfc8448Hellos(f) {
		f.Add(rec, uint16(0))
	}
	for _, cfg := range []*tls.Config{
		{ServerName: "h.test", NextProtos: []string{"h2", "http/1.1"}},
		{ServerName: "h.test", NextProtos: []string{"acme-tls/1"}},
		{ServerName: "ember-otter-42.mirrin.link", CurvePreferences: []tls.CurveID{tls.X25519MLKEM768}},
		{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12},
	} {
		rec := clientHello(f, cfg)
		f.Add(rec, uint16(len(rec)/3))
	}
	f.Add(hello(sniExt("h.test"), alpnExt("h2")), uint16(7))
	f.Add(hello(), uint16(1))
	f.Add(hello(sniExt("h.test."), sniExt("x")), uint16(0))
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"), uint16(0))
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x04, 0x01, 0x00, 0x00, 0x00}, uint16(3))
	f.Fuzz(func(t *testing.T, data []byte, cut uint16) {
		split := min(int(cut), len(data))
		sni, alpn, replay, err := PeekClientHello(&memConn{chunks: [][]byte{data[:split], data[split:]}}, MaxHello, time.Second)
		if len(replay) > MaxHello || !bytes.HasPrefix(data, replay) {
			t.Fatalf("replay is not a prefix of at most %d bytes (%d)", MaxHello, len(replay))
		}
		if err == nil && sni != "" && !validServerName([]byte(sni)) {
			t.Fatalf("returned name %q", sni)
		}
		sni1, alpn1, _, err1 := PeekClientHello(&memConn{chunks: [][]byte{data}}, MaxHello, time.Second)
		if (err == nil) != (err1 == nil) || sni != sni1 || !slices.Equal(alpn, alpn1) {
			t.Fatalf("depends on chunking: %q %v %v / %q %v %v", sni, alpn, err, sni1, alpn1, err1)
		}
		if !plainRecords(data) {
			return
		}
		hi, ok := stdlibHello(data)
		if !ok || hi.ServerName != "" && !validServerName([]byte(hi.ServerName)) {
			return
		}
		if err != nil || hi.ServerName != sni || !slices.Equal(hi.SupportedProtos, alpn) {
			t.Fatalf("crypto/tls read %q %v; relay read %q %v (%v)", hi.ServerName, hi.SupportedProtos, sni, alpn, err)
		}
	})
}

// plainRecords reports whether data opens with handshake records the
// relay's limits allow (major version 3, 1 byte to 16 KiB each) that
// complete a ClientHello within MaxHello bytes.
func plainRecords(data []byte) bool {
	var hs []byte
	for off := 0; off+5 <= len(data) && off+5 <= MaxHello; {
		n := int(data[off+3])<<8 | int(data[off+4])
		if data[off] != recordHandshake || data[off+1] != 3 || n == 0 || n > maxRecordPayload ||
			off+5+n > len(data) || off+5+n > MaxHello {
			return false
		}
		hs = append(hs, data[off+5:off+5+n]...)
		off += 5 + n
		if len(hs) >= 4 && len(hs) >= 4+(int(hs[1])<<16|int(hs[2])<<8|int(hs[3])) {
			return true
		}
	}
	return false
}

// stdlibHello is what crypto/tls's server makes of data's ClientHello, if
// it gets as far as asking for a config.
func stdlibHello(data []byte) (tls.ClientHelloInfo, bool) {
	var got tls.ClientHelloInfo
	ok := false
	stop := errors.New("stop")
	cfg := &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		got = tls.ClientHelloInfo{ServerName: h.ServerName, SupportedProtos: slices.Clone(h.SupportedProtos)}
		ok = true
		return nil, stop
	}}
	tls.Server(&memConn{chunks: [][]byte{data}}, cfg).Handshake()
	return got, ok
}

// memConn is a net.Conn that yields its chunks one Read at a time, then
// EOF, and swallows writes.
type memConn struct{ chunks [][]byte }

func (c *memConn) Read(p []byte) (int, error) {
	for len(c.chunks) > 0 && len(c.chunks[0]) == 0 {
		c.chunks = c.chunks[1:]
	}
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	c.chunks[0] = c.chunks[0][n:]
	return n, nil
}

func (c *memConn) Write(p []byte) (int, error)        { return len(p), nil }
func (c *memConn) Close() error                       { return nil }
func (c *memConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *memConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *memConn) SetDeadline(t time.Time) error      { return nil }
func (c *memConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *memConn) SetWriteDeadline(t time.Time) error { return nil }
