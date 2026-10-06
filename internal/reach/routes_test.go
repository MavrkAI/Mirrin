package reach

import (
	"fmt"
	"net/url"
	"os/exec"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
)

const (
	tsHost    = "mac.tail1234.ts.net"
	relayHost = "twin.example.com"
)

// tsState is Tailscale on this computer, as `tailscale status` finds it.
type tsState string

const (
	tsReady    tsState = "ready"     // running, HTTPS certificates on
	tsHTTPSOff tsState = "https-off" // running, HTTPS certificates off
	tsMissing  tsState = "missing"   // no Tailscale command
)

func tsInputs(s tsState) (tailscale.Status, error) {
	var st tailscale.Status
	switch s {
	case tsReady:
		st.BackendState = "Running"
		st.Self.DNSName = tsHost + "."
		st.CertDomains = []string{tsHost}
		return st, nil
	case tsHTTPSOff:
		st.BackendState = "Running"
		st.Self.DNSName = tsHost + "."
		return st, &tailscale.Problem{Message: "enable HTTPS certificates in your tailnet at " + tailscale.AdminURL}
	}
	return st, &tailscale.Problem{Message: "could not find the Tailscale command; install its CLI or open the Tailscale app, then check " + tailscale.AdminURL,
		Cause: fmt.Errorf("Tailscale CLI lookup: %w", &exec.Error{Name: "tailscale", Err: exec.ErrNotFound})}
}

// The route resolver matrix: Tailscale ready, with HTTPS off, or missing,
// each with and without the owner's relay. A working route gives the QR
// code's host; with none, the empty state lists the fixes Tailscale first,
// in these exact words.
func TestRouteMatrix(t *testing.T) {
	type want struct {
		host   string   // the QR code's host; "" for the empty state
		fixes  []string // the empty state's fixes, in order
		urls   []string // and their links
		ts, rl string   // each route's problem, for the "How phones reach" list
	}
	offer := devices.Offer{ID: "of_abcdefghij", Secret: "c2VjcmV0", Kind: devices.KindPWA}
	for _, c := range []struct {
		ts    tsState
		relay bool
		want  want
	}{
		{tsReady, false, want{host: tsHost, rl: RelayNoneProblem}},
		{tsHTTPSOff, false, want{fixes: []string{TSHTTPSOffFix, RelayNoneFix}, urls: []string{tailscale.AdminURL, ""}, ts: TSHTTPSOffProblem, rl: RelayNoneProblem}},
		{tsMissing, false, want{fixes: []string{TSMissingFix, RelayNoneFix}, urls: []string{TailscaleDownload, ""}, ts: TSMissingProblem, rl: RelayNoneProblem}},
		{tsReady, true, want{host: relayHost, ts: TSUnusedProblem}},
		{tsHTTPSOff, true, want{host: relayHost, ts: TSHTTPSOffProblem}},
		{tsMissing, true, want{host: relayHost, ts: TSMissingProblem}},
	} {
		t.Run(fmt.Sprintf("tailscale %s, relay %v", c.ts, c.relay), func(t *testing.T) {
			st, err := tsInputs(c.ts)
			in := RouteInputs{Tailscale: st, TailscaleErr: err}
			if c.relay {
				in.Reach = Config{Mode: "relay", RelayURL: "wss://relay.example.com/v1/tunnel", Hostname: relayHost}
				in.Live = []api.Base{{URL: "https://" + relayHost, Pins: []string{"pin"}}}
				in.RelayReady = true
			} else if c.ts == tsReady {
				in.Reach = Config{Mode: "tailscale"}
				in.Live = []api.Base{{URL: "https://" + tsHost}}
			}
			routes := Routes(in)
			byKind := map[string]api.Route{}
			for _, r := range routes {
				byKind[r.Kind] = r
			}
			if got := byKind[api.RouteTailscale].Problem; got != c.want.ts {
				t.Errorf("tailscale says %q, want %q", got, c.want.ts)
			}
			if got := byKind[api.RouteRelay].Problem; got != c.want.rl {
				t.Errorf("relay says %q, want %q", got, c.want.rl)
			}
			best, ok := api.BestRoute(routes)
			if c.want.host != "" {
				if !ok {
					t.Fatalf("no route, want %s: %+v", c.want.host, routes)
				}
				u, err := url.Parse(api.PairLink(best.BaseURL, offer, "Mirrin"))
				if err != nil || u.Host != c.want.host || u.Scheme != "https" {
					t.Fatalf("the QR code goes to %v (%v), want https://%s", u, err, c.want.host)
				}
				return
			}
			if ok {
				t.Fatalf("a QR code for %s with nothing working", best.BaseURL)
			}
			problems := api.RouteProblems(routes)
			if len(problems) != len(c.want.fixes) || problems[0].Kind != api.RouteTailscale {
				t.Fatalf("empty state %+v, want Tailscale first", problems)
			}
			for i, p := range problems {
				if p.Fix != c.want.fixes[i] || p.FixURL != c.want.urls[i] {
					t.Errorf("fix %d: %q (%s), want %q (%s)", i, p.Fix, p.FixURL, c.want.fixes[i], c.want.urls[i])
				}
			}
		})
	}
}

