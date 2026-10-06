package reach

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
)

// How a phone can reach the twin, for "Add your phone" and "Reach from
// anywhere" (api.RoutesBackend): each free route, whether it works now, and
// if not, the one thing to do next.

// TailscaleDownload is where Tailscale is installed from.
const TailscaleDownload = "https://tailscale.com/download"

// The words each route uses when it doesn't work yet. Tests hold them to
// the letter: they are what the owner reads.
const (
	TSMissingProblem  = "Tailscale isn't installed on this computer."
	TSMissingFix      = "Install Tailscale here and on your phone, and sign in to both with the same account."
	TSStoppedProblem  = "Tailscale is installed, but it isn't running or signed in."
	TSStoppedFix      = "Open Tailscale on this computer and sign in."
	TSHTTPSOffProblem = "Tailscale is running, but HTTPS certificates are off for your tailnet."
	TSHTTPSOffFix     = "In the Tailscale admin console, open DNS and turn on HTTPS Certificates."
	TSUnusedProblem   = "Tailscale is ready, but your twin isn't using it yet."
	TSUnusedFix       = "Run `mirrin reach use tailscale` on this computer."
	TSStartingProblem = "Starting HTTPS on your tailnet…"
	TSStartingFix     = "Give it a minute. If it stays like this, open Health from the menu."
	RelayNoneProblem  = "No relay of your own is set up."
	RelayNoneFix      = "If you run mirrin-relay on a server of yours, run `mirrin reach use relay` on this computer."
	RelayDownProblem  = "Your relay isn't connected yet"
	RelayDownFix      = "Run `mirrin reach verify` on this computer to see what's wrong."
	FilesDownProblem  = "Your certificate isn't being served yet."
	FilesDownFix      = "Check reach.cert_file and reach.key_file in config.yaml, then open Health from the menu."
	CloudDownProblem  = "Your address isn't connected yet"
	CloudDownFix      = "Run `mirrin reach verify` on this computer to see what's wrong."
)

// RouteInputs is what the routes are worked out from.
type RouteInputs struct {
	Reach Config
	// Tailscale is `tailscale status`, and the error it gave.
	Tailscale    tailscale.Status
	TailscaleErr error
	// Live are the TLS listeners serving now (api.Server.RemoteBases).
	Live []api.Base
	// RelayReady says the relay tunnel is up with a certificate; when it
	// isn't, RelayDetail says why.
	RelayReady  bool
	RelayDetail string
	// CloudHost is the paid handle's name when reach.mode is cloud;
	// CloudReady says its tunnels are up with a certificate, and when they
	// aren't, CloudDetail says why.
	CloudHost   string
	CloudReady  bool
	CloudDetail string
}

// liveAt is the live listener's address for host, if one serves it.
func liveAt(live []api.Base, host string) (string, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, b := range live {
		u, err := url.Parse(b.URL)
		if err != nil || u.Scheme != "https" {
			continue
		}
		if strings.ToLower(u.Hostname()) == host && host != "" {
			return b.URL, true
		}
	}
	return "", false
}

// tailscaleMissing reports whether err means the Tailscale command isn't
// there at all.
func tailscaleMissing(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	var p *tailscale.Problem
	return errors.As(err, &p) && strings.HasPrefix(p.Message, "could not find the Tailscale command")
}

// tailscaleRoute is the tailnet route.
func tailscaleRoute(in RouteInputs) api.Route {
	r := api.Route{Kind: api.RouteTailscale}
	st := in.Tailscale
	running := st.BackendState == "Running"
	switch {
	case in.TailscaleErr != nil && tailscaleMissing(in.TailscaleErr):
		r.Problem, r.Fix, r.FixURL = TSMissingProblem, TSMissingFix, TailscaleDownload
		return r
	case !running:
		r.Problem, r.Fix = TSStoppedProblem, TSStoppedFix
		return r
	}
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	https := false
	for _, d := range st.CertDomains {
		if strings.EqualFold(strings.TrimSuffix(d, "."), host) && host != "" {
			https = true
		}
	}
	switch {
	case !https:
		r.Problem, r.Fix, r.FixURL = TSHTTPSOffProblem, TSHTTPSOffFix, tailscale.AdminURL
	case in.Reach.Mode != "tailscale":
		r.Problem, r.Fix = TSUnusedProblem, TSUnusedFix
	default:
		if u, ok := liveAt(in.Live, host); ok {
			r.Ready, r.BaseURL = true, u
		} else {
			r.Problem, r.Fix = TSStartingProblem, TSStartingFix
		}
	}
	return r
}

