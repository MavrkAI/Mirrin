package daemon

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/skills/calendar"
	"github.com/MavrkAI/Mirrin/internal/skills/email"
	gauth "github.com/MavrkAI/Mirrin/internal/skills/google"
	"github.com/MavrkAI/Mirrin/internal/watch"
)

// Keys in the key-value store.
const (
	googleConnectedKey = "google.connected_at" // when the owner last signed in
	googleEmailKey     = "google.email"        // the account's address, once known
	googleToldKey      = "google.signout.told" // the sign-in the owner was told Google refused
)

// wireGoogle is called once the daemon is assembled: it listens for Google
// signing the twin out, and watches the Gmail inbox and the calendar when
// they are connected. What was stored before a restart still counts, so
// changes while the twin was off are noticed.
func (d *Daemon) wireGoogle() {
	d.google.OnSignedOut(d.googleSignedOut)
	d.syncGoogleWatch(false)
}

// googleFeatures lists the Google features switched on.
func googleFeatures(cfg config.Config) []string {
	var out []string
	if cfg.Skills.Calendar.Enabled {
		out = append(out, "calendar")
	}
	if cfg.Skills.Gmail.Enabled {
		out = append(out, "gmail")
	}
	if cfg.Skills.Drive.Enabled {
		out = append(out, "drive")
	}
	return out
}

var featureLabel = map[string]string{"calendar": "Calendar", "gmail": "Gmail", "drive": "Drive"}

// syncGoogleWatch makes the watcher follow the Google features: the calendar
// and the Gmail inbox are watched while connected and switched on. fresh
// means a source added now starts from what is there (connecting isn't
// news). Gmail isn't watched when the email skill already watches the same
// inbox over IMAP, so the owner isn't told twice.
func (d *Daemon) syncGoogleWatch(fresh bool) {
	if d.watcher == nil {
		return
	}
	cfg := d.Config()
	connected := d.google.Connected()
	// A client of its own (they share the sign-in): d.calendar belongs to
	// the screen and is swapped by settings changes on other goroutines.
	if connected && cfg.Skills.Calendar.Enabled && cfg.Watch.Calendar {
		d.watcher.Add(calendar.New(cfg.Skills.Calendar, d.location()), fresh && !d.watcher.Has("calendar"))
	} else {
		d.watcher.Remove("calendar")
	}
	gw := d.google.GmailWatch()
	if connected && cfg.Skills.Gmail.Enabled && cfg.Watch.Inbox && !d.imapWatchesGmail(cfg) {
		d.watcher.Add(gw, fresh && !d.watcher.Has(gw.Name()))
	} else {
		d.watcher.Remove(gw.Name())
	}
}

// restartGoogleWatch starts the Google sources from what is there now: a
// new sign-in (maybe another account) is not a week of news.
func (d *Daemon) restartGoogleWatch() {
	if d.watcher == nil {
		return
	}
	if d.watcher.Has("calendar") {
		d.watcher.Add(calendar.New(d.Config().Skills.Calendar, d.location()), true)
	}
	if gw := d.google.GmailWatch(); d.watcher.Has(gw.Name()) {
		d.watcher.Add(gw, true)
	}
}

func (d *Daemon) imapWatchesGmail(cfg config.Config) bool {
	if d.email == nil || !cfg.Skills.Email.Enabled || !cfg.Watch.Inbox {
		return false
	}
	known, _ := d.store.Get(context.Background(), googleEmailKey)
	return known != "" && strings.EqualFold(strings.TrimSpace(cfg.Skills.Email.Username), known)
}

// googleEmail is the connected account's address, remembered so pages and
// the watcher don't ask Google each time.
func (d *Daemon) googleEmail(ctx context.Context) string {
	if v, _ := d.store.Get(ctx, googleEmailKey); v != "" {
		return v
	}
	e := d.google.Email(ctx)
	if e != "" {
		_ = d.store.Set(ctx, googleEmailKey, e)
	}
	return e
}

// accountsPage is the Accounts page on this computer, for messages: it
// carries no key, so it is safe in a chat, and it opens for a browser that
// has used the page before (the menu opens it for any). Settings pages open
// only on this computer's loopback address, which the API always serves
// (on api.listen's port), whatever address it also listens on (a Tailscale
// one, 0.0.0.0): so that is the link.
func (d *Daemon) accountsPage() string {
	listen := d.Config().API.Listen
	if listen == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return ""
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/accounts"
}

// googleSignedOut tells the owner, once per sign-in, that Google has signed
// the twin out and exactly how to fix it.
func (d *Daemon) googleSignedOut(so gauth.SignOut) {
	if d.runCtx == nil {
		return // a one-off check (mirrin doctor) prints the fix itself; the running twin tells the owner
	}
	ctx := context.Background()
	googleToldMu.Lock()
	told, _ := d.store.Get(ctx, googleToldKey)
	if told == so.Token {
		googleToldMu.Unlock()
		return
	}
	_ = d.store.Set(ctx, googleToldKey, so.Token) // claimed: a second notice waits for this one
	googleToldMu.Unlock()
	d.store.Audit(ctx, "account.signed_out", "", "google: "+so.Detail)
	d.log.Warn("Google signed the twin out", "why", so.Detail, "client", so.Client)
	msg := d.signedOutMessage(ctx, so)
	var err error
	if owner := d.ownerChatKey(); owner != "" {
		err = d.Notify(ctx, owner, msg)
	} else {
		err = desktopNotify(d.cfg.Name, msg)
	}
	if err != nil { // nobody heard it: say it again next time it's noticed
		d.log.Warn("couldn't tell the owner Google signed the twin out", "err", err)
		_ = d.store.Set(ctx, googleToldKey, told)
	}
}

