package reach

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/relay"
	"github.com/MavrkAI/Mirrin/internal/relay/wire"
	"github.com/MavrkAI/Mirrin/internal/tlsmgr"
)

// Cloud reach (docs/cloud-design.md §6, §10): the handle the entitlement
// names, carried by every relay the entitlement lists at once (dual-homed:
// either relay alone is enough), with TLS ending here under a key only this
// machine holds. The certificate comes from this machine's own ACME account
// by TLS-ALPN-01 through the relays, and the control plane is told that
// account so the handle's CAA record pins it: whoever controls a relay can
// steer traffic but can't get a certificate for the name.
//
// Which machine holds the handle is decided by (gen, iat): a relay that
// sees a higher generation for the name says superseded, and this machine
// asks the control plane, which has the last word. Confirmed, it stands by
// (paused, channels stopped) until the owner brings it back. The same
// generation with a newer entitlement (superseded_retry) only means a
// refresh is due: this machine refreshes and reconnects, and never gives up.

// CloudEnd is why a cloud endpoint stopped by itself.
type CloudEnd int

const (
	// CloudRunning: still running, or stopped by its context.
	CloudRunning CloudEnd = iota
	// CloudStandby: another machine holds the handle at a higher
	// generation. This one stands by.
	CloudStandby
	// CloudExpired: the entitlement expired, or the control plane revoked
	// this machine. The relays refuse it; free reach takes over.
	CloudExpired
	// CloudUnlinked: the link was removed (mirrin cloud unlink).
	CloudUnlinked
	// CloudRestart: a refreshed entitlement names other relays or names;
	// the endpoint starts again with them.
	CloudRestart
)

func (c CloudEnd) String() string {
	switch c {
	case CloudRunning:
		return "running"
	case CloudStandby:
		return "standby"
	case CloudExpired:
		return "expired"
	case CloudUnlinked:
		return "unlinked"
	case CloudRestart:
		return "restart"
	}
	return fmt.Sprintf("CloudEnd(%d)", int(c))
}

// CloudStop is StartCloud's answer when the link can't carry reach now.
type CloudStop struct {
	End CloudEnd
	At  time.Time // standby: when another machine took over
}

func (e *CloudStop) Error() string {
	switch e.End {
	case CloudStandby:
		return "another machine took over this address on " + e.At.Local().Format("2 Jan") + "; this one is standing by"
	case CloudExpired:
		return "the paid address has expired"
	case CloudUnlinked:
		return "this machine isn't linked"
	}
	return "the paid address needs starting again"
}

// Timing for the cloud endpoint.
const (
	// cloudMaxBackoff caps the wait between tunnel attempts, so a relay that
	// comes back carries traffic again within seconds.
	cloudMaxBackoff = 4 * time.Second
	// refreshGap is the least time between refreshes a relay's refusal
	// asks for, so two relays refusing at once make one request. It
	// doubles after each such refresh, up to maxRefreshGap, until both
	// tunnels are up again: a relay that keeps refusing (or a clock that
	// is off) can't make this machine ask the control plane over and over.
	refreshGap    = 20 * time.Second
	maxRefreshGap = time.Hour
	// confirmFirst and confirmMax bound the wait between attempts to ask
	// the control plane whether a relay's superseded is true, while it
	// can't be reached.
	confirmFirst = 2 * time.Second
	confirmMax   = 5 * time.Minute
	// pinSettle is the wait after a new ACME account is pinned, while the
	// handle's CAA record changes at the DNS provider.
	pinSettle = 90 * time.Second
)

