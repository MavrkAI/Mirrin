package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/skills/calendar"
	gauth "github.com/MavrkAI/Mirrin/internal/skills/google"
)

const googleBlurb = "Calendar, Gmail and Drive with one sign-in. Your twin can see your day, read and answer mail, and find files."

// Console pages the setup steps open.
const (
	consoleProject  = "https://console.cloud.google.com/projectcreate"
	consoleAPIs     = "https://console.cloud.google.com/flows/enableapi?apiid=calendar-json.googleapis.com,gmail.googleapis.com,drive.googleapis.com"
	consoleAuth     = "https://console.cloud.google.com/auth/overview"
	consoleAudience = "https://console.cloud.google.com/auth/audience"
	consoleClients  = "https://console.cloud.google.com/auth/clients"
)

// googleSetupSteps is the whole setup, for someone who has never opened the
// Google Cloud console: one screen per step, what to press on it, and the
// mistakes that cost people an evening (Testing's 7-day sign-ins, a Web
// client, the unverified-app warning) headed off.
func googleSetupSteps() []api.Step {
	return []api.Step{
		{Text: "Create a Google Cloud project for your twin. It's free and needs no card; call it Mirrin.", URL: consoleProject, Link: "Create project"},
		{Text: "Turn on Calendar, Gmail and Drive for it: check your new project is the one picked at the top, then press Next and Enable.", URL: consoleAPIs, Link: "Enable the APIs"},
		{Text: "Introduce your app to Google: press Get started, name it Mirrin, pick your email, choose External, give your email again, agree, and press Create.", URL: consoleAuth, Link: "Google Auth Platform"},
		{Text: "On Audience, press Publish app and confirm. Skip this and Google signs your twin out every 7 days. A note that the app needs verification is for public apps; ignore it. (On a Google Workspace account, choosing Internal in the last step does the same.)", URL: consoleAudience, Link: "Audience"},
		{Text: "On Clients, press Create client, choose Desktop app (not Web application), press Create, then Download JSON. Open that file, copy everything in it and paste it in the box below (or copy just the Client ID and secret).", URL: consoleClients, Link: "Clients"},
		{Text: "Press Connect Google, choose your account and tick every box. Google warns that it hasn't verified the app: it's your own, so press Advanced, then Go to Mirrin (unsafe)."},
	}
}

// googleConnectSteps is what is left once the client is saved. Publishing
// comes first: an app left in Testing connects fine and then signs the twin
// out a week later, which is the surprise to head off.
func googleConnectSteps() []api.Step {
	return []api.Step{
		{Text: "Before connecting: on Audience, make sure the app says In production (press Publish app if it doesn't), or Google signs your twin out every 7 days.", URL: consoleAudience, Link: "Audience"},
		{Text: "Press Connect Google, choose your account and tick every box. Google warns that it hasn't verified the app: it's your own, so press Advanced, then Go to Mirrin (unsafe)."},
	}
}

// AccountStates implements api.AccountsBackend.
func (d *Daemon) AccountStates(ctx context.Context) []api.AccountState {
	cfg := d.Config()
	g := api.AccountState{
		Name: "google", Label: "Google", Blurb: googleBlurb,
		HasClient: d.google.HasCredentials(),
		Features: []api.Feature{
			{Key: "calendar", Label: "Calendar", On: cfg.Skills.Calendar.Enabled},
			{Key: "gmail", Label: "Gmail", On: cfg.Skills.Gmail.Enabled},
			{Key: "drive", Label: "Drive (read)", On: cfg.Skills.Drive.Enabled},
		},
	}
	if !d.google.Connected() {
		switch {
		case !g.HasClient:
			g.Steps = googleSetupSteps()
			if _, err := os.Stat(cfg.Skills.Calendar.CredentialsFile + ".rejected"); err == nil {
				g.Blurb = "The OAuth client that was saved was set aside: Google didn't accept it (it was deleted, its secret changed, or it wasn't a Desktop app client), or you chose to use a different one. Make a new one in step 5 and paste it below."
			}
		default:
			g.Steps = googleConnectSteps()
		}
		return []api.AccountState{g}
	}
	if !g.HasClient {
		// A sign-in with no client file. Shown as not connected, with the
		// fields for a client; Google is only blamed when it refused one.
		g.Blurb = "Your twin's Google OAuth client is missing, so Calendar, Gmail and Drive are paused. Paste your Desktop app client below (step 5) and press Connect Google."
		if d.googleClientRefused(cfg) {
			g.Blurb = "Google no longer accepts the OAuth client your twin used (it was deleted, or its secret changed), so Calendar, Gmail and Drive are paused. Make a new Desktop app client (step 5), paste it below and connect again."
		}
		g.Steps = googleSetupSteps()
		return []api.AccountState{g}
	}
	err := d.google.Check(ctx)
	if so, out := gauth.IsSignedOut(err); out {
		// Shown as not connected, so the page offers Connect again.
		g.Blurb = "Google signed your twin out on " + so.At.In(d.location()).Format("Mon 2 Jan") + ", so Calendar, Gmail and Drive are paused."
		switch {
		case so.Client:
			g.HasClient = false
			g.Blurb += " Google no longer accepts the OAuth client, so it needs a new one (step 5)."
			g.Steps = googleSetupSteps()
		default:
			g.Steps = []api.Step{
				{Text: "If your Google project is still in Testing, Google ends sign-ins after 7 days. On Audience, press Publish app so it stops.", URL: consoleAudience, Link: "Audience"},
				{Text: "Press Connect Google and approve again, ticking every box. Nothing else needs redoing."},
			}
		}
		return []api.AccountState{g}
	}
	g.Connected = true
	g.Email = d.googleEmail(ctx)
	if probs := d.google.Problems(); len(probs) > 0 {
		var lines []string
		for _, name := range gauth.APIs {
			if p := probs[name]; p != "" {
				lines = append(lines, p)
			}
		}
		g.Blurb = "Connected, with something to fix: " + strings.Join(lines, " ")
		g.Notices = googleNotices(probs)
	} else if gauth.IsOffline(err) {
		g.Blurb = googleBlurb + " (Google can't be reached right now; it will try again.)"
	}
	return []api.AccountState{g}
}

