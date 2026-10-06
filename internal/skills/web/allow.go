package web

import (
	"net/netip"
	"strings"
)

// PrivateAddr reports whether ip is this machine, the local network,
// link-local (where cloud metadata services live) or otherwise not a public
// internet address. The browser keeps to the same rule as fetch_url.
func PrivateAddr(ip netip.Addr) bool { return privateAddr(ip) }

// Allowlist is skills.web.allow_hosts, parsed: the names, addresses and
// ranges on the owner's own network the twin may reach anyway. fetch_url and
// the browser share it.
type Allowlist struct{ e *egress }

// NewAllowlist parses allow the way fetch_url does: host names
// ("homeassistant.local"), addresses ("192.168.1.20") or ranges
// ("10.0.0.0/8").
func NewAllowlist(allow []string) Allowlist { return Allowlist{e: newEgress(allow)} }

// Host reports whether the name itself is allowed (wherever it points).
func (a Allowlist) Host(name string) bool {
	return a.e != nil && a.e.hosts[strings.ToLower(strings.TrimSuffix(name, "."))]
}

// Addr reports whether ip is an allowed address or inside an allowed range.
func (a Allowlist) Addr(ip netip.Addr) bool { return a.e != nil && a.e.allowed(ip.Unmap()) }