// RefusalSentence is what the owner reads when a relay refuses this
// machine, for each code of antbot.tunnel.v1 (docs/relay-protocol.md
// §3.5). The relay's own message goes to the log.
func RefusalSentence(code string) string {
	switch code {
	case wire.CodeEntitlementExpired:
		return "The relays say this computer's pass for your address has run out. I'm fetching a new one and will reconnect."
	case wire.CodeBadSignature:
		return "The relays couldn't check this computer's signature. I'll keep trying; if it lasts, run `mirrin cloud status`."
	case wire.CodeHostnameNotAllowed:
		return "The relays won't carry your address for this computer. I'll keep trying; if it lasts, run `mirrin cloud status`."
	case wire.CodeDenied:
		return "Your address is suspended at the relays. I'll try again when they allow it."
	case wire.CodeSuperseded:
		return "Another computer has taken over your address, so this one is standing by."
	case wire.CodeSupersededRetry:
		return "A newer connection for your address came in. I'm refreshing and reconnecting."
	case wire.CodeRateLimited:
		return "The relays asked me to slow down. I'll reconnect in a moment."
	case wire.CodeUpgradeRequired:
		return "The relays need a newer Mirrin. Run `mirrin update`, then restart Mirrin."
	}
	return "The relays refused the connection. I'll keep trying."
}

// MovedLine is the menu's line while this machine stands by because another
// one took over the address.
func MovedLine(name string, at time.Time) string {
	return name + " moved to another machine on " + at.Local().Format("2 Jan")
}

// cloudEndpoint is the part of an Endpoint only cloud reach has.
type cloudEndpoint struct {
	client *cloud.Client
	state  *cloud.State
	d      Deps
	log    *slog.Logger
	status []byte // the status key

	mu          sync.Mutex
	claims      entitle.Claims
	kind        cloud.StateKind
	end         CloudEnd
	endAt       time.Time
	refusal     string // the owner's sentence for the last refusal
	lastRefresh time.Time
	gap         time.Duration // least time between refusal-driven refreshes now
	confirming  bool          // a superseded refusal is being confirmed
	refreshing  sync.Mutex    // one refresh at a time
}

// StartCloud starts reach through the relays the entitlement lists: a
// tunnel to each, the remote API over them, the handle's certificate
// (pinned by CAA to this machine's ACME account, which is sent to the
// control plane first), and the CT/CAA watch. st is the link state; nil is
// c's. It returns at once; ctx ends it, and so does the link changing
// (Ended says why). When the link can't carry reach now, it returns a
// *CloudStop.
func StartCloud(ctx context.Context, c *cloud.Client, st *cloud.State, d Deps) (*Endpoint, error) {
	if c == nil {
		return nil, &CloudStop{End: CloudUnlinked}
	}
	if st == nil {
		st = c.State()
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	cl, kind := st.Current(now())
	if stop := stopFor(st, kind); stop != nil {
		return nil, stop
	}
	if !cl.Has("reach") || len(cl.Hosts) == 0 || len(cl.Relays) == 0 {
		return nil, errors.New("this machine's link doesn't include reach from anywhere")
	}
	key, err := c.TunnelKey()
	if err != nil {
		return nil, err
	}
	status, err := StatusKey(d.DataDir)
	if err != nil {
		return nil, fmt.Errorf("status key: %w", err)
	}
	ce := &cloudEndpoint{client: c, state: st, d: d, log: log, status: status, claims: cl, kind: kind}
	var refs []relay.RelayRef
	var urls []string
	for _, r := range cl.Relays {
		refs = append(refs, relay.RelayRef{ID: r.ID, URL: r.URL})
		urls = append(urls, r.URL)
	}
	host := cl.Hosts[0]
	e, err := startTunnels(ctx, tunnelSpec{
		via:      "cloud",
		hosts:    cl.Hosts,
		relays:   refs,
		relayURL: strings.Join(urls, ", "),
		key:      key,
		status:   status,
		roots:    d.RelayRoots,
		cfg:      d.Reach,
		entitlement: func() string {
			tok, _ := st.Entitlement()
			return tok
		},
		maxBackoff: cloudMaxBackoff,
		onRefused:  ce.refused,
		prepare: func(ctx context.Context, e *Endpoint) error {
			uri, err := e.ACME.Register(ctx)
			if err != nil {
				return err
			}
			return ce.pin(ctx, uri)
		},
		ready: func(ctx context.Context, e *Endpoint) error {
			if uri := e.ACME.AccountURI(); uri != "" {
				return ce.pin(ctx, uri)
			}
			return nil
		},
		health: CloudHealth,
		expiring: func(left time.Duration) string {
			return fmt.Sprintf("My HTTPS certificate for %s runs out in %s and hasn't renewed. Run `mirrin reach verify` to see why.", host, roughDays(left))
		},
	}, d)
	if err != nil {
		return nil, err
	}
	e.cloud = ce
	go ce.watch(ctx, e)
	return e, nil
}

// stopFor is the stop a link state means, or nil when it can carry reach.
func stopFor(st *cloud.State, kind cloud.StateKind) *CloudStop {
	switch kind {
	case cloud.None:
		return &CloudStop{End: CloudUnlinked}
	case cloud.Expired:
		return &CloudStop{End: CloudExpired}
	case cloud.Superseded:
		stop := &CloudStop{End: CloudStandby}
		if info, ok, _ := st.Info(); ok && info.Superseded != nil {
			stop.At = info.Superseded.At
		}
		return stop
	}
	return nil
}

func (ce *cloudEndpoint) now() time.Time {
	if ce.d.Now != nil {
		return ce.d.Now()
	}
	return time.Now()
}

// finish stops e for good, for why.
func (ce *cloudEndpoint) finish(e *Endpoint, why CloudEnd, at time.Time) {
	ce.mu.Lock()
	if ce.end == CloudRunning {
		ce.end, ce.endAt = why, at
	}
	ce.mu.Unlock()
	e.stop()
}

// watch reads the link state until ctx ends, and stops the endpoint when it
// can no longer carry reach, or carries it through other relays or names.
func (ce *cloudEndpoint) watch(ctx context.Context, e *Endpoint) {
	every := ce.d.CloudCheck
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.Done():
			return
		case <-t.C:
		}
		cl, kind := ce.state.Current(ce.now())
		if stop := stopFor(ce.state, kind); stop != nil {
			ce.finish(e, stop.End, stop.At)
			return
		}
		ce.mu.Lock()
		if allOnline(e) {
			ce.gap = 0 // the relays take this machine again
		}
		moved := !slices.Equal(cl.Hosts, ce.claims.Hosts) || !slices.EqualFunc(cl.Relays, ce.claims.Relays, func(a, b entitle.Relay) bool { return a.ID == b.ID && a.URL == b.URL })
		ce.claims, ce.kind = cl, kind
		ce.mu.Unlock()
		if moved {
			ce.log.Info("cloud reach: the entitlement names other relays or names; starting again")
			ce.finish(e, CloudRestart, time.Time{})
			return
		}
	}
}

