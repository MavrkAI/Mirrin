package mail

import (
	"strings"

	"github.com/MavrkAI/Mirrin/internal/skills/email"
)

// The From header is whatever the sender typed, so on its own it proves
// nothing. Mail counts as the owner's only when the receiving server,
// which checked SPF, DKIM and DMARC as the message arrived, recorded a pass
// for the owner's address in its Authentication-Results header.
//
// Anyone can write an Authentication-Results header into a message, so only
// the block the receiving servers wrote counts. They add theirs above
// everything that came with the message, so the block is read from the top
// and ends at the first header that isn't theirs: one from a server that
// isn't trusted, or one from a server name or trusted entry already seen
// (a server writes its own header once). Above the topmost Received header,
// when a trusted server wrote it, only the receiving servers can have
// written, so there a name may repeat: some servers write each check in a header of its
// own (see sealed). Nothing below the block counts, not even for a check
// the servers left out. Inside it the first verdict on each check decides,
// and a DMARC fail overrides every pass. Mail a mailbox sends itself may
// carry no header at all; its copy in Sent vouches for it (SelfSent).

// authResult is one "method=result prop=value …" clause.
type authResult struct {
	method, result string
	props          map[string]string
}

// parseAuthResults splits an Authentication-Results value into the server
// that wrote it ("" if it left its name out, as Microsoft does) and its
// results.
func parseAuthResults(v string) (server string, results []authResult) {
	parts := strings.Split(stripComments(v), ";")
	first := strings.TrimSpace(parts[0])
	if strings.Contains(first, "=") {
		server = ""
	} else {
		if f := strings.Fields(first); len(f) > 0 {
			server = strings.ToLower(f[0]) // a version number may follow
		}
		parts = parts[1:]
	}
	for _, p := range parts {
		toks := fields(p)
		if len(toks) == 0 {
			continue
		}
		method, result, ok := strings.Cut(toks[0], "=")
		if !ok {
			continue
		}
		method, _, _ = strings.Cut(method, "/")
		r := authResult{method: strings.ToLower(method), result: strings.ToLower(result), props: map[string]string{}}
		for _, t := range toks[1:] {
			if k, val, ok := strings.Cut(t, "="); ok {
				r.props[strings.ToLower(k)] = strings.Trim(val, `"<>`)
			}
		}
		results = append(results, r)
	}
	return server, results
}

// sealed is how many of the message's Authentication-Results headers, from
// the top, sit above the topmost Received header, when a trusted server
// wrote that Received. Servers put their headers above what they
// received, so nothing the sender wrote can be up there: those headers are
// all the receiving servers' own, and a server may write several under one
// name (one per check). If the topmost Received isn't a trusted server's
// (a local delivery hop such as Dovecot LMTP under an internal name), or
// there is none, nothing is sealed: a trusted name further down could be
// one the sender forged, so it proves nothing. Below the sealed headers, a
// repeated name ends the block as before.
func sealed(trace []email.Field, trusted []string) int {
	n := 0
	for _, f := range trace {
		switch f.Name {
		case "Authentication-Results":
			n++
		case "Received":
			if by := receivedBy(f.Value); by != "" && trustedServer(by, trusted) >= 0 {
				return n
			}
			return 0 // the topmost Received isn't trusted: fail closed
		}
	}
	return 0 // no Received at all: no telling where the block ends
}

// receivedBy is the server that wrote a Received field: the name after "by".
func receivedBy(v string) string {
	toks := strings.Fields(stripComments(v))
	for i := 0; i+1 < len(toks); i++ {
		if strings.EqualFold(toks[i], "by") {
			return strings.ToLower(strings.TrimRight(toks[i+1], ";."))
		}
	}
	return ""
}

// dmarcFailed reports whether a trusted server recorded a DMARC fail. It
// only spares the mailbox a look in Sent for mail that is plainly forged,
// so a fail anywhere in the headers counts.
func dmarcFailed(headers, trusted []string) bool {
	for _, h := range headers {
		server, results := parseAuthResults(h)
		if trustedServer(server, trusted) < 0 {
			continue
		}
		for _, r := range results {
			if r.method == "dmarc" && r.result == "fail" {
				return true
			}
		}
	}
	return false
}

