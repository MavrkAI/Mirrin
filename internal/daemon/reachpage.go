package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/reach"
)

// The Reach page (api.ReachBackend): the three ways in, the security panel
// for the one in use, Verify now, and the one click that links this machine
// and switches to the paid handle. The page names the paid service; the
// daemon's own words here never do (the twin never pitches it).

// linkPollEvery is how often a checkout started from the page is polled.
var linkPollEvery = 3 * time.Second

// linking marks a twin with a checkout being polled.
var linking sync.Map // *Daemon → bool

// cloudRouteInputs adds the handle to the routes when reach.mode is cloud.
func (d *Daemon) cloudRouteInputs(in *reach.RouteInputs) {
	if in.Reach.Mode != "cloud" {
		return
	}
	if e := d.cloudEndpoint(); e != nil {
		in.CloudHost = e.Hostname
		in.CloudReady, in.CloudDetail = reach.CloudStatus(e)
		return
	}
	if _, detail, _ := d.cloudHealth(); detail != "" {
		in.CloudDetail = detail
	}
}

// cloudLine is the menu's reach line while the handle needs saying:
// another machine took it over, or payment lapsed. "" otherwise.
func (d *Daemon) cloudLine() string {
	if at, ok := d.cloudStandingBy(); ok {
		return reach.MovedLine(d.Config().Name, at)
	}
	if e := d.cloudEndpoint(); e != nil {
		if cl, kind := e.CloudState(); kind == cloud.Grace {
			return "Reachable at " + e.Hostname + " until " + cl.Exp.Local().Format("2 Jan") + " (payment lapsed)"
		}
	}
	return ""
}

// ReachInfo is what the Reach page shows.
func (d *Daemon) ReachInfo(ctx context.Context) api.ReachInfo {
	cfg := d.Config()
	mode := cfg.Reach.Mode
	if mode == "" {
		mode = "off"
	}
	info := api.ReachInfo{Mode: mode}
	routes := d.Routes(ctx)
	card := func(kind, routeKind string) api.ReachCard {
		c := api.ReachCard{Kind: kind, Using: mode == kind}
		for _, r := range routes {
			if r.Kind == routeKind {
				c.Ready, c.Address, c.Detail, c.Fix, c.FixURL = r.Ready, r.BaseURL, r.Problem, r.Fix, r.FixURL
			}
		}
		return c
	}
	info.Cards = []api.ReachCard{card(api.ReachTailscale, api.RouteTailscale), card(api.ReachRelay, api.RouteRelay)}
	if mode == "files" {
		info.Cards[0].Using = false
	}

	cc := api.ReachCard{Kind: api.ReachCloud, Using: mode == "cloud", Available: len(cloudKeys()) > 0}
	if cc.Using {
		cc = card(api.ReachCloud, api.RouteCloud)
		cc.Using, cc.Available = true, len(cloudKeys()) > 0
	}
	if st, err := cloud.OpenStateWithKeys(cfg.DataDir, cloudKeys()); err == nil && st.Linked() {
		cc.Linked = true
		cl, kind := st.Current(time.Now())
		cc.State = kind.String()
		if kind == cloud.Grace {
			cc.Until = cl.Exp
		}
		if e := d.cloudEndpoint(); e != nil && e.Listener != nil {
			for _, s := range e.Listener.Status() {
				cc.Relays = append(cc.Relays, api.RelayTunnel{ID: s.Relay, Online: s.Online})
			}
		}
		info.Security = d.reachSecurity(ctx)
		if info.Security != nil {
			info.Security.Egress = egressSummary(cfg.DataDir)
		}
	}
	info.Cards = append(info.Cards, cc)
	if info.Security == nil {
		info.Security = d.reachSecurity(ctx)
	}
	if at, ok := d.cloudStandingBy(); ok {
		info.Moved = reach.MovedLine(cfg.Name, at)
	}
	return info
}

// reachSecurity is the security panel for the address in use, or nil when
// nothing is served to other devices.
func (d *Daemon) reachSecurity(ctx context.Context) *api.ReachSecurity {
	if e := d.relayEndpoint(); e != nil {
		s := &api.ReachSecurity{Host: e.Hostname}
		if e.Pins != nil {
			if p := e.Pins(); len(p) > 0 {
				s.Served = p[0]
				if len(p) > 1 {
					s.Next = p[1]
				}
			}
		}
		if e.AccountURI != nil {
			s.AccountURI = e.AccountURI()
		}
		if e.Watcher != nil {
			st := e.Watcher.Status()
			switch {
			case st.CAAProblem != "":
				s.CAA = st.CAAProblem
			case st.Checked.IsZero():
				s.CAA = "Not checked yet: the first check runs once the certificate is in place."
			default:
				s.CAA, s.CAAOK = "Only this computer's certificate account can get certificates for "+e.Hostname+".", true
			}
			h, detail, fix := e.Watcher.Health(ctx)
			s.CT = &api.CTStatus{Checked: st.Checked, SourcesUp: st.SourcesUp, SourcesDown: st.SourcesDown, CAAProblem: st.CAAProblem, State: string(h), Detail: sentenceCase(detail), Fix: fix}
			if e.Alarm != nil {
				if a := e.Alarm.State(); a.Active {
					s.CT.Alarm = "Certificate alarm since " + a.Since.Local().Format("2 Jan 15:04") + ": " + a.Detail + "."
					s.CT.State = "fail"
				}
			}
		}
		return s
	}
	if srv := d.pageServer(); srv != nil {
		if certs := srv.RemoteCerts(); len(certs) > 0 {
			return &api.ReachSecurity{Host: certs[0].Host, Served: certs[0].Current, Next: certs[0].Next}
		}
	}
	return nil
}

