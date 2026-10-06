package certwatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DefaultDoH are the resolvers CAA is looked up through (RFC 8484 wire
// format). They are tried in turn and the first to answer is used: several
// operators, so one failing resolver doesn't blind the watch. One that
// lies is believed; the CT watch doesn't depend on DNS.
var DefaultDoH = []string{"https://1.1.1.1/dns-query", "https://8.8.8.8/dns-query", "https://9.9.9.9/dns-query"}

const typeCAA dnsmessage.Type = 257

// CAA is one CAA record (RFC 8659).
type CAA struct {
	Name  string // the owner name the record was found at
	Flags uint8
	Tag   string // issue, issuewild, iodef, …
	Value string
}

// Critical is the issuer-critical flag.
func (c CAA) Critical() bool { return c.Flags&0x80 != 0 }

// Issuer is the CA domain of an issue or issuewild record; "" for ";",
// which forbids issuance.
func (c CAA) Issuer() string {
	d, _, _ := strings.Cut(c.Value, ";")
	return strings.ToLower(strings.TrimSpace(d))
}

// Params are an issue record's key=value parameters.
func (c CAA) Params() map[string]string {
	_, rest, ok := strings.Cut(c.Value, ";")
	out := map[string]string{}
	if !ok {
		return out
	}
	for _, p := range strings.Split(rest, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok {
			out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out
}

// paramCount counts an issue record's parameters named key.
func (c CAA) paramCount(key string) int {
	_, rest, ok := strings.Cut(c.Value, ";")
	if !ok {
		return 0
	}
	n := 0
	for _, p := range strings.Split(rest, ";") {
		if k, _, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(strings.TrimSpace(k), key) {
			n++
		}
	}
	return n
}

// AccountURI is the RFC 8657 accounturi parameter.
func (c CAA) AccountURI() string { return c.Params()["accounturi"] }

// ValidationMethods is the RFC 8657 validationmethods parameter.
func (c CAA) ValidationMethods() []string {
	v := c.Params()["validationmethods"]
	if v == "" {
		return nil
	}
	var out []string
	for _, m := range strings.Split(v, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func (c CAA) String() string { return fmt.Sprintf("%d %s %q", c.Flags, c.Tag, c.Value) }

// Resolver looks CAA up over DNS-over-HTTPS.
type Resolver struct {
	DoH    []string // nil is DefaultDoH
	Client *http.Client
}

// LookupCAA finds the CAA set that governs name: the records at name, or at
// the closest parent that has any (RFC 8659 §3). No records anywhere is an
// empty answer, not an error.
func LookupCAA(ctx context.Context, name string, doh []string) ([]CAA, error) {
	return (&Resolver{DoH: doh}).Lookup(ctx, name)
}

// Lookup is LookupCAA with the resolver's client.
func (r *Resolver) Lookup(ctx context.Context, name string) ([]CAA, error) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for n := name; strings.Contains(n, "."); {
		recs, err := r.query(ctx, n)
		if err != nil {
			return nil, err
		}
		if len(recs) > 0 {
			return recs, nil
		}
		_, n, _ = strings.Cut(n, ".")
	}
	return nil, nil
}

func (r *Resolver) query(ctx context.Context, name string) ([]CAA, error) {
	servers := r.DoH
	if servers == nil {
		servers = DefaultDoH
	}
	var errs []error
	for _, s := range servers {
		recs, err := r.ask(ctx, s, name)
		if err == nil {
			return recs, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, errors.New("no DNS-over-HTTPS resolvers configured")
	}
	return nil, fmt.Errorf("couldn't look up CAA for %s: %w", name, errors.Join(errs...))
}

func (r *Resolver) ask(ctx context.Context, server, name string) ([]CAA, error) {
	q, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{RecursionDesired: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(dnsmessage.Question{Name: q, Type: typeCAA, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	msg, err := b.Finish()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	c := r.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 65536))
	if err != nil {
		return nil, err
	}
	return ParseCAAResponse(body, name)
}

// ParseCAAResponse reads the CAA records from a DNS response. NXDOMAIN and
// an empty NOERROR both mean none; any other rcode is an error.
func ParseCAAResponse(msg []byte, name string) ([]CAA, error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return nil, err
	}
	switch h.RCode {
	case dnsmessage.RCodeSuccess, dnsmessage.RCodeNameError:
	default:
		return nil, fmt.Errorf("DNS error %v for %s", h.RCode, name)
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, err
	}
	var out []CAA
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		if rh.Type != typeCAA {
			if err := p.SkipAnswer(); err != nil {
				return nil, err
			}
			continue
		}
		res, err := p.UnknownResource()
		if err != nil {
			return nil, err
		}
		c, err := parseCAAData(res.Data)
		if err != nil {
			return nil, err
		}
		c.Name = name
		out = append(out, c)
	}
	return out, nil
}

func parseCAAData(d []byte) (CAA, error) {
	if len(d) < 2 || int(d[1]) == 0 || len(d) < 2+int(d[1]) {
		return CAA{}, errors.New("malformed CAA record")
	}
	return CAA{Flags: d[0], Tag: strings.ToLower(string(d[2 : 2+int(d[1])])), Value: string(d[2+int(d[1]):])}, nil
}

// CAAData encodes a record's RDATA (tests and fakes build responses with it).
func CAAData(c CAA) []byte {
	return append(append([]byte{c.Flags, byte(len(c.Tag))}, c.Tag...), c.Value...)
}

// CAAVerdict is what a CAA set means for this machine's account.
type CAAVerdict struct {
	OK       bool   // only our account can issue (or nobody)
	Missing  bool   // no CAA at all: any CA may issue
	Critical bool   // someone else's account may issue: the alarm
	Problem  string // a sentence for the owner when not OK
}

// CheckCAA judges a CAA set against accountURI: every issue and issuewild
// record that names a CA must carry our accounturi, or it is Critical.
// Records that forbid issuance (";") are fine. A missing set, or one that
// drops tls-alpn-01, is a warning: nobody else can issue because of it, but
// the owner should fix it.
func CheckCAA(recs []CAA, accountURI string) CAAVerdict {
	if len(recs) == 0 {
		return CAAVerdict{Missing: true, Problem: "No CAA records protect this name, so any certificate authority may issue for it."}
	}
	for _, r := range recs {
		if r.Tag != "issue" && r.Tag != "issuewild" {
			continue
		}
		if r.Issuer() == "" {
			continue
		}
		if r.paramCount("accounturi") > 1 {
			// Parsers disagree on which one wins; a CA may read the other.
			return CAAVerdict{Critical: true, Problem: fmt.Sprintf("A CAA record at %s names more than one ACME account: %s", r.Name, r)}
		}
		if got := r.AccountURI(); got != accountURI {
			if got == "" {
				return CAAVerdict{Critical: true, Problem: fmt.Sprintf("A CAA record at %s lets %s issue for any account: %s", r.Name, r.Issuer(), r)}
			}
			return CAAVerdict{Critical: true, Problem: fmt.Sprintf("A CAA record at %s names another ACME account: %s", r.Name, r)}
		}
		if ms := r.ValidationMethods(); len(ms) > 0 && !slices.Contains(ms, "tls-alpn-01") {
			return CAAVerdict{Problem: fmt.Sprintf("A CAA record at %s no longer allows tls-alpn-01: %s", r.Name, r)}
		}
	}
	return CAAVerdict{OK: true}
}