// The other states say what to do too: Tailscale signed out, a tailnet
// listener still starting, and a relay that isn't connected.
func TestRoutesWhileThingsStart(t *testing.T) {
	r := tailscaleRoute(RouteInputs{Tailscale: tailscale.Status{BackendState: "NeedsLogin"}, TailscaleErr: &tailscale.Problem{Message: "Tailscale is still starting or signed out"}})
	if r.Ready || r.Problem != TSStoppedProblem || r.Fix != TSStoppedFix {
		t.Fatalf("signed out: %+v", r)
	}
	st, _ := tsInputs(tsReady)
	r = tailscaleRoute(RouteInputs{Reach: Config{Mode: "tailscale"}, Tailscale: st})
	if r.Ready || r.Problem != TSStartingProblem {
		t.Fatalf("starting: %+v", r)
	}
	// A listener for another name doesn't count.
	r = tailscaleRoute(RouteInputs{Reach: Config{Mode: "tailscale"}, Tailscale: st, Live: []api.Base{{URL: "https://other.ts.net"}}})
	if r.Ready {
		t.Fatalf("another host counted: %+v", r)
	}
	r = relayRoute(RouteInputs{Reach: Config{Mode: "relay", Hostname: relayHost}, Live: []api.Base{{URL: "https://" + relayHost}}, RelayDetail: "connection refused"})
	if r.Ready || r.Problem != RelayDownProblem+": connection refused." || r.Fix != RelayDownFix {
		t.Fatalf("relay down: %+v", r)
	}
	// Files mode is offered only in files mode, and only while it serves.
	if n := len(Routes(RouteInputs{})); n != 2 {
		t.Fatalf("%d routes with nothing set up", n)
	}
	rs := Routes(RouteInputs{Reach: Config{Mode: "files"}, Live: []api.Base{{URL: "https://twin.home.example:7743"}}})
	if best, ok := api.BestRoute(rs); !ok || best.Kind != api.RouteFiles || best.BaseURL != "https://twin.home.example:7743" {
		t.Fatalf("files: %+v", rs)
	}
}

func TestTailnetPeersPutPhonesFirst(t *testing.T) {
	got := TailnetPeers(tailscale.Status{Peer: map[string]tailscale.PeerStatus{
		"a": {HostName: "nas", OS: "linux", Online: true},
		"b": {DNSName: "akshays-iphone.tail1234.ts.net.", OS: "iOS", Online: false},
		"c": {HostName: "pixel", OS: "android", Online: true},
		"d": {OS: "macOS"},
	}})
	if len(got) != 3 || got[0].Name != "pixel" || got[1].Name != "akshays-iphone" || got[2].Name != "nas" {
		t.Fatalf("%+v", got)
	}
	if got := TailnetPeers(tailscale.Status{}); got == nil || len(got) != 0 {
		t.Fatalf("no peers: %#v", got)
	}
}
