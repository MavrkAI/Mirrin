package httpsig

// RFC 9421 section 2: component values and the signature base, and parsing
// the Signature-Input and Signature fields. Only request components are
// supported, and no component parameters.

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"strings"
)

// maxField bounds each signature-related field before it is parsed.
const maxField = 8 << 10

// signature is one Signature-Input member and, if present, its Signature.
type signature struct {
	label      string
	components []item // each v is a component name (string)
	params     []param
	sig        []byte
}

// param returns the named signature parameter.
func (s signature) param(key string) (any, bool) {
	for _, p := range s.params {
		if p.key == key {
			return p.v, true
		}
	}
	return nil, false
}

// fieldValue joins every line of a field with ", ", after trimming each.
func fieldValue(h http.Header, name string) string {
	var vs []string
	for _, v := range h.Values(name) { // not a copy: leave it alone
		vs = append(vs, strings.Trim(v, " \t"))
	}
	return strings.Join(vs, ", ")
}

// parseSignatures reads every signature on r.
func parseSignatures(h http.Header) ([]signature, error) {
	in, sg := fieldValue(h, "Signature-Input"), fieldValue(h, "Signature")
	if in == "" {
		return nil, nil
	}
	if len(in) > maxField || len(sg) > maxField {
		return nil, fmt.Errorf("%w: signature fields too long", errSFV)
	}
	inputs, err := parseDictionary(in)
	if err != nil {
		return nil, err
	}
	sigs, err := parseDictionary(sg)
	if err != nil {
		return nil, err
	}
	var out []signature
	for _, m := range inputs {
		if !m.isList {
			return nil, sfvErr("signature input %q is not an inner list", m.key)
		}
		for _, c := range m.list {
			if _, ok := c.v.(string); !ok {
				return nil, sfvErr("component in %q is not a string", m.key)
			}
		}
		s := signature{label: m.key, components: m.list, params: m.item.params}
		for _, g := range sigs {
			if g.key == m.key && !g.isList {
				s.sig, _ = g.item.v.([]byte)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// signatureBase builds the RFC 9421 signature base for s over r, a request
// to o.
func signatureBase(r *http.Request, o origin, s signature) (string, error) {
	var b strings.Builder
	seen := map[string]bool{}
	for _, c := range s.components {
		name := c.v.(string)
		if len(c.params) > 0 {
			return "", fmt.Errorf("httpsig: component parameters are not supported (%q)", name)
		}
		if seen[name] {
			return "", fmt.Errorf("httpsig: component %q covered twice", name)
		}
		seen[name] = true
		v, err := componentValue(r, o, name)
		if err != nil {
			return "", err
		}
		if strings.ContainsAny(v, "\r\n") {
			return "", fmt.Errorf("httpsig: component %q has a line break", name)
		}
		b.WriteString(serializeBare(name))
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteByte('\n')
	}
	b.WriteString(`"@signature-params": `)
	b.WriteString(serializeInnerList(s.components, s.params))
	return b.String(), nil
}

// componentValue is RFC 9421 section 2.1 (fields) and 2.2 (derived). The
// scheme and authority come from o, never from r.
func componentValue(r *http.Request, o origin, name string) (string, error) {
	switch name {
	case "@method":
		return r.Method, nil
	case "@target-uri":
		return o.scheme + "://" + o.authority + r.URL.RequestURI(), nil
	case "@authority":
		return o.authority, nil
	case "@scheme":
		return o.scheme, nil
	case "@path":
		if p := r.URL.EscapedPath(); p != "" {
			return p, nil
		}
		return "/", nil
	case "@query":
		return "?" + r.URL.RawQuery, nil
	}
	if strings.HasPrefix(name, "@") || name == "" || name != strings.ToLower(name) {
		return "", fmt.Errorf("httpsig: unsupported component %q", name)
	}
	if name == "host" { // net/http keeps Host out of r.Header
		return host(r), nil
	}
	if len(r.Header.Values(name)) == 0 {
		return "", fmt.Errorf("httpsig: covered field %q is missing", name)
	}
	return fieldValue(r.Header, name), nil
}

// verifyBase checks an Ed25519 signature over base.
func verifyBase(pub ed25519.PublicKey, base string, sig []byte) bool {
	return len(pub) == ed25519.PublicKeySize && len(sig) == ed25519.SignatureSize &&
		ed25519.Verify(pub, []byte(base), sig)
}