// egressSummary sums what this machine sent to the linked service.
func egressSummary(dataDir string) *api.EgressSummary {
	es, _, err := cloud.ReadLedger(dataDir)
	if err != nil {
		return nil
	}
	out := &api.EgressSummary{Requests: len(es)}
	for _, e := range es {
		out.Sent += e.ReqBytes
		out.Received += e.RespBytes
		if e.At.After(out.Last) {
			out.Last = e.At
		}
	}
	return out
}

// ReachVerify checks the address in use from outside, as a phone would.
func (d *Daemon) ReachVerify(ctx context.Context) ([]api.ReachCheck, error) {
	e := d.relayEndpoint()
	if e == nil {
		return nil, &api.HumanError{Sentence: "Verify now checks an address carried by a relay, and none is running.", Fix: "With Tailscale, open your address on your phone to check it."}
	}
	rep, err := reach.Verify(ctx, e)
	if err != nil {
		return nil, err
	}
	out := make([]api.ReachCheck, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		out = append(out, api.ReachCheck{Name: c.Name, OK: c.OK, Warn: c.Warn, Detail: sentenceCase(c.Detail)})
	}
	return out, nil
}

// ReachUseCloud switches reach to the handle. A machine that isn't linked
// starts a checkout (or resumes the one it started) and returns its page;
// once it is paid, reach switches by itself.
func (d *Daemon) ReachUseCloud(ctx context.Context, handle string) (string, error) {
	keys := cloudKeys()
	if len(keys) == 0 {
		return "", &api.HumanError{Sentence: "This version can't link to the paid address yet: it's coming.", Fix: "Tailscale and your own relay work today, for free."}
	}
	if handle != "" && cloud.CheckHandle(handle) != nil {
		return "", &api.HumanError{Sentence: "An address name is 3 to 32 letters, digits and single hyphens.", Fix: "Or leave it empty for a random one."}
	}
	if st, err := backup.LoadState(d.Config().DataDir); err == nil && st.Standby != nil {
		// A machine standing by after a handover sends nothing, not even a
		// checkout: the twin lives on the other machine now.
		return "", &api.HumanError{Sentence: "This copy is standing by: the twin moved to another computer.", Fix: "Run `mirrin backup resume` to bring it back here first."}
	}
	cfg := d.Config()
	c, err := openLinked(cfg.DataDir)
	if err != nil {
		return "", err
	}
	if c != nil {
		if _, kind := c.State().Current(time.Now()); kind == cloud.Superseded {
			return "", &api.HumanError{Sentence: "Another computer holds your address now.", Fix: "This copy stands by; the address stays with the computer that took it over."}
		}
		if cfg.Reach.Mode == "cloud" {
			return "", nil
		}
		return "", d.useCloudReach(d.runCtx)
	}
	base := cfg.Cloud.API
	if base == "" {
		base = cloud.DefaultAPI
	}
	if c, err = cloud.New(cfg.DataDir, base, keys); err != nil {
		return "", err
	}
	acct := ""
	ls, _, resumed := c.PendingLink()
	if !resumed {
		acct = reach.LinkedAccount(cfg.DataDir, cfg.Reach)
		if ls, err = c.StartLink(ctx, handle, acct); err != nil {
			var ae *cloud.APIError
			if errors.As(err, &ae) && ae.Message != "" {
				return "", &api.HumanError{Sentence: ae.Message}
			}
			return "", &api.HumanError{Sentence: "I couldn't start the checkout: " + strings.TrimPrefix(err.Error(), "cloud: ") + ".", Fix: "Check this computer is online, then try again."}
		}
	}
	d.store.Audit(ctx, "reach.link", "", c.API())
	if _, busy := linking.LoadOrStore(d, true); !busy {
		go d.finishLink(c, ls.ID, acct)
	}
	return ls.CheckoutURL, nil
}

// finishLink polls a checkout until it is paid, then switches reach to the
// handle; it gives up when the checkout expires or after two hours.
func (d *Daemon) finishLink(c *cloud.Client, id, acct string) {
	defer linking.Delete(d)
	ctx := d.runCtx
	if ctx == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	for {
		st, _, err := c.PollLink(ctx, id)
		switch {
		case err != nil:
			d.log.Warn("link: poll", "err", err)
		case st == cloud.LinkExpired:
			d.log.Info("link: the checkout expired unpaid")
			return
		case st == cloud.LinkActive:
			if acct != "" {
				if info, ok, _ := c.State().Info(); ok {
					_ = reach.NotePinned(d.Config().DataDir, info.Handle, info.Gen, acct)
				}
			}
			if err := d.useCloudReach(d.runCtx); err != nil {
				d.log.Warn("link: switch reach", "err", err)
			}
			return
		}
		if !pauseFor(ctx, linkPollEvery) {
			return
		}
	}
}