// vouched reports whether the receiving servers passed mail from owner. The
// first sealed headers are known to be the receiving servers' own, so a
// server's name may repeat among them.
func vouched(headers, trusted []string, owner string, sealed int) bool {
	owner = strings.ToLower(owner)
	domain := domainOf(owner)
	if domain == "" {
		return false
	}
	names := map[string]bool{}   // server names in the block
	entries := map[int]bool{}    // trusted entries that have vouched for a header
	decided := map[string]bool{} // checks already ruled on
	pass := false
	for i, h := range headers {
		server, results := parseAuthResults(h)
		entry := trustedServer(server, trusted)
		if entry < 0 || (i >= sealed && (entries[entry] || names[server])) {
			break // everything from here down came with the message
		}
		entries[entry], names[server] = true, true
		ruled := map[string]bool{}
		for _, r := range results {
			if r.method == "dmarc" && r.result == "fail" {
				return false // the server found the From address isn't authentic
			}
			if decided[r.method] {
				continue
			}
			ruled[r.method] = true
			if r.result == "pass" && passesFor(r, owner, domain) {
				pass = true
			}
		}
		for m := range ruled {
			decided[m] = true
		}
	}
	return pass
}

// topServer names the server that wrote the topmost header, the one to
// trust if it is the owner's mail provider.
func topServer(headers []string) (string, bool) {
	if len(headers) == 0 {
		return "", false
	}
	server, _ := parseAuthResults(headers[0])
	return server, true
}

// passesFor reports whether a passing result is about the owner.
func passesFor(r authResult, owner, domain string) bool {
	switch r.method {
	case "dmarc":
		from := r.props["header.from"]
		return from == "" || domainOf(from) == domain
	case "dkim":
		d := r.props["header.d"]
		if d == "" {
			d = domainOf(r.props["header.i"])
		}
		return aligned(strings.ToLower(d), domain)
	case "spf":
		mf := strings.ToLower(r.props["smtp.mailfrom"])
		if strings.Contains(mf, "@") {
			return mf == owner // the envelope sender itself, not just anyone at the domain
		}
		return mf != "" && mf == domain
	}
	return false
}

// aligned is DMARC's relaxed alignment: the same domain, or one inside the other.
func aligned(d, domain string) bool {
	if d == "" || !strings.Contains(d, ".") {
		return false
	}
	return d == domain || strings.HasSuffix(domain, "."+d) || strings.HasSuffix(d, "."+domain)
}

// trustedServer is the index of the first trusted entry that names server,
// or -1. An entry names a server exactly or as its parent domain
// (messagingengine.com for mx6.messagingengine.com); "" is the nameless
// header Microsoft writes.
func trustedServer(server string, trusted []string) int {
	for i, t := range trusted {
		t = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(t), "."))
		switch {
		case t == "":
			if server == "" {
				return i
			}
		case server == t || strings.HasSuffix(server, "."+t):
			return i
		}
	}
	return -1
}

func domainOf(addr string) string {
	addr = strings.ToLower(strings.Trim(strings.TrimSpace(addr), "<>"))
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

// stripComments drops RFC 5322 (comments), which may nest, outside quotes.
func stripComments(s string) string {
	var b strings.Builder
	depth, quoted := 0, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && i+1 < len(s):
			if depth == 0 {
				b.WriteByte(ch)
				b.WriteByte(s[i+1])
			}
			i++
			continue
		case ch == '"' && depth == 0:
			quoted = !quoted
		case ch == '(' && !quoted:
			depth++
			b.WriteByte(' ')
			continue
		case ch == ')' && !quoted && depth > 0:
			depth--
			continue
		}
		if depth == 0 {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// fields splits on white space, keeping "quoted strings" whole.
func fields(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, ch := range s {
		switch {
		case ch == '"':
			quoted = !quoted
			cur.WriteRune(ch)
		case !quoted && (ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n'):
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(ch)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