// googleNotices are a connected account's problems, one per API, each
// linking the console page that turns the API on when that is the fix.
func googleNotices(probs map[string]string) []api.Step {
	var out []api.Step
	for _, name := range gauth.APIs {
		p := probs[name]
		if p == "" {
			continue
		}
		n := api.Step{Text: p}
		if u := gauth.APIURL[name]; u != "" && strings.Contains(p, "turned off") {
			n.URL, n.Link = u, "Turn it on"
		}
		out = append(out, n)
	}
	return out
}

// UseDifferentGoogleClient sets the saved OAuth client aside (as
// .rejected) and signs out of Google, whose sign-in belongs to that client,
// so the page asks for a new client.
func (d *Daemon) UseDifferentGoogleClient(ctx context.Context) error {
	f := d.google.CredentialsFile
	if err := os.Rename(f, f+".rejected"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if d.google.Connected() {
		if err := d.DisconnectGoogle(ctx); err != nil {
			// the client is aside all the same: say so, not that it failed
			return fmt.Errorf("%w: %v", api.ErrGoogleSignOutPending, err)
		}
	}
	return nil
}

var _ api.GoogleClientReplacer = (*Daemon)(nil)

// googleClientRefused reports whether Google refused the OAuth client (it
// was set aside as .rejected, or refused in this run).
func (d *Daemon) googleClientRefused(cfg config.Config) bool {
	if _, err := os.Stat(cfg.Skills.Calendar.CredentialsFile + ".rejected"); err == nil {
		return true
	}
	so, out := d.google.SignedOut()
	return out && so.Client
}

// SaveGoogleClient stores an OAuth client id/secret or an uploaded credentials JSON.
func (d *Daemon) SaveGoogleClient(ctx context.Context, clientID, clientSecret, rawJSON string) error {
	var err error
	if strings.TrimSpace(rawJSON) != "" {
		err = d.google.ImportCredentials([]byte(rawJSON))
	} else {
		err = d.google.WriteClient(clientID, clientSecret)
	}
	if err == nil {
		_ = os.Remove(d.Config().Skills.Calendar.CredentialsFile + ".rejected")
	}
	return err
}

// BeginGoogle returns the URL the browser should open to sign in.
func (d *Daemon) BeginGoogle(ctx context.Context, redirect string) (string, error) {
	if !d.google.HasCredentials() {
		return "", errors.New("paste your OAuth client first (step 5)")
	}
	return d.google.BeginURL(loopbackRedirect(redirect))
}

// BeginGoogleBound is BeginGoogle for a page that keeps binding (in a
// short-lived cookie) and hands it back with the callback, so the sign-in
// can only be finished in the browser that started it.
func (d *Daemon) BeginGoogleBound(ctx context.Context, redirect string) (authURL, binding string, err error) {
	if !d.google.HasCredentials() {
		return "", "", errors.New("paste your OAuth client first (step 5)")
	}
	return d.google.Begin(loopbackRedirect(redirect))
}

// loopbackRedirect sends Google's answer to this computer: a twin listening
// on every address (0.0.0.0) is reachable on 127.0.0.1, which is the only
// kind of address a Desktop app client accepts.
func loopbackRedirect(redirect string) string {
	u, err := url.Parse(redirect)
	if err != nil {
		return redirect
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		u.Host = net.JoinHostPort("127.0.0.1", u.Port())
		return u.String()
	}
	return redirect
}

// FinishGoogle completes the sign-in, turns the Google skills on and
// registers their tools live.
func (d *Daemon) FinishGoogle(ctx context.Context, state, code string) error {
	if err := d.google.Finish(ctx, state, code); err != nil {
		return err
	}
	return d.googleConnected()
}

// FinishGoogleBound is FinishGoogle for a sign-in started with
// BeginGoogleBound: binding is what that browser kept.
func (d *Daemon) FinishGoogleBound(ctx context.Context, state, code, binding string) error {
	if err := d.google.FinishBound(ctx, state, code, binding); err != nil {
		return err
	}
	return d.googleConnected()
}

// ExplainGoogleError is what the callback page says when Google sends the
// browser back with an error instead of a code.
func (d *Daemon) ExplainGoogleError(code string) string { return gauth.ExplainCallback(code) }

// googleConnected turns on what was granted (Google lets people untick
// boxes), starts watching at once (no restart), and checks each API works.
func (d *Daemon) googleConnected() error {
	granted := d.google.Granted()
	on := func(api string) bool { return granted == nil || granted[api] }
	if err := d.UpdateConfig(func(c *config.Config) {
		c.Skills.Calendar.Enabled = on("calendar")
		c.Skills.Gmail.Enabled = on("gmail")
		c.Skills.Drive.Enabled = on("drive")
	}); err != nil {
		return err
	}
	ctx := context.Background()
	_ = d.store.Set(ctx, googleConnectedKey, time.Now().UTC().Format(time.RFC3339))
	_ = d.store.Set(ctx, googleEmailKey, "")
	d.applyGoogle()
	d.restartGoogleWatch()
	d.store.Audit(ctx, "account.connected", "", "google")
	d.googleWork.Add(1)
	go func() {
		defer d.googleWork.Done()
		pctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = d.google.Probe(pctx, googleFeatures(d.Config())...)
		d.googleEmail(pctx)
		d.syncGoogleWatch(false) // the address is known now: IMAP may already watch this inbox
	}()
	return nil
}

// SetGoogleFeature toggles one of the Google skills.
func (d *Daemon) SetGoogleFeature(ctx context.Context, key string, on bool) error {
	if err := d.UpdateConfig(func(c *config.Config) {
		switch key {
		case "calendar":
			c.Skills.Calendar.Enabled = on
		case "gmail":
			c.Skills.Gmail.Enabled = on
		case "drive":
			c.Skills.Drive.Enabled = on
		}
	}); err != nil {
		return err
	}
	d.applyGoogle()
	return nil
}

// DisconnectGoogle forgets the token and unregisters the tools.
func (d *Daemon) DisconnectGoogle(ctx context.Context) error {
	if err := d.google.Disconnect(); err != nil {
		return err
	}
	_ = d.UpdateConfig(func(c *config.Config) {
		c.Skills.Calendar.Enabled = false
		c.Skills.Gmail.Enabled = false
		c.Skills.Drive.Enabled = false
	})
	_ = d.store.Set(context.Background(), googleEmailKey, "")
	d.applyGoogle()
	d.store.Audit(context.Background(), "account.disconnected", "", "google")
	return nil
}

// applyGoogle makes the registry and the watcher match the config and
// connection state.
func (d *Daemon) applyGoogle() {
	d.gmu.Lock()
	defer d.gmu.Unlock()
	cfg := d.Config()
	reg := d.agent.Tools()
	connected := d.google.Connected()
	for _, t := range d.google.GmailTools() {
		if connected && cfg.Skills.Gmail.Enabled {
			reg.Register(t)
		} else {
			reg.Unregister(t.Spec().Name)
		}
	}
	for _, t := range d.google.DriveTools() {
		if connected && cfg.Skills.Drive.Enabled {
			reg.Register(t)
		} else {
			reg.Unregister(t.Spec().Name)
		}
	}
	cal := calendar.New(cfg.Skills.Calendar, d.location())
	for _, t := range cal.Tools() {
		if connected && cfg.Skills.Calendar.Enabled {
			reg.Register(t)
		} else {
			reg.Unregister(t.Spec().Name)
		}
	}
	if connected && cfg.Skills.Calendar.Enabled {
		d.calendar.Store(cal)
	} else {
		d.calendar.Store(nil)
	}
	d.syncGoogleWatch(true)
}
