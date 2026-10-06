// Package certwatch watches Certificate Transparency and CAA for the names
// this machine answers to (docs/cloud-design.md §6.5). A certificate whose
// key this machine never held, issued after its checkpoint, means someone
// else can impersonate it; a CAA record that lets another ACME account
// issue means someone changed the name's DNS. Either is a critical finding,
// which the reach alarm playbook acts on.
//
// Only CT aggregators and DNS-over-HTTPS resolvers are contacted, and only
// with the public hostname: nothing about the owner leaves the machine.
package certwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/health"
)

// Severity of a finding.
type Severity string

const (
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Kind of a finding.
type Kind string

const (
	UnknownIssuance Kind = "unknown_issuance" // a certificate for a key this machine never held
	CAAMismatch     Kind = "caa_mismatch"     // CAA lets another account issue
	CAAWarning      Kind = "caa_warning"      // CAA missing, or it no longer allows tls-alpn-01
	SourceDown      Kind = "source_down"      // a CT source or every DoH resolver failed
	Superseded      Kind = "superseded"       // another machine took the name (the relay said so)
)

// Finding is something the owner, or the playbook, should know.
type Finding struct {
	Kind     Kind      `json:"kind"`
	Severity Severity  `json:"severity"`
	Host     string    `json:"host"`
	Detail   string    `json:"detail"`
	Issuance *Issuance `json:"issuance,omitempty"`
	CAA      []CAA     `json:"caa,omitempty"`
	// NotBefore starts the window the playbook treats as compromised: a
	// rogue certificate's notBefore, or the last time CAA was seen right.
	NotBefore time.Time `json:"not_before"`
	At        time.Time `json:"at"`
}

// Key identifies a finding across polls and restarts: the same rogue key or
// the same bad CAA set is one alarm, however often it is seen.
func (f Finding) Key() string {
	switch {
	case f.Issuance != nil:
		return string(f.Kind) + ":" + f.Host + ":" + f.Issuance.SPKI
	case len(f.CAA) > 0:
		var recs []string
		for _, c := range f.CAA {
			recs = append(recs, c.String())
		}
		slices.Sort(recs)
		return string(f.Kind) + ":" + f.Host + ":" + strings.Join(recs, "|")
	default:
		return string(f.Kind) + ":" + f.Host + ":" + f.Detail
	}
}

// Watcher polls Sources and CAA for every host.
type Watcher struct {
	// Hosts are the names to watch.
	Hosts func() []string
	// Known returns every SPKI this machine has used or announced
	// (current, next, restored history) and its checkpoint: issuances not
	// after it are ignored.
	Known func() ([]string, time.Time)
	// Sources are the CT aggregators. Every one is asked each poll.
	Sources []CTSource
	// Every is the poll interval. Zero is 6 hours.
	Every time.Duration
	// OnFinding hears each new finding once.
	OnFinding func(Finding)

	// AccountURI is this machine's ACME account. Nil or "" skips CAA.
	AccountURI func() string
	// CAA looks a name's CAA set up. Nil is LookupCAA over DefaultDoH.
	CAA func(ctx context.Context, name string) ([]CAA, error)
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
	// Log hears warnings. Nil discards them.
	Log *slog.Logger
	// Acknowledged reports whether the owner has already dealt with a
	// finding (by its Key): the alarm it raised was cleared. Such a
	// certificate stays in the logs for its whole life, so it is counted
	// apart and the health line warns instead of failing. Nil is none.
	Acknowledged func(key string) bool
	// StateFile keeps, across restarts, when each name's CAA was last seen
	// correct: a mismatch's window starts there rather than at the
	// checkpoint. "" keeps it in memory only.
	StateFile string

	mu       sync.Mutex
	reported map[string]bool
	poke     chan struct{}
	last     Status
	caaGood  map[string]time.Time
}

// Status is the last poll's outcome.
type Status struct {
	Checked     time.Time
	SourcesDown []string // CT sources that failed
	SourcesUp   int
	CAADown     bool
	CAAProblem  string
	Critical    int // critical findings seen in the last poll, not yet dealt with
	Cleared     int // critical findings the owner has already dealt with
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) init() {
	if w.reported == nil {
		w.reported = map[string]bool{}
		w.caaGood = map[string]time.Time{}
		w.poke = make(chan struct{}, 1)
		w.loadState()
	}
}

// watchState is what StateFile holds.
type watchState struct {
	CAAGood map[string]time.Time `json:"caa_good"`
}

// loadState reads StateFile. Callers hold mu.
func (w *Watcher) loadState() {
	if w.StateFile == "" {
		return
	}
	b, err := os.ReadFile(w.StateFile)
	if err != nil {
		return
	}
	var st watchState
	if json.Unmarshal(b, &st) == nil {
		maps.Copy(w.caaGood, st.CAAGood)
	}
}

// saveState writes StateFile. Callers hold mu.
func (w *Watcher) saveState() {
	if w.StateFile == "" {
		return
	}
	b, err := json.Marshal(watchState{CAAGood: w.caaGood})
	if err == nil {
		err = os.MkdirAll(filepath.Dir(w.StateFile), 0o700)
	}
	tmp := w.StateFile + ".tmp"
	if err == nil {
		err = os.WriteFile(tmp, b, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, w.StateFile)
	}
	if err != nil {
		w.logf("certificate watch: save state", "err", err)
	}
}

// Poke asks for a poll now (after an issuance). It never blocks.
func (w *Watcher) Poke() {
	w.mu.Lock()
	w.init()
	p := w.poke
	w.mu.Unlock()
	select {
	case p <- struct{}{}:
	default:
	}
}

// Run polls at once, then every Every and after each Poke, until ctx ends.
func (w *Watcher) Run(ctx context.Context) error {
	every := w.Every
	if every <= 0 {
		every = 6 * time.Hour
	}
	w.mu.Lock()
	w.init()
	poke := w.poke
	w.mu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.Check(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-poke:
		}
	}
}