// relayRoute is the owner's own relay.
func relayRoute(in RouteInputs) api.Route {
	r := api.Route{Kind: api.RouteRelay}
	if in.Reach.Mode != "relay" {
		r.Problem, r.Fix = RelayNoneProblem, RelayNoneFix
		return r
	}
	if u, ok := liveAt(in.Live, in.Reach.Hostname); ok && in.RelayReady {
		r.Ready, r.BaseURL = true, u
		return r
	}
	r.Problem, r.Fix = RelayDownProblem+".", RelayDownFix
	if in.RelayDetail != "" {
		r.Problem = RelayDownProblem + ": " + in.RelayDetail + "."
	}
	return r
}

// cloudRoute is the paid handle, listed only when the owner chose it
// (reach.mode cloud): the pages never offer it otherwise.
func cloudRoute(in RouteInputs) api.Route {
	r := api.Route{Kind: api.RouteCloud}
	if u, ok := liveAt(in.Live, in.CloudHost); ok && in.CloudReady {
		r.Ready, r.BaseURL = true, u
		return r
	}
	r.Problem, r.Fix = CloudDownProblem+".", CloudDownFix
	if in.CloudDetail != "" {
		r.Problem = CloudDownProblem + ": " + in.CloudDetail + "."
	}
	return r
}

// Routes lists the free routes: Tailscale, your relay, and your own
// certificate files when that is the mode; and the paid handle last, only
// when that is the mode.
func Routes(in RouteInputs) []api.Route {
	out := []api.Route{tailscaleRoute(in), relayRoute(in)}
	if in.Reach.Mode == "files" {
		r := api.Route{Kind: api.RouteFiles, Problem: FilesDownProblem, Fix: FilesDownFix}
		for _, b := range in.Live {
			if u, err := url.Parse(b.URL); err == nil && u.Scheme == "https" && (in.Tailscale.Self.DNSName == "" || !strings.EqualFold(u.Hostname(), strings.TrimSuffix(in.Tailscale.Self.DNSName, "."))) {
				r = api.Route{Kind: api.RouteFiles, Ready: true, BaseURL: b.URL}
				break
			}
		}
		out = append(out, r)
	}
	if in.Reach.Mode == "cloud" {
		out = append(out, cloudRoute(in))
	}
	return out
}

// TailnetPeers are the tailnet's other devices, phones first.
func TailnetPeers(st tailscale.Status) []api.TailnetPeer {
	out := []api.TailnetPeer{}
	for _, p := range st.Peer {
		name := p.HostName
		if name == "" {
			name, _, _ = strings.Cut(strings.TrimSuffix(p.DNSName, "."), ".")
		}
		if name == "" {
			continue
		}
		out = append(out, api.TailnetPeer{Name: name, OS: p.OS, Online: p.Online})
	}
	phone := func(os string) bool { return strings.EqualFold(os, "iOS") || strings.EqualFold(os, "android") }
	sort.Slice(out, func(i, j int) bool {
		if phone(out[i].OS) != phone(out[j].OS) {
			return phone(out[i].OS)
		}
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// RelayStatus says whether a running relay endpoint can take a phone now:
// connected, with a certificate. When it can't, detail says why.
func RelayStatus(e *Endpoint) (ready bool, detail string) {
	if e == nil || e.Listener == nil {
		return false, "not started"
	}
	if !e.Online() {
		_, msg, _ := relayHealth(e)
		return false, strings.TrimPrefix(msg, "relay not connected: ")
	}
	if e.ACME == nil || e.ACME.Leaf() == nil {
		return false, "getting a certificate for " + e.Hostname
	}
	return true, ""
}
