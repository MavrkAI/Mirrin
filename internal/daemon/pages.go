package daemon

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/push"
	"github.com/MavrkAI/Mirrin/internal/reach"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
)

// The device and safety pages (api.LocalPages): "Add your phone",
// Devices, Backup, Trust and the device review after a restore, and the
// menu entries that open them.

// relayEndpoints holds each running twin's relay reach (relay mode only).
var relayEndpoints sync.Map // *Daemon → *reach.Endpoint

// pageServers holds the API server each running twin's pages are on, for
// the menu's links.
var pageServers sync.Map // *Daemon → *api.Server

// tailscaleStatus is how the pages ask Tailscale; tests replace it.
var tailscaleStatus = func(ctx context.Context) (tailscale.Status, error) {
	return (&tailscale.CLI{}).Status(ctx)
}

// tsCache keeps `tailscale status` for a few seconds: the page asks on
// every new code, and the menu every few seconds.
type tsCache struct {
	mu  sync.Mutex
	at  time.Time
	st  tailscale.Status
	err error
	ttl time.Duration
}

// tsHungFor is how long a Tailscale that didn't answer at all is left
// alone: each ask hangs until it's killed, so asking every few seconds
// only piles up stuck commands and warnings.
const tsHungFor = 2 * time.Minute

var tsCaches sync.Map // *Daemon → *tsCache