// allOnline reports whether every tunnel of e is up.
func allOnline(e *Endpoint) bool {
	if e.Listener == nil {
		return false
	}
	st := e.Listener.Status()
	for _, s := range st {
		if !s.Online {
			return false
		}
	}
	return len(st) > 0
}

// refused hears a relay's refusal, on that tunnel's goroutine, before it
// reconnects.
func (ce *cloudEndpoint) refused(e *Endpoint, relayID string, we wire.Error) {
	ce.mu.Lock()
	ce.refusal = RefusalSentence(we.Code)
	ce.mu.Unlock()
	ce.log.Warn("cloud reach: a relay refused this machine", "relay", relayID, "code", we.Code, "message", we.Message)
	switch we.Code {
	case wire.CodeSuperseded:
		// This tunnel has stopped. The control plane says whether another
		// machine really holds the handle now; a relay alone can't put this
		// one to sleep, and nor can the control plane's silence.
		ce.mu.Lock()
		busy := ce.confirming
		ce.confirming = true
		ce.mu.Unlock()
		if !busy {
			go ce.confirmSuperseded(e)
		}
	case wire.CodeSupersededRetry, wire.CodeEntitlementExpired:
		ctx, cancel := endpointContext(e, time.Minute)
		defer cancel()
		ce.refresh(ctx, e, false)
	}
}

