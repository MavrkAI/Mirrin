package reach

import (
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"strings"
)

// Bring your own domain (docs/cloud-design.md §6.2). With the name in your
// own DNS, pointed straight at the relay's addresses and pinned by CAA to
// this machine's ACME account and TLS-ALPN-01, nobody who controls the
// relay, the relay's DNS or its IP routing can get a certificate for it.

// Record is one DNS record to publish.
type Record struct {
	Name  string
	Type  string
	TTL   int
	Value string
}

// String is the record as a zone-file line.
func (r Record) String() string {
	return fmt.Sprintf("%s. %d IN %s %s", r.Name, r.TTL, r.Type, r.Value)
}

// BYODRecords are the records for host: A and AAAA to the relay (never a
// CNAME, which would hand the name's CAA to the relay's DNS), the _mirrin
// TXT naming the ACME account, and CAA pinning that account to
// TLS-ALPN-01 with wildcards forbidden. Nothing checks the TXT record yet.
func BYODRecords(host, accountURI string, relayIPs []netip.Addr) []Record {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	var out []Record
	for _, ip := range relayIPs {
		ip = ip.Unmap()
		switch {
		case ip.Is4():
			out = append(out, Record{host, "A", 300, ip.String()})
		case ip.Is6():
			out = append(out, Record{host, "AAAA", 300, ip.String()})
		}
	}
	if accountURI != "" {
		out = append(out,
			Record{"_mirrin." + host, "TXT", 300, fmt.Sprintf("%q", "v=mirrin1; acct="+accountID(accountURI))},
			Record{host, "CAA", 300, fmt.Sprintf("0 issue %q", caaIssuer(accountURI)+"; accounturi="+accountURI+"; validationmethods=tls-alpn-01")},
		)
	}
	out = append(out, Record{host, "CAA", 300, `0 issuewild ";"`})
	return out
}

func accountID(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	return path.Base(u.Path)
}

// caaIssuer is the CA's CAA identity for an account URL: Let's Encrypt's
// is letsencrypt.org; others are taken from the host's last two labels.
func caaIssuer(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	if h == "letsencrypt.org" || strings.HasSuffix(h, ".letsencrypt.org") {
		return "letsencrypt.org"
	}
	labels := strings.Split(h, ".")
	if len(labels) > 2 {
		labels = labels[len(labels)-2:]
	}
	return strings.Join(labels, ".")
}
