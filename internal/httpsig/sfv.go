package httpsig

// The part of Structured Field Values (RFC 8941) that signatures need:
// dictionaries whose members are items or inner lists, with parameters.
// Decimals, dates and display strings are refused. Parsing follows the
// algorithms of RFC 8941 section 4.2, except that a repeated dictionary or
// parameter key is an error rather than last-wins: a signature header that
// means two things is refused.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// token is an sf-token; plain Go strings are sf-strings.
type token string

// item is a bare item (int64, string, token, []byte or bool) with parameters.
type item struct {
	v      any
	params []param
}

type param struct {
	key string
	v   any
}

// member is one dictionary entry: an item, or an inner list when list is set.
type member struct {
	key    string
	item   item
	list   []item
	isList bool
}

var errSFV = errors.New("httpsig: malformed structured field")

func sfvErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errSFV, fmt.Sprintf(format, a...))
}

type parser struct {
	s string
	i int
}

func (p *parser) done() bool { return p.i >= len(p.s) }

func (p *parser) peek() byte {
	if p.done() {
		return 0
	}
	return p.s[p.i]
}

func (p *parser) skipSP() {
	for p.peek() == ' ' {
		p.i++
	}
}

func (p *parser) skipOWS() {
	for c := p.peek(); c == ' ' || c == '\t'; c = p.peek() {
		p.i++
	}
}

// parseDictionary parses a whole field value as a dictionary.
func parseDictionary(s string) ([]member, error) {
	p := &parser{s: s}
	p.skipSP()
	var out []member
	seen := map[string]bool{}
	for !p.done() {
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		if seen[key] {
			return nil, sfvErr("repeated key %q", key)
		}
		seen[key] = true
		m := member{key: key}
		if p.peek() == '=' {
			p.i++
			if p.peek() == '(' {
				m.isList = true
				if m.list, m.item.params, err = p.innerList(); err != nil {
					return nil, err
				}
			} else if m.item, err = p.item(); err != nil {
				return nil, err
			}
		} else {
			m.item.v = true
			if m.item.params, err = p.params(); err != nil {
				return nil, err
			}
		}
		out = append(out, m)
		p.skipOWS()
		if p.done() {
			break
		}
		if p.s[p.i] != ',' {
			return nil, sfvErr("expected a comma at %d", p.i)
		}
		p.i++
		p.skipOWS()
		if p.done() {
			return nil, sfvErr("trailing comma")
		}
	}
	return out, nil
}

func (p *parser) innerList() ([]item, []param, error) {
	p.i++ // (
	var items []item
	for !p.done() {
		p.skipSP()
		if p.peek() == ')' {
			p.i++
			params, err := p.params()
			return items, params, err
		}
		it, err := p.item()
		if err != nil {
			return nil, nil, err
		}
		items = append(items, it)
		if c := p.peek(); c != ' ' && c != ')' {
			return nil, nil, sfvErr("bad inner list at %d", p.i)
		}
	}
	return nil, nil, sfvErr("unterminated inner list")
}

func (p *parser) item() (item, error) {
	v, err := p.bare()
	if err != nil {
		return item{}, err
	}
	params, err := p.params()
	return item{v: v, params: params}, err
}

func (p *parser) params() ([]param, error) {
	var out []param
	for p.peek() == ';' {
		p.i++
		p.skipSP()
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		for _, q := range out {
			if q.key == key {
				return nil, sfvErr("repeated parameter %q", key)
			}
		}
		var v any = true
		if p.peek() == '=' {
			p.i++
			if v, err = p.bare(); err != nil {
				return nil, err
			}
		}
		out = append(out, param{key: key, v: v})
	}
	return out, nil
}

func (p *parser) key() (string, error) {
	start := p.i
	if c := p.peek(); !isLCAlpha(c) && c != '*' {
		return "", sfvErr("bad key at %d", p.i)
	}
	for c := p.peek(); isLCAlpha(c) || isDigit(c) || c == '_' || c == '-' || c == '.' || c == '*'; c = p.peek() {
		p.i++
	}
	return p.s[start:p.i], nil
}

func (p *parser) bare() (any, error) {
	switch c := p.peek(); {
	case c == '-' || isDigit(c):
		return p.integer()
	case c == '"':
		return p.str()
	case c == ':':
		return p.bytes()
	case c == '?':
		return p.boolean()
	case isAlpha(c) || c == '*':
		return p.token()
	}
	return nil, sfvErr("bad item at %d", p.i)
}