// googleToldMu makes telling the owner about a sign-out a claim, so two
// notices at once (the self-check and a tool call) send one message.
var googleToldMu sync.Mutex

// signedOutMessage is the one message the owner gets: what stopped, why
// (as far as can be told), and the fix, with the page to do it on.
func (d *Daemon) signedOutMessage(ctx context.Context, so gauth.SignOut) string {
	where := "open Accounts from my menu"
	if page := d.accountsPage(); page != "" {
		where += " (or " + page + ")"
	}
	if so.Client {
		return "Google stopped accepting the key I use to sign in (the OAuth client was deleted or its secret changed), so I can't see your calendar, mail or files right now. On your computer, " + where + ": it asks for a new client and walks you through it."
	}
	if raw, _ := d.store.Get(ctx, googleConnectedKey); raw != "" {
		if at, err := time.Parse(time.RFC3339, raw); err == nil {
			if days := so.At.Sub(at).Hours() / 24; days >= 6.5 && days < 8 {
				return "Google signed me out, so I can't see your calendar, mail or files right now: it's been 7 days since you connected, and Google ends sign-ins after 7 days while your Google project is in Testing. To fix it for good, on your computer " + where + ", press Publish app on the Audience page it links to, then press Connect Google. It takes two minutes."
			}
		}
	}
	return "Google signed me out (the sign-in expired or was removed), so I can't see your calendar, mail or files right now. On your computer, " + where + " and press Connect Google. If your Google project is still in Testing, press Publish app on its Audience page first, or this happens every 7 days."
}

// What the Google self-check says for a sign-out, which googleSignedOut has
// told the owner about already (usage.go selfNotifying).
const (
	googleSignedOutDetail     = "Google signed Mirrin out"
	googleClientRefusedDetail = "Google no longer accepts the OAuth client"
)

// googleCheck is the self-check for Google: signed in, still accepted by
// Google, and every switched-on API working.
func (d *Daemon) googleCheck() health.Check {
	return health.Func("google", "Google", d.googleHealth, nil)
}

func (d *Daemon) googleHealth(ctx context.Context) (health.State, string, string) {
	cfg := d.Config()
	feats := googleFeatures(cfg)
	if !d.google.Connected() {
		if len(feats) == 0 {
			return health.Off, "not connected", "to add Calendar, Gmail and Drive, open Accounts from the menu"
		}
		return health.Fail, "not signed in", "open Accounts from the menu and press Connect Google"
	}
	if len(feats) == 0 {
		return health.Off, "connected, with every feature switched off", ""
	}
	probs, err := d.google.Probe(ctx, feats...)
	if err != nil {
		if so, out := gauth.IsSignedOut(err); out {
			// Told once per sign-in (googleToldKey); when telling failed
			// (no channel up yet), this hourly check tries again.
			d.googleWork.Add(1)
			go func() {
				defer d.googleWork.Done()
				d.googleSignedOut(so)
			}()
			if so.Client {
				return health.Fail, googleClientRefusedDetail, "open Accounts from the menu: it asks for a new Desktop app client (Google Auth Platform → Clients), then press Connect Google"
			}
			return health.Fail, googleSignedOutDetail + " on " + so.At.In(d.location()).Format("Mon 2 Jan"),
				"open Accounts from the menu and press Connect Google. If your Google project is in Testing, press Publish app on its Audience page (" + consoleAudience + ") first, or this happens every 7 days"
		}
		if gauth.IsOffline(err) {
			return health.Warn, "couldn't reach Google just now", "check the internet connection; this is checked again every hour"
		}
		return health.Fail, err.Error(), "open Accounts from the menu"
	}
	var broken []string
	fix := ""
	for _, f := range feats {
		if p := probs[f]; p != "" {
			broken = append(broken, featureLabel[f])
			if fix == "" {
				fix = p
			}
		}
	}
	if len(broken) > 0 {
		return health.Fail, strings.Join(broken, " and ") + " not working", fix
	}
	var labels []string
	for _, f := range feats {
		labels = append(labels, featureLabel[f])
	}
	detail := strings.Join(labels, ", ")
	if e := d.googleEmail(ctx); e != "" {
		detail += " for " + e
		d.syncGoogleWatch(false) // now that the address is known
	}
	return health.OK, detail, ""
}

// googleWatchSources lists the Google sources the watcher has, for tests
// and the log.
func (d *Daemon) googleWatchSources() []string {
	if d.watcher == nil {
		return nil
	}
	var out []string
	for _, n := range d.watcher.Names() {
		if n == "calendar" || n == "gmail" {
			out = append(out, n)
		}
	}
	return out
}

// Both inboxes tell the watcher when mail arrived, so only new mail is news.
var (
	_ watch.Arrivals = (*gauth.GmailWatch)(nil)
	_ watch.Arrivals = (*email.Client)(nil)
)