func (d *Daemon) tailnet(ctx context.Context) (tailscale.Status, error) {
	v, _ := tsCaches.LoadOrStore(d, &tsCache{})
	c := v.(*tsCache)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < c.ttl {
		return c.st, c.err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c.st, c.err = tailscaleStatus(ctx)
	c.at, c.ttl = time.Now(), 10*time.Second
	if c.err != nil && ctx.Err() != nil {
		c.ttl = tsHungFor
	}
	return c.st, c.err
}

// attachPages adds the device and safety pages to the API.
func (d *Daemon) attachPages(ctx context.Context, srv *api.Server) {
	srv.WithLocalPages(d)
	srv.WithReachPage(d) // reachpage.go
	pageServers.Store(d, srv)
	context.AfterFunc(ctx, func() { pageServers.CompareAndDelete(d, srv); tsCaches.Delete(d) })
	if d.RestoreReview(ctx).Pending {
		// After a restore: a snapshot from before a device was removed
		// brings it back, so the owner is asked to look.
		msg := "I was restored from a backup. Check the devices that can reach me, and remove any you don't recognise: open Review devices from my menu."
		d.bus.Publish(events.Event{Kind: "notice", Text: msg})
		_ = desktopNotify(d.Config().Name, msg)
	}
}

func (d *Daemon) pageServer() *api.Server {
	if v, ok := pageServers.Load(d); ok {
		return v.(*api.Server)
	}
	return nil
}

func (d *Daemon) relayEndpoint() *reach.Endpoint {
	if v, ok := relayEndpoints.Load(d); ok {
		return v.(*reach.Endpoint)
	}
	return nil
}

// PageURL is the menu's link to one of the twin's pages ("" while the API
// is off).
func (d *Daemon) PageURL(path string) string {
	if s := d.pageServer(); s != nil {
		return s.PageURL(path)
	}
	return ""
}

// DeviceCount is how many devices are paired (browsers on this computer
// aside), for the menu's "Devices (n)".
func (d *Daemon) DeviceCount() int {
	store := d.deviceStore()
	if store == nil {
		return 0
	}
	n := 0
	for _, dev := range store.List() {
		if !dev.Revoked() && !dev.Local() {
			n++
		}
	}
	return n
}

// ReachLine is the menu's status line: where other devices reach the twin,
// or that they can't.
func (d *Daemon) ReachLine() string {
	if line := d.cloudLine(); line != "" { // reachpage.go: moved, or payment lapsed
		return line
	}
	s := d.pageServer()
	if s == nil {
		return "Only on this Mac"
	}
	if best, ok := api.BestRoute(d.Routes(context.Background())); ok {
		if u, err := url.Parse(best.BaseURL); err == nil {
			return "Reachable at " + u.Host
		}
	}
	return "Only on this Mac"
}

// Routes says how a phone can reach the twin now.
func (d *Daemon) Routes(ctx context.Context) []api.Route {
	in := reach.RouteInputs{Reach: d.Config().Reach}
	in.Tailscale, in.TailscaleErr = d.tailnet(ctx)
	if s := d.pageServer(); s != nil {
		in.Live = s.RemoteBases()
	}
	in.RelayReady, in.RelayDetail = reach.RelayStatus(d.relayEndpoint())
	d.cloudRouteInputs(&in) // reachpage.go
	return reach.Routes(in)
}

// TailnetPeers are the owner's other devices on Tailscale.
func (d *Daemon) TailnetPeers(ctx context.Context) []api.TailnetPeer {
	st, _ := d.tailnet(ctx)
	return reach.TailnetPeers(st)
}

// ---- Backup ----

func (d *Daemon) backupScheduler() *backup.Scheduler { return d.backups }

// BackupStatus is what the Backup page shows.
func (d *Daemon) BackupStatus(ctx context.Context) api.BackupStatus {
	s, _ := backup.LoadSettings(config.Path())
	st, _ := backup.LoadState(d.Config().DataDir)
	where := ""
	if s.Recipient != "" {
		where = " (" + backup.Where(s) + ")"
	}
	h, detail, fix := backup.Health(st, backup.Settings{Recipient: s.Recipient, Where: where}, time.Now())
	out := api.BackupStatus{
		On: s.Recipient != "", Health: string(h), Detail: sentenceCase(detail), Fix: fix,
		LastGood: st.LastGood, LastAttempt: st.LastAttempt, LastError: st.LastError, Seq: st.Seq, Size: st.LastSize, LeftOut: st.LeftOut,
	}
	if sch := d.backupScheduler(); sch != nil {
		out.Running = sch.Running()
	}
	if st.Standby != nil {
		out.Standby = st.Standby.HostLabel
	}
	if out.On {
		out.Target, out.Where = s.Target, backup.Where(s)
		if out.Target == "" {
			out.Target = backup.TargetICloud
		}
		switch out.Target {
		case backup.TargetFolder:
			out.Path = s.Path
		case backup.TargetS3:
			out.S3URL = "s3://" + s.S3.Bucket
			if s.S3.Prefix != "" {
				out.S3URL += "/" + s.S3.Prefix
			}
			out.Endpoint, out.Region = s.S3.Endpoint, s.S3.Region
			a, sk := s.S3.KeyEnvs()
			out.KeySaved = config.Secret(a) != "" && config.Secret(sk) != ""
		}
		if ns, err := backup.NamespaceOf(s.RecoveryPub); err == nil {
			out.KitID = backup.KitIDFor(ns)
		}
	}
	if _, err := backup.ICloudDrivePath(); err == nil {
		out.ICloud = true
	} else if runtime.GOOS == "darwin" {
		out.ICloudNote = "iCloud Drive isn't turned on for this Mac. Turn it on in System Settings › your name › iCloud › iCloud Drive."
	} else {
		out.ICloudNote = "iCloud Drive is only on a Mac."
	}
	return out
}

// sentenceCase starts a health detail with a capital.
func sentenceCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// BackupCheck turns what the page chose into settings, checked before any
// words are made.
func (d *Daemon) BackupCheck(ctx context.Context, w api.BackupWhere) (config.Backup, error) {
	cur, _ := backup.LoadSettings(config.Path())
	where := config.Backup{Target: w.Target, Sessions: cur.Sessions}
	switch w.Target {
	case backup.TargetICloud:
		if _, err := backup.ICloudDrivePath(); err != nil {
			return where, &api.HumanError{Sentence: "iCloud Drive isn't available on this computer.", Fix: "Turn it on in System Settings › your name › iCloud › iCloud Drive, or choose a folder."}
		}
	case backup.TargetFolder:
		p := strings.TrimSpace(w.Path)
		if p == "" {
			return where, &api.HumanError{Sentence: "Type the folder to keep backups in.", Fix: "A USB disk or a network drive is best: somewhere other than this computer's own disk."}
		}
		p = expandUser(p)
		if !filepath.IsAbs(p) {
			return where, &api.HumanError{Sentence: "Type the whole path of the folder, starting with /.", Fix: "In Finder, hold Option and choose Copy as Pathname, then paste it here."}
		}
		where.Path = filepath.Clean(p)
		if err := backup.CheckWhere(config.Path(), d.Config().DataDir, where); err != nil {
			return where, err
		}
	case backup.TargetS3:
		bucket, prefix, err := backup.ParseS3URL(strings.TrimSpace(w.S3URL))
		if err != nil {
			return where, &api.HumanError{Sentence: sentenceCase(err.Error()) + ".", Fix: "Give the bucket as s3://bucket/folder."}
		}
		s3 := config.BackupS3{Endpoint: strings.TrimSpace(w.Endpoint), Region: strings.TrimSpace(w.Region), Bucket: bucket, Prefix: prefix}
		if cur.Target == backup.TargetS3 {
			s3.AccessKeyEnv, s3.SecretKeyEnv = cur.S3.AccessKeyEnv, cur.S3.SecretKeyEnv
		}
		u, err := backup.S3Endpoint(s3.Endpoint, s3.Region)
		if err != nil {
			return where, &api.HumanError{Sentence: sentenceCase(err.Error()) + ".", Fix: "Copy the endpoint from your storage provider's dashboard, or leave it empty for AWS."}
		}
		if s3.Endpoint != "" && !strings.HasSuffix(u.Hostname(), ".amazonaws.com") {
			s3.PathStyle = true // R2, B2, MinIO and Wasabi take the bucket in the path
		}
		where.S3 = s3
		access, secret := s3.KeyEnvs()
		ak, sk := strings.TrimSpace(w.AccessKey), strings.TrimSpace(w.SecretKey)
		switch {
		case ak != "" && sk != "":
			if err := config.SaveSecrets(map[string]string{access: ak, secret: sk}); err != nil {
				return where, err
			}
		case ak != "" || sk != "":
			return where, &api.HumanError{Sentence: "Type both halves of the bucket's key: the access key ID and the secret access key."}
		case config.Secret(access) == "" || config.Secret(secret) == "":
			return where, &api.HumanError{Sentence: "Type the bucket's access key ID and secret access key.", Fix: "Make a key that can read and write this bucket only, in your storage provider's dashboard."}
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := backup.CheckBucket(cctx, where); err != nil {
			return where, &api.HumanError{Sentence: "I can't use " + backup.Where(where) + ": " + err.Error() + ".", Fix: "Check the bucket's name, endpoint and key, then try again."}
		}
	default:
		return where, &api.HumanError{Sentence: "Choose where backups go: iCloud Drive, a folder or a bucket."}
	}
	return where, nil
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

// BackupSetup turns backups on with p's public keys and takes the first
// backup.
func (d *Daemon) BackupSetup(ctx context.Context, p backup.Phrase, where config.Backup) error {
	if _, err := backup.Setup(config.Path(), d.Config().DataDir, p, where, time.Now()); err != nil {
		return err
	}
	d.store.Audit(ctx, "backup.setup", "", backup.Where(where))
	_ = d.BackupNow(ctx)
	return nil
}

// BackupMove changes where backups go.
func (d *Daemon) BackupMove(ctx context.Context, where config.Backup) error {
	s, err := backup.SetTarget(config.Path(), where)
	if err != nil {
		return err
	}
	d.store.Audit(ctx, "backup.target", "", backup.Where(s))
	d.BackupSoon("backup moved")
	return nil
}

// BackupNow starts a backup now.
func (d *Daemon) BackupNow(context.Context) error {
	sch := d.backupScheduler()
	if sch == nil {
		return &api.HumanError{Sentence: "Backups start with " + d.Config().Name + "; it isn't running here.", Fix: "Run `mirrin backup now` in a terminal."}
	}
	switch err := sch.RunNow("owner asked"); {
	case errors.Is(err, backup.ErrBusy):
		return &api.HumanError{Sentence: "A backup is already under way.", Fix: "This page updates when it is done."}
	case err != nil:
		return &api.HumanError{Sentence: "Backups aren't running right now.", Fix: "Restart " + d.Config().Name + ", then try again."}
	}
	return nil
}

// RestoreReview says whether this machine was restored and its devices
// not yet reviewed.
func (d *Daemon) RestoreReview(context.Context) api.RestoreReview {
	st, _ := backup.LoadState(d.Config().DataDir)
	return api.RestoreReview{RestoredAt: st.RestoredAt, Pending: !st.RestoredAt.IsZero() && st.DevicesReviewed.Before(st.RestoredAt)}
}

// FinishRestoreReview records that the owner reviewed the devices.
func (d *Daemon) FinishRestoreReview(ctx context.Context) error {
	if err := backup.UpdateState(d.Config().DataDir, func(s *backup.State) { s.DevicesReviewed = time.Now() }); err != nil {
		return err
	}
	d.store.Audit(ctx, "restore.reviewed", "", "")
	return nil
}

// ---- Trust ----

// Trust marks what this twin sends where, and the certificate watch.
func (d *Daemon) Trust(ctx context.Context) api.TrustInfo {
	c := d.Config()
	out := api.OutboundCatalog()
	mark := func(id, where string) {
		for i := range out {
			if out[i].ID == id {
				out[i].On, out[i].Where = true, where
			}
		}
	}
	mark("model", modelWhere(c))
	if c.UI.Weather == nil || *c.UI.Weather {
		mark("weather", "api.open-meteo.com")
	}
	if d.google.Connected() {
		mark("google", strings.Join(googleFeatures(c), ", "))
	}
	var chans []string
	for _, k := range c.Connectors() {
		if k.Enabled && k.Name != "cli" && k.Name != "voice" && k.Name != "imessage" {
			chans = append(chans, k.Label)
		}
	}
	if len(chans) > 0 {
		mark("channels", strings.Join(chans, ", "))
	}
	mark("web", "")
	var voice []string
	if c.Channels.Voice.Engine == "elevenlabs" || c.Channels.Voice.ElevenLabsAPIKey != "" || config.Secret("ELEVENLABS_API_KEY") != "" {
		voice = append(voice, "ElevenLabs")
	}
	if c.Phone.Enabled {
		voice = append(voice, "Twilio")
	}
	if len(voice) > 0 {
		mark("voice", strings.Join(voice, " and "))
	}
	if s, _ := backup.LoadSettings(config.Path()); s.Recipient != "" {
		mark("backup", backup.Where(s))
	}
	if c.Push.IsEnabled() && len(pushSubscribers(d)) > 0 {
		mark("push", "")
	}
	switch c.Reach.Mode {
	case "tailscale":
		mark("tailscale", "")
	case "relay":
		mark("relay", hostOf(c.Reach.RelayURL))
		mark("certificates", c.Reach.Hostname)
	case "cloud":
		if e := d.cloudEndpoint(); e != nil {
			mark("certificates", e.Hostname)
		}
	}
	if cl, err := cloud.OpenLinked(c.DataDir); err == nil && cl != nil {
		mark("linked", hostOf(cl.API()))
	}
	info := api.TrustInfo{Outbound: out}
	if e := d.relayEndpoint(); e != nil && e.Watcher != nil {
		st := e.Watcher.Status()
		h, detail, fix := e.Watcher.Health(ctx)
		ct := &api.CTStatus{Checked: st.Checked, SourcesUp: st.SourcesUp, SourcesDown: st.SourcesDown, CAAProblem: st.CAAProblem, State: string(h), Detail: sentenceCase(detail), Fix: fix}
		if e.Alarm != nil {
			if a := e.Alarm.State(); a.Active {
				ct.Alarm = "Certificate alarm since " + a.Since.Local().Format("2 Jan 15:04") + ": " + a.Detail + "."
				ct.State = "fail"
			}
		}
		info.CT = ct
	}
	return info
}

// pushSubscribers lists the devices with notifications on.
func pushSubscribers(d *Daemon) []string {
	v, ok := pushDispatchers.Load(d)
	if !ok {
		return nil
	}
	var out []string
	for _, sub := range v.(*push.Dispatcher).Store.List() {
		out = append(out, sub.DeviceID)
	}
	return out
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// modelWhere names where model requests go.
func modelWhere(c config.Config) string {
	p := c.LLM.Provider
	base := c.ProviderBaseURL(p)
	if base == "" {
		base = c.LLM.BaseURL
	}
	names := map[string]string{"anthropic": "Anthropic", "openai": "OpenAI", "gemini": "Google Gemini", "ollama": "Ollama"}
	name := names[p]
	if name == "" {
		name = p
	}
	if h := hostOf(base); h != "" && base != "" {
		if strings.HasPrefix(h, "127.0.0.1") || strings.HasPrefix(h, "localhost") || strings.HasPrefix(h, "[::1]") {
			return name + " on this computer"
		}
		return name + " at " + h
	}
	if p == "ollama" {
		return "Ollama on this computer"
	}
	return name
}