// endpointContext is a context that ends after d or when e stops, so a
// request in flight is dropped once this machine stands by.
func endpointContext(e *Endpoint, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	go func() {
		select {
		case <-e.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// confirmSuperseded asks the control plane whether another machine holds
// the handle, until it answers: yes stands by; no starts the endpoint
// again, since the relay has stopped that tunnel for good. While it can't
// be reached, the question is asked again with growing waits, so a
// takeover is never left half done with two machines answering.
func (ce *cloudEndpoint) confirmSuperseded(e *Endpoint) {
	defer func() {
		ce.mu.Lock()
		ce.confirming = false
		ce.mu.Unlock()
	}()
	wait := confirmFirst
	if ce.d.ConfirmEvery > 0 {
		wait = ce.d.ConfirmEvery
	}
	for {
		ctx, cancel := endpointContext(e, time.Minute)
		ans := ce.refresh(ctx, e, true)
		cancel()
		switch ans {
		case refreshMoved, refreshStopped:
			return
		case refreshOurs:
			ce.log.Warn("cloud reach: a relay said another machine holds this handle, but the control plane still names this one; starting again")
			ce.finish(e, CloudRestart, time.Time{})
			return
		}
		select {
		case <-e.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, confirmMax)
	}
}

// refreshAnswer is what a refresh learned.
type refreshAnswer int

const (
	refreshUnknown refreshAnswer = iota // no answer: unreachable, 5xx, …
	refreshOurs                         // the control plane names this machine
	refreshMoved                        // another machine holds the handle
	refreshStopped                      // the endpoint has stopped, or no refresh was due
)

// refresh asks for a new entitlement, at most once per the current gap
// unless force, and stands by if the control plane says another machine
// took over.
func (ce *cloudEndpoint) refresh(ctx context.Context, e *Endpoint, force bool) refreshAnswer {
	ce.refreshing.Lock()
	defer ce.refreshing.Unlock()
	ce.mu.Lock()
	gap := ce.gap
	if gap <= 0 {
		gap = refreshGap
	}
	recent := !ce.lastRefresh.IsZero() && ce.now().Sub(ce.lastRefresh) < gap
	ended := ce.end != CloudRunning
	ce.mu.Unlock()
	select {
	case <-e.Done():
		ended = true
	default:
	}
	if ended || recent && !force {
		// Once standing by (or stopped), nothing more is sent: the other
		// relay's refusal of the same takeover needs no second answer.
		return refreshStopped
	}
	_, err := ce.client.Refresh(ctx)
	ce.mu.Lock()
	ce.lastRefresh = ce.now()
	if !force {
		ce.gap = min(gap*2, maxRefreshGap)
	}
	ce.mu.Unlock()
	var sup *cloud.SupersededError
	switch {
	case errors.As(err, &sup):
		ce.log.Warn("cloud reach: another machine took over this handle; standing by", "gen", sup.Gen, "at", sup.At)
		ce.finish(e, CloudStandby, sup.At)
		return refreshMoved
	case errors.Is(err, cloud.ErrRevoked):
		ce.log.Warn("cloud reach: the control plane revoked this machine")
		ce.finish(e, CloudExpired, time.Time{})
		return refreshStopped
	case err == nil, errors.Is(err, cloud.ErrLapsed):
		return refreshOurs
	}
	ce.log.Warn("cloud reach: refresh", "err", err)
	return refreshUnknown
}

// pin tells the control plane the ACME account the handle's CAA record must
// name, unless it was told this account for this handle already, and then
// waits for the record to change.
func (ce *cloudEndpoint) pin(ctx context.Context, uri string) error {
	ce.mu.Lock()
	handle, gen := ce.claims.Handle, ce.claims.Gen
	ce.mu.Unlock()
	rec := readCloudRecord(ce.d.DataDir)
	if rec.Handle == handle && rec.Gen == gen && rec.Account == uri {
		// Told already at this generation. Another generation (the handle
		// went to another machine and came back) may have pinned another
		// machine's account since, so it is told again.
		return nil
	}
	if err := ce.client.SetACMEAccount(ctx, uri); err != nil {
		return fmt.Errorf("tell the control plane this machine's ACME account: %w", err)
	}
	if err := NotePinned(ce.d.DataDir, handle, gen, uri); err != nil {
		return err
	}
	ce.log.Info("cloud reach: the handle's CAA record now names this machine's ACME account", "handle", handle, "account", uri)
	settle := ce.d.PinSettle
	if settle <= 0 {
		settle = pinSettle
	}
	if !pause(ctx, settle) {
		return ctx.Err()
	}
	return nil
}

// cloudRecord is data/reach/cloud.json: what this machine told the control
// plane, and whether the owner was told the address expired.
type cloudRecord struct {
	Handle   string    `json:"handle,omitempty"`
	Account  string    `json:"account,omitempty"`
	Gen      int64     `json:"gen,omitempty"` // the handle's generation then
	PinnedAt time.Time `json:"pinned_at,omitzero"`
	// ExpiredNoticed is the entitlement expiry the owner was told about,
	// so one expiry is one notice.
	ExpiredNoticed time.Time `json:"expired_noticed,omitzero"`
}

var cloudRecordMu sync.Mutex

func cloudRecordPath(dataDir string) string { return filepath.Join(dataDir, "reach", "cloud.json") }

func readCloudRecord(dataDir string) cloudRecord {
	var rec cloudRecord
	if b, err := os.ReadFile(cloudRecordPath(dataDir)); err == nil {
		_ = json.Unmarshal(b, &rec)
	}
	return rec
}

func updateCloudRecord(dataDir string, fn func(*cloudRecord)) error {
	cloudRecordMu.Lock()
	defer cloudRecordMu.Unlock()
	rec := readCloudRecord(dataDir)
	fn(&rec)
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	p := cloudRecordPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeNew(p, b)
}

// NotePinned records that the control plane was told account for handle
// at generation gen, at link or later, so it isn't told again while the
// handle stays at that generation.
func NotePinned(dataDir, handle string, gen int64, account string) error {
	return updateCloudRecord(dataDir, func(r *cloudRecord) {
		r.Handle, r.Gen, r.Account, r.PinnedAt = handle, gen, account, time.Now().UTC().Truncate(time.Second)
	})
}

// PinnedAccount is the ACME account the control plane was last told for
// the handle, or "".
func PinnedAccount(dataDir, handle string) string {
	rec := readCloudRecord(dataDir)
	if rec.Handle != handle {
		return ""
	}
	return rec.Account
}

// ExpiryNotice reports whether the owner should be told that the
// entitlement expiring at exp ended reach, and records that they were: one
// expiry, one notice.
func ExpiryNotice(dataDir string, exp time.Time) bool {
	due := false
	_ = updateCloudRecord(dataDir, func(r *cloudRecord) {
		if !r.ExpiredNoticed.Equal(exp.UTC()) {
			r.ExpiredNoticed, due = exp.UTC(), true
		}
	})
	return due
}

// LinkedAccount is the ACME account this machine already has at the
// configured CA, read without writing anything, for `mirrin cloud link` to
// send at link time. "" when there is none yet: the daemon sends it later.
func LinkedAccount(dataDir string, c config.Reach) string {
	want := c.ACMEDirectory
	if want == "" {
		want = tlsmgr.LetsEncrypt
	}
	st, err := tlsmgr.ReadACMEState(filepath.Join(dataDir, "tls"))
	if err != nil || st.Directory != want || cloud.CheckACMEAccount(st.AccountURI) != nil {
		return ""
	}
	return st.AccountURI
}

// Ended says why a cloud endpoint stopped by itself, and when another
// machine took over (standby only). CloudRunning for any other endpoint.
func (e *Endpoint) Ended() (CloudEnd, time.Time) {
	if e == nil || e.cloud == nil {
		return CloudRunning, time.Time{}
	}
	e.cloud.mu.Lock()
	defer e.cloud.mu.Unlock()
	return e.cloud.end, e.cloud.endAt
}

// Cloud reports whether e is cloud reach.
func (e *Endpoint) Cloud() bool { return e != nil && e.cloud != nil }

// CloudState is the link state the endpoint last read: Active or Grace
// while it runs, and the entitlement's claims.
func (e *Endpoint) CloudState() (entitle.Claims, cloud.StateKind) {
	if e == nil || e.cloud == nil {
		return entitle.Claims{}, cloud.None
	}
	e.cloud.mu.Lock()
	defer e.cloud.mu.Unlock()
	return e.cloud.claims, e.cloud.kind
}

// PublicURL is the address the internet reaches this twin at now, or ""
// while no tunnel is up with a certificate.
func (e *Endpoint) PublicURL() string {
	if e == nil || e.ACME == nil || e.ACME.Leaf() == nil || !e.Online() {
		return ""
	}
	return e.URL
}

// StatusURLs are each relay's status endpoint for the handle, with this
// twin's status key: a paired phone asks them whether the twin is online,
// or since when it has been asleep.
func (e *Endpoint) StatusURLs() []string {
	if e == nil || e.cloud == nil {
		return nil
	}
	claims, _ := e.CloudState()
	k := base64.RawURLEncoding.EncodeToString(e.cloud.status)
	var out []string
	for _, r := range claims.Relays {
		u, err := url.Parse(r.URL)
		if err != nil || u.Host == "" {
			continue
		}
		out = append(out, "https://"+u.Host+"/v1/status/"+url.PathEscape(claims.Handle)+"?k="+k)
	}
	return out
}

// Refusal is the owner's sentence for the last refusal a relay gave, or "".
func (e *Endpoint) Refusal() string {
	if e == nil || e.cloud == nil {
		return ""
	}
	e.cloud.mu.Lock()
	defer e.cloud.mu.Unlock()
	return e.cloud.refusal
}

// CloudHealth is the reach self-check for cloud reach.
func CloudHealth(e *Endpoint) (health.State, string, string) {
	claims, kind := e.CloudState()
	var up, all []string
	for _, s := range e.Listener.Status() {
		all = append(all, s.Relay)
		if s.Online {
			up = append(up, s.Relay)
		}
	}
	if len(up) == 0 {
		why := e.Refusal()
		if why == "" {
			why = "connecting"
		}
		return health.Warn, "not connected to the relays: " + why, "Run `mirrin reach verify`; Mirrin keeps retrying"
	}
	if err := tlsmgr.Warning(e.ACME); err != nil {
		return health.Warn, err.Error(), "Run `mirrin reach verify`; Mirrin keeps retrying"
	}
	if e.ACME.Leaf() == nil {
		return health.Warn, "connected; getting a certificate for " + e.Hostname, "Mirrin is asking the certificate authority now"
	}
	if kind == cloud.Grace {
		return health.Warn, "payment lapsed; " + e.Hostname + " keeps working until " + claims.Exp.Local().Format("2 Jan"), "Renew in the billing page (`mirrin cloud billing`), or nothing changes until then"
	}
	msg := "reachable at " + e.URL + " through " + strings.Join(up, " and ")
	if len(up) < len(all) {
		msg += " (the other relay is reconnecting)"
	}
	return health.OK, msg, ""
}

// CloudStatus says whether a cloud endpoint can take a phone now:
// connected, with a certificate. When it can't, detail says why.
func CloudStatus(e *Endpoint) (ready bool, detail string) {
	if e == nil || e.cloud == nil || e.Listener == nil {
		return false, "not started"
	}
	if !e.Online() {
		if r := e.Refusal(); r != "" {
			return false, strings.TrimSuffix(r, ".")
		}
		return false, "connecting to the relays"
	}
	if e.ACME == nil || e.ACME.Leaf() == nil {
		return false, "getting a certificate for " + e.Hostname
	}
	return true, ""
}

// LocalCloudEndpoint is what the CLI knows about this machine's cloud
// reach, read from the link state and data/tls without changing anything,
// for `mirrin reach verify`.
func LocalCloudEndpoint(st *cloud.State, dataDir string) (*Endpoint, error) {
	cl, kind := st.Current(time.Now())
	if kind == cloud.None {
		return nil, errors.New("this machine isn't linked; run `mirrin reach use cloud`")
	}
	if len(cl.Hosts) == 0 {
		return nil, errors.New("the stored entitlement doesn't verify in this build; run `mirrin cloud status`")
	}
	ts, err := tlsmgr.ReadACMEState(filepath.Join(dataDir, "tls"))
	if err != nil {
		return nil, fmt.Errorf("this machine has no HTTPS keys yet; start Mirrin with reach set to cloud first (%w)", err)
	}
	var urls []string
	for _, r := range cl.Relays {
		urls = append(urls, r.URL)
	}
	pins := []string{ts.Current, ts.Next}
	return &Endpoint{
		Hostname: cl.Hosts[0], URL: "https://" + cl.Hosts[0], RelayURL: strings.Join(urls, ", "),
		Pins:       func() []string { return pins },
		AccountURI: func() string { return ts.AccountURI },
	}, nil
}