func (p *parser) integer() (int64, error) {
	start := p.i
	if p.peek() == '-' {
		p.i++
	}
	digits := p.i
	for isDigit(p.peek()) {
		p.i++
	}
	switch {
	case p.i == digits:
		return 0, sfvErr("bad integer at %d", start)
	case p.i-digits > 15:
		return 0, sfvErr("integer too long at %d", start)
	case p.peek() == '.':
		return 0, sfvErr("decimals are not supported")
	}
	return strconv.ParseInt(p.s[start:p.i], 10, 64)
}

func (p *parser) str() (string, error) {
	p.i++ // "
	var b strings.Builder
	for !p.done() {
		c := p.s[p.i]
		p.i++
		switch {
		case c == '\\':
			if p.done() || p.s[p.i] != '"' && p.s[p.i] != '\\' {
				return "", sfvErr("bad escape in string")
			}
			b.WriteByte(p.s[p.i])
			p.i++
		case c == '"':
			return b.String(), nil
		case c < 0x20 || c > 0x7e:
			return "", sfvErr("bad character in string")
		default:
			b.WriteByte(c)
		}
	}
	return "", sfvErr("unterminated string")
}

func (p *parser) token() (token, error) {
	start := p.i
	p.i++
	for c := p.peek(); isTChar(c) || c == ':' || c == '/'; c = p.peek() {
		p.i++
	}
	return token(p.s[start:p.i]), nil
}

// bytes reads a byte sequence, insisting on canonical padded base64.
func (p *parser) bytes() ([]byte, error) {
	p.i++ // :
	end := strings.IndexByte(p.s[p.i:], ':')
	if end < 0 {
		return nil, sfvErr("unterminated byte sequence")
	}
	enc := p.s[p.i : p.i+end]
	p.i += end + 1
	for i := 0; i < len(enc); i++ {
		if c := enc[i]; !isAlpha(c) && !isDigit(c) && c != '+' && c != '/' && c != '=' {
			return nil, sfvErr("bad byte sequence")
		}
	}
	b, err := base64.StdEncoding.Strict().DecodeString(enc)
	if err != nil {
		return nil, sfvErr("bad byte sequence")
	}
	return b, nil
}

func (p *parser) boolean() (bool, error) {
	p.i++ // ?
	switch p.peek() {
	case '1':
		p.i++
		return true, nil
	case '0':
		p.i++
		return false, nil
	}
	return false, sfvErr("bad boolean")
}

func isDigit(c byte) bool   { return '0' <= c && c <= '9' }
func isLCAlpha(c byte) bool { return 'a' <= c && c <= 'z' }
func isAlpha(c byte) bool   { return isLCAlpha(c) || 'A' <= c && c <= 'Z' }
func isTChar(c byte) bool {
	return isAlpha(c) || isDigit(c) || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// serializeInnerList writes an inner list with its parameters.
func serializeInnerList(items []item, params []param) string {
	var b strings.Builder
	b.WriteByte('(')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(serializeItem(it))
	}
	b.WriteByte(')')
	b.WriteString(serializeParams(params))
	return b.String()
}

func serializeItem(it item) string { return serializeBare(it.v) + serializeParams(it.params) }

func serializeParams(ps []param) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteByte(';')
		b.WriteString(p.key)
		if v, ok := p.v.(bool); !ok || !v {
			b.WriteByte('=')
			b.WriteString(serializeBare(p.v))
		}
	}
	return b.String()
}

// serializeBare writes a bare item. Callers only pass values that parse, so
// every string here is printable ASCII.
func serializeBare(v any) string {
	switch v := v.(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case string:
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	case token:
		return string(v)
	case []byte:
		return ":" + base64.StdEncoding.EncodeToString(v) + ":"
	case bool:
		if v {
			return "?1"
		}
		return "?0"
	}
	panic(fmt.Sprintf("httpsig: cannot serialize %T", v))
}

// serializeDictionary writes members as a dictionary field value.
func serializeDictionary(ms []member) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		switch {
		case m.isList:
			parts[i] = m.key + "=" + serializeInnerList(m.list, m.item.params)
		case m.item.v == true:
			parts[i] = m.key + serializeParams(m.item.params)
		default:
			parts[i] = m.key + "=" + serializeItem(m.item)
		}
	}
	return strings.Join(parts, ", ")
}
