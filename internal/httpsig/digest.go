package httpsig

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxBody bounds the content Sign and Verify will read to digest.
const MaxBody = 1 << 20

// ErrDigest means Content-Digest is missing, malformed or wrong.
var ErrDigest = errors.New("httpsig: content digest mismatch")

// contentDigest is the RFC 9530 Content-Digest value for body, sha-256 only.
func contentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return serializeDictionary([]member{{key: "sha-256", item: item{v: sum[:]}}})
}

// checkDigest insists that Content-Digest is exactly one sha-256 member and
// that it matches body.
func checkDigest(h http.Header, body []byte) error {
	ms, err := parseDictionary(fieldValue(h, "Content-Digest"))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDigest, err)
	}
	if len(ms) != 1 || ms[0].key != "sha-256" || ms[0].isList || len(ms[0].item.params) > 0 {
		return fmt.Errorf("%w: want exactly one sha-256 digest", ErrDigest)
	}
	want, ok := ms[0].item.v.([]byte)
	sum := sha256.Sum256(body)
	if !ok || subtle.ConstantTimeCompare(want, sum[:]) != 1 {
		return ErrDigest
	}
	return nil
}

// readBody reads r's content, up to MaxBody, and puts it back so the handler
// or transport can read it again.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBody {
		return nil, fmt.Errorf("httpsig: body over %d bytes", MaxBody)
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	return b, nil
}