// Check runs one poll and returns the findings that are new.
func (w *Watcher) Check(ctx context.Context) []Finding {
	w.mu.Lock()
	w.init()
	w.mu.Unlock()
	var hosts []string
	if w.Hosts != nil {
		hosts = w.Hosts()
	}
	var known []string
	var checkpoint time.Time
	if w.Known != nil {
		known, checkpoint = w.Known()
	}
	st := Status{Checked: w.now()}
	var found []Finding
	down := map[string]bool{}
	for _, src := range w.Sources {
		name := sourceName(src)
		for _, h := range hosts {
			list, err := src.Issuances(ctx, h)
			if err != nil {
				down[name] = true
				w.logf("CT source failed", "source", name, "err", err)
				continue
			}
			for _, is := range list {
				if !Covers(is.DNSNames, h) || !is.NotBefore.After(checkpoint) || slices.Contains(known, is.SPKI) {
					continue
				}
				is := is
				found = append(found, Finding{Kind: UnknownIssuance, Severity: Critical, Host: h, Issuance: &is, NotBefore: is.NotBefore, At: st.Checked,
					Detail: fmt.Sprintf("A certificate for %s was issued to a key this computer never had (%s, logged by %s, valid from %s).", h, issuerOr(is.Issuer), is.Source, is.NotBefore.UTC().Format("2 Jan 2006 15:04 MST"))})
			}
		}
	}
	for _, src := range w.Sources {
		if n := sourceName(src); down[n] {
			st.SourcesDown = append(st.SourcesDown, n)
		} else {
			st.SourcesUp++
		}
	}
	if len(w.Sources) > 0 && len(st.SourcesDown) > 0 {
		sev, detail := Warning, "Couldn't read "+strings.Join(st.SourcesDown, " and ")+"; the other certificate log still covers this name."
		if st.SourcesUp == 0 {
			detail = "Couldn't read any certificate log (" + strings.Join(st.SourcesDown, ", ") + "), so a stray certificate would go unnoticed for now."
		}
		for _, h := range hosts {
			found = append(found, Finding{Kind: SourceDown, Severity: sev, Host: h, Detail: detail, At: st.Checked})
		}
	}
	if acct := w.account(); acct != "" {
		for _, h := range hosts {
			found = append(found, w.checkCAA(ctx, h, acct, checkpoint, &st)...)
		}
	}
	var fresh []Finding
	w.mu.Lock()
	for _, f := range found {
		if f.Severity == Critical {
			if w.Acknowledged != nil && w.Acknowledged(f.Key()) {
				st.Cleared++
			} else {
				st.Critical++
			}
		}
		// Warnings about outages repeat every poll in the status; only
		// first sightings of anything are reported.
		k := f.Key()
		if f.Kind == SourceDown {
			k += st.Checked.Truncate(24 * time.Hour).String()
		}
		if w.reported[k] {
			continue
		}
		w.reported[k] = true
		fresh = append(fresh, f)
	}
	w.last = st
	w.mu.Unlock()
	for _, f := range fresh {
		if f.Severity == Critical {
			w.logf("certificate watch: critical finding", "kind", f.Kind, "host", f.Host, "detail", f.Detail)
		}
		if w.OnFinding != nil {
			w.OnFinding(f)
		}
	}
	return fresh
}

