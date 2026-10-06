package server

import (
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"time"

	"github.com/MavrkAI/Mirrin/internal/relay/wire"
)

// metrics are the relay's counters, served in the Prometheus text format
// on metrics_listen. No metric is labelled by handle or address, and every
// label has a fixed set of values.
type metrics struct {
	tunnels     atomic.Int64 // gauge
	splicing    atomic.Int64 // gauge
	hellos      *counterVec  // by result: welcome or a refusal code
	conns       *counterVec  // client connections, by what happened to them
	toDaemon    atomic.Int64 // spliced bytes
	toClient    atomic.Int64
	suspensions atomic.Int64
	denySeq     atomic.Int64
	denyAt      atomic.Int64 // unix seconds of the last good fetch
	denyErrors  atomic.Int64
	certErrors  atomic.Int64 // the control name's certificate could not be had
	certExpiry  atomic.Int64 // unix seconds: NotAfter of the control certificate last served
}

// Connection results.
const (
	connSpliced   = "spliced"
	connControl   = "control"
	connUnknown   = "unknown_name"
	connBadHello  = "bad_hello"
	connFull      = "stream_limit"
	connBusy      = "busy"
	connSuspended = "suspended"
	connNoSession = "no_session"
)

func newMetrics() *metrics {
	return &metrics{
		hellos: newCounterVec("mirrin_relay_hellos_total", "Tunnel hellos, by result.", "result",
			"welcome", wire.CodeEntitlementExpired, wire.CodeBadSignature, wire.CodeHostnameNotAllowed, wire.CodeDenied,
			wire.CodeSuperseded, wire.CodeSupersededRetry, wire.CodeRateLimited, wire.CodeUpgradeRequired),
		conns: newCounterVec("mirrin_relay_connections_total", "Client connections on the TLS port, by outcome.", "result",
			connSpliced, connControl, connUnknown, connBadHello, connFull, connBusy, connSuspended, connNoSession),
	}
}

// counterVec is a counter with one label whose values are fixed up front.
type counterVec struct {
	name, help, label string
	values            []string
	c                 map[string]*atomic.Int64
}

func newCounterVec(name, help, label string, values ...string) *counterVec {
	v := &counterVec{name: name, help: help, label: label, values: append(values, "other"), c: map[string]*atomic.Int64{}}
	for _, l := range v.values {
		v.c[l] = new(atomic.Int64)
	}
	return v
}

func (v *counterVec) inc(value string) {
	c := v.c[value]
	if c == nil {
		c = v.c["other"]
	}
	c.Add(1)
}

func (v *counterVec) get(value string) int64 { return v.c[value].Load() }

func (v *counterVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", v.name, v.help, v.name)
	for _, l := range slices.Sorted(slices.Values(v.values)) {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", v.name, v.label, l, v.c[l].Load())
	}
}

// write renders every metric.
func (m *metrics) write(w io.Writer, now time.Time) {
	one := func(name, typ, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, typ, name, v)
	}
	one("mirrin_relay_tunnels", "gauge", "Tunnels attached now.", m.tunnels.Load())
	one("mirrin_relay_splices", "gauge", "Client connections being spliced now.", m.splicing.Load())
	m.hellos.write(w)
	m.conns.write(w)
	fmt.Fprintf(w, "# HELP mirrin_relay_splice_bytes_total Bytes spliced, by direction.\n# TYPE mirrin_relay_splice_bytes_total counter\n")
	fmt.Fprintf(w, "mirrin_relay_splice_bytes_total{dir=\"to_client\"} %d\n", m.toClient.Load())
	fmt.Fprintf(w, "mirrin_relay_splice_bytes_total{dir=\"to_daemon\"} %d\n", m.toDaemon.Load())
	one("mirrin_relay_suspensions_total", "counter", "Handles suspended by the distinct-client heuristic.", m.suspensions.Load())
	one("mirrin_relay_denylist_seq", "gauge", "Sequence number of the deny list in force; 0 before the first.", m.denySeq.Load())
	age := int64(-1)
	if at := m.denyAt.Load(); at > 0 {
		age = now.Unix() - at
	}
	one("mirrin_relay_denylist_age_seconds", "gauge", "Seconds since a deny list was last fetched and verified; -1 before the first.", age)
	one("mirrin_relay_denylist_errors_total", "counter", "Deny list fetches that failed or did not verify.", m.denyErrors.Load())
	one("mirrin_relay_control_cert_errors_total", "counter", "Handshakes for the control name that got no certificate.", m.certErrors.Load())
	one("mirrin_relay_control_cert_expiry_timestamp_seconds", "gauge", "When the control certificate last served expires; 0 before the first.", m.certExpiry.Load())
}
