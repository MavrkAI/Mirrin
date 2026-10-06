package wire

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
)

// Nightly CI runs each of these for 60 s (.github/workflows/fuzz.yml).

// A header that parses is at most MaxProxyHeader bytes, leaves everything
// after it in the reader, and survives a write and read unchanged.
func FuzzReadProxyV2(f *testing.F) {
	f.Add(goldenV4)
	f.Add(goldenV6)
	f.Add(withCRC(goldenV4))
	f.Add(append(bytes.Clone(goldenV6), "\x16\x03\x01\x02\x00"...))
	f.Add(unhex("0d0a0d0a000d0a515549540a 20 00 0000"))
	f.Add(unhex("0d0a0d0a000d0a515549540a 20 31 00d8") /* LOCAL, UNIX, truncated */)
	f.Add([]byte("PROXY TCP4 203.0.113.9 198.51.100.1 51234 443\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bufio.NewReader(bytes.NewReader(data))
		h, err := ReadProxyV2(r)
		if err != nil {
			return
		}
		n := 16 + int(binary.BigEndian.Uint16(data[14:16]))
		if n > MaxProxyHeader {
			t.Fatalf("accepted a %d-byte header", n)
		}
		if rest, _ := io.ReadAll(r); !bytes.Equal(rest, data[n:]) {
			t.Fatal("ReadProxyV2 consumed bytes past the header")
		}
		var buf bytes.Buffer
		if err := WriteProxyV2(&buf, h); err != nil {
			t.Fatalf("cannot re-encode %+v: %v", h, err)
		}
		again, err := ReadProxyV2(bufio.NewReader(&buf))
		if err != nil || again != h {
			t.Fatalf("round trip: %+v → %+v (%v)", h, again, err)
		}
	})
}

// A hello that parses re-encodes to one that parses the same, and only the
// seed's own key and signature ever verify for the seed's binding.
func FuzzHello(f *testing.F) {
	h := signedHello(f)
	good, err := h.Marshal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	h.Ent = "v4.public.eyJ4Ijp0cnVlfQ.eyJraWQiOiJlbnQtdGVzdC1hIn0"
	withEnt, _ := h.Marshal()
	f.Add(withEnt)
	f.Add([]byte(`{"t":"hello","v":1}`))
	f.Add([]byte(`{"t":"hello","v":1,"t":"hello"}`))
	f.Add([]byte(`null`))
	seed := signedHello(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHello(data)
		if err != nil {
			return
		}
		b, err := h.Marshal()
		if err != nil {
			t.Fatalf("parsed hello does not re-encode: %v", err)
		}
		again, err := ParseHello(b)
		if err != nil || !reflect.DeepEqual(again, h) {
			t.Fatalf("round trip: %+v → %+v (%v)", h, again, err)
		}
		if _, err := VerifyHello(h, "r1", testNonce, testExporter); err == nil {
			if !bytes.Equal(h.Key, seed.Key) || !bytes.Equal(h.Sig, seed.Sig) {
				t.Fatal("a signature the seed key never made verified")
			}
		}
		VerifyHello(h, "r2", testExporter, testNonce)
	})
}

// Everything a relay sends the daemon: a message that parses re-encodes to
// one that parses the same, and nothing malformed is taken for a refusal.
func FuzzRelayMessages(f *testing.F) {
	ch, _ := NewChallenge("r1")
	b, _ := ch.Marshal()
	f.Add(b)
	b, _ = Welcome{Hostnames: []string{"h.test"}, Gen: 3, Keepalive: 25, MaxStreams: 64}.Marshal()
	f.Add(b)
	b, _ = Error{Code: CodeSupersededRetry, Message: "Another machine holds this name.", RetryAfter: 3600}.Marshal()
	f.Add(b)
	for _, c := range []Control{{T: ControlSuperseded, Gen: 4}, {T: ControlDrain, RetryAfter: 30}, {T: ControlLimits, MaxStreams: 32, BPS: 20e6}} {
		b, _ = c.Marshal()
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if c, err := ParseChallenge(data); err == nil {
			b, err := c.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if again, err := ParseChallenge(b); err != nil || !reflect.DeepEqual(again, c) {
				t.Fatalf("challenge round trip: %v", err)
			}
		}
		w, err := ParseReply(data)
		var e Error
		switch {
		case err == nil:
			b, err := w.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if again, err := ParseReply(b); err != nil || !reflect.DeepEqual(again, w) {
				t.Fatalf("welcome round trip: %v", err)
			}
		case errors.As(err, &e):
			if errors.Is(err, ErrMalformed) || e.check() != nil {
				t.Fatalf("malformed refusal surfaced: %+v", e)
			}
			b, err := e.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var again Error
			if _, err := ParseReply(b); !errors.As(err, &again) || again != e {
				t.Fatalf("error round trip: %v", err)
			}
		}
		if c, err := ParseControl(data); err == nil {
			var buf bytes.Buffer
			if err := WriteControl(&buf, c); err != nil {
				t.Fatal(err)
			}
			if again, err := NewControlReader(&buf).Next(); err != nil || again != c {
				t.Fatalf("control round trip: %v", err)
			}
		}
	})
}