func (w *Watcher) account() string {
	if w.AccountURI == nil {
		return ""
	}
	return w.AccountURI()
}

func (w *Watcher) checkCAA(ctx context.Context, host, acct string, checkpoint time.Time, st *Status) []Finding {
	lookup := w.CAA
	if lookup == nil {
		lookup = func(ctx context.Context, name string) ([]CAA, error) { return LookupCAA(ctx, name, nil) }
	}
	recs, err := lookup(ctx, host)
	if err != nil {
		st.CAADown = true
		w.logf("CAA lookup failed", "host", host, "err", err)
		return []Finding{{Kind: SourceDown, Severity: Warning, Host: host, Detail: "Couldn't look up the CAA records for " + host + ".", At: st.Checked}}
	}
	v := CheckCAA(recs, acct)
	w.mu.Lock()
	good, seen := w.caaGood[host]
	if v.OK {
		w.caaGood[host] = st.Checked
		w.saveState()
	}
	w.mu.Unlock()
	switch {
	case v.OK:
		return nil
	case v.Critical:
		st.CAAProblem = v.Problem
		since := checkpoint
		if seen {
			since = good
		}
		return []Finding{{Kind: CAAMismatch, Severity: Critical, Host: host, Detail: v.Problem, CAA: recs, NotBefore: since, At: st.Checked}}
	default:
		st.CAAProblem = v.Problem
		return []Finding{{Kind: CAAWarning, Severity: Warning, Host: host, Detail: v.Problem, CAA: recs, At: st.Checked}}
	}
}

func issuerOr(s string) string {
	if s == "" {
		return "unknown issuer"
	}
	return s
}

func (w *Watcher) logf(msg string, args ...any) {
	if w.Log != nil {
		w.Log.Warn(msg, args...)
	}
}

// Status is the last poll's outcome.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// Health is the watch's line on the health page. It never passes when no
// log could be read: silence is not safety.
func (w *Watcher) Health(context.Context) (health.State, string, string) {
	st := w.Status()
	switch {
	case st.Checked.IsZero():
		return health.Warn, "certificate logs not checked yet", "Mirrin checks them every few hours"
	case len(w.Sources) > 0 && st.SourcesUp == 0:
		return health.Warn, "couldn't read any certificate log", "Check this computer's internet connection; Mirrin retries on its own"
	case st.Critical > 0:
		return health.Fail, "a certificate or CAA record isn't this computer's", "Open the Reach page on this computer"
	case st.CAAProblem != "":
		return health.Warn, st.CAAProblem, "Run `mirrin reach use relay` again to see the CAA records to publish"
	case st.Cleared > 0:
		return health.Warn, "a stray certificate you've dealt with is still in the logs", "Nothing to do: it stays listed until it expires"
	}
	detail := "no stray certificates"
	if len(st.SourcesDown) > 0 {
		detail += "; " + strings.Join(st.SourcesDown, " and ") + " didn't answer"
	}
	if st.CAADown {
		detail += "; CAA lookup failed"
	}
	return health.OK, detail, ""
}
