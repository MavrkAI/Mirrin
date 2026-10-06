package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// The watcher is the only thing that pages, and it pages for exactly one
// condition (docs/cloud-design.md §15): the canary fails end-to-end HTTPS
// through every relay, as seen from at least two regions, for three
// consecutive rounds. A region whose probe can't be read, or whose latest
// round is stale or comes from another region, counts as neither failing
// nor passing, so the watcher's own network trouble can't page. A round
// the watcher already counted is not counted again: the three consecutive
// rounds are three distinct probe rounds. Everything else is an email: one relay
// down (daemons are dual-homed), a silent probe, the control plane, the
// deny-list mirror, the canary's certificate running short, and any other
// check listed.

// alarm is a condition with hysteresis: raise consecutive bad rounds raise
// it, clear consecutive good rounds clear it.
type alarm struct {
	raise, clear int
	bad, good    int
	on           bool
}

// observe feeds one round and reports whether the alarm went on or off.
func (a *alarm) observe(bad bool) (raised, cleared bool) {
	if bad {
		a.bad++
		a.good = 0
		if !a.on && a.bad >= a.raise {
			a.on = true
			return true, false
		}
		return false, false
	}
	a.good++
	a.bad = 0
	if a.on && a.good >= a.clear {
		a.on = false
		return false, true
	}
	return false, false
}

// Event is an alarm going on or off.
type Event struct {
	Key     string `json:"key"`
	Page    bool   `json:"page"` // the one paging alarm
	Raised  bool   `json:"raised"`
	Summary string `json:"summary"`

	paged bool // the pager has it; only the email copy is left
}

// pageKey is the one paging alarm's key, and its dedup key at the pager.
const pageKey = "mirrin-canary-all-relays"

// view is what one round of the watcher saw.
type view struct {
	now time.Time
	// rounds holds each region's latest fresh round; a region missing
	// from it is unknown.
	rounds map[string]Round
	// repeat marks the regions whose round is the one the previous view
	// already had: a stalled probe's last round must not count twice
	// towards the paging rule.
	repeat map[string]bool
	// checks holds each check's error; nil passed.
	checks map[string]error
}

// evaluator turns views into events. It is pure, so the paging rule is
// tested round by round.
type evaluator struct {
	cfg    WatchConfig
	page   alarm
	relay  map[string]*alarm
	silent map[string]*alarm
	check  map[string]*alarm
	cert   alarm
}

func newEvaluator(cfg WatchConfig) *evaluator {
	e := &evaluator{
		cfg:    cfg,
		page:   alarm{raise: cfg.PageAfter, clear: cfg.ClearAfter},
		relay:  map[string]*alarm{},
		silent: map[string]*alarm{},
		check:  map[string]*alarm{},
		cert:   alarm{raise: 1, clear: 1},
	}
	for _, id := range cfg.Relays {
		e.relay[id] = &alarm{raise: cfg.RelayAfter, clear: 2}
	}
	for _, r := range cfg.Regions {
		e.silent[r.Name] = &alarm{raise: cfg.SilentAfter, clear: 1}
	}
	for _, c := range cfg.Checks {
		e.check[c.Name] = &alarm{raise: c.After, clear: 2}
	}
	return e
}

func (e *evaluator) step(v view) []Event {
	var out []Event
	emit := func(key string, page, raised, cleared bool, on, off string) {
		switch {
		case raised:
			out = append(out, Event{Key: key, Page: page, Raised: true, Summary: on})
		case cleared:
			out = append(out, Event{Key: key, Page: page, Summary: off})
		}
	}

	// The paging rule. A bad view needs PageRegions regions whose new
	// round has every relay failing. When only repeated rounds make up
	// the number, the view says nothing new: the alarm neither advances
	// nor resets.
	var allDown []string
	fresh := 0
	for _, r := range e.cfg.Regions {
		rd, ok := v.rounds[r.Name]
		if !ok {
			continue
		}
		down := true
		for _, id := range e.cfg.Relays {
			down = down && !rd.ok(id)
		}
		if down {
			allDown = append(allDown, r.Name)
			if !v.repeat[r.Name] {
				fresh++
			}
		}
	}
	bad := fresh >= e.cfg.PageRegions
	var raised, cleared bool
	if hold := !bad && len(allDown) >= e.cfg.PageRegions; !hold {
		raised, cleared = e.page.observe(bad)
	}
	emit(pageKey, true, raised, cleared,
		fmt.Sprintf("The canary is unreachable through every relay (%s) from %s for %d minutes: paid users can't reach their twins.", strings.Join(e.cfg.Relays, ", "), strings.Join(allDown, ", "), e.cfg.PageAfter),
		"The canary answers through the relays again.")

	// One relay at a time: email.
	for _, id := range e.cfg.Relays {
		var from []string
		for _, r := range e.cfg.Regions {
			if rd, ok := v.rounds[r.Name]; ok && !rd.ok(id) {
				from = append(from, r.Name)
			}
		}
		raised, cleared := e.relay[id].observe(len(from) >= e.cfg.PageRegions)
		emit("relay-"+id, false, raised, cleared,
			fmt.Sprintf("Relay %s isn't carrying the canary from %s. Daemons are using the other relay.", id, strings.Join(from, ", ")),
			fmt.Sprintf("Relay %s carries the canary again.", id))
	}

	// Probes that went quiet.
	for _, r := range e.cfg.Regions {
		_, ok := v.rounds[r.Name]
		raised, cleared := e.silent[r.Name].observe(!ok)
		emit("probe-"+r.Name, false, raised, cleared,
			fmt.Sprintf("The %s probe has sent no fresh round for %d minutes; it can't count towards a page.", r.Name, e.cfg.SilentAfter),
			fmt.Sprintf("The %s probe is reporting again.", r.Name))
	}

	// The canary's certificate, the earliest expiry any probe saw.
	var notAfter time.Time
	for _, rd := range v.rounds {
		for _, x := range rd.Relays {
			if !x.NotAfter.IsZero() && (notAfter.IsZero() || x.NotAfter.Before(notAfter)) {
				notAfter = x.NotAfter
			}
		}
	}
	if !notAfter.IsZero() {
		left := notAfter.Sub(v.now)
		raised, cleared := e.cert.observe(left < e.cfg.CertWarn)
		emit("cert", false, raised, cleared,
			fmt.Sprintf("The canary's certificate has %d days left and hasn't renewed. Twins renew the same way: check Let's Encrypt and the relays.", int(left.Hours()/24)),
			"The canary's certificate renewed.")
	}

	for _, c := range e.cfg.Checks {
		err, ran := v.checks[c.Name]
		if !ran {
			continue
		}
		detail := ""
		if err != nil {
			detail = ": " + err.Error()
		}
		raised, cleared := e.check[c.Name].observe(err != nil)
		emit("check-"+c.Name, false, raised, cleared,
			fmt.Sprintf("%s has failed for %d rounds%s", c.Name, c.After, detail),
			fmt.Sprintf("%s passes again.", c.Name))
	}
	return out
}

// watcher runs the evaluator against the probes and checks each round,
// and delivers what it says.
type watcher struct {
	cfg    *Config
	eval   *evaluator
	notify *notifier
	log    *slog.Logger
	http   *http.Client
	token  string
	now    func() time.Time

	mu       sync.Mutex
	last     time.Time // the last round that ran
	pending  []Event   // not delivered yet; retried each round
	lastView map[string]Round
	lastAt   map[string]time.Time // each region's round in the previous view
	pages    int                  // pages delivered, for /v1/status
}

func newWatcher(cfg *Config, n *notifier, log *slog.Logger) (*watcher, error) {
	w := &watcher{cfg: cfg, eval: newEvaluator(cfg.Watch), notify: n, log: log, http: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
	if cfg.Watch.TokenEnv != "" {
		if w.token = os.Getenv(cfg.Watch.TokenEnv); w.token == "" {
			return nil, fmt.Errorf("watch.token_env: %s is empty", cfg.Watch.TokenEnv)
		}
	}
	return w, nil
}

// fetch reads one probe's latest round, if it is fresh and from region r:
// two regions pointed at one probe by mistake must not count as two.
func (w *watcher) fetch(ctx context.Context, r Region, now time.Time) (Round, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return Round{}, false
	}
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	res, err := w.http.Do(req)
	if err != nil {
		w.log.Warn("watch: probe unreachable", "region", r.Name, "err", err)
		return Round{}, false
	}
	defer res.Body.Close()
	var rs Results
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&rs) != nil || len(rs.Rounds) == 0 {
		return Round{}, false
	}
	last := rs.Rounds[len(rs.Rounds)-1]
	if rs.Region != r.Name || last.Region != r.Name {
		w.log.Warn("watch: probe answers for another region", "region", r.Name, "answered", rs.Region)
		return Round{}, false
	}
	// Fresh: from the last two probe intervals, with some slack.
	if now.Sub(last.At) > 2*w.cfg.Probe.Every+15*time.Second || last.At.After(now.Add(time.Minute)) {
		return Round{}, false
	}
	return last, true
}

// runCheck runs one email-only check.
func (w *watcher) runCheck(ctx context.Context, c Check, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mirrin-canary/"+version)
	res, err := w.http.Do(req)
	if err != nil {
		return shortErr(err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	if c.Kind != "denylist" {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, entitle.MaxDenyListSize+2))
	if err != nil {
		return err
	}
	keys, err := w.cfg.dlKeys()
	if err != nil {
		return err
	}
	d, err := entitle.VerifyDenyList(strings.TrimSpace(string(b)), keys, now)
	if err != nil {
		return fmt.Errorf("the deny list doesn't verify: %w", err)
	}
	if age := now.Sub(d.Iat); age > c.MaxAge {
		return fmt.Errorf("the deny list is %s old (seq %d)", age.Round(time.Minute), d.Seq)
	}
	return nil
}

// tick runs one round: read every probe and check, evaluate, deliver.
func (w *watcher) tick(ctx context.Context) {
	now := w.now()
	v := view{now: now, rounds: map[string]Round{}, repeat: map[string]bool{}, checks: map[string]error{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, r := range w.cfg.Watch.Regions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rd, ok := w.fetch(ctx, r, now); ok {
				mu.Lock()
				v.rounds[r.Name] = rd
				mu.Unlock()
			}
		}()
	}
	for _, c := range w.cfg.Watch.Checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := w.runCheck(ctx, c, now)
			mu.Lock()
			v.checks[c.Name] = err
			mu.Unlock()
		}()
	}
	wg.Wait()
	w.mu.Lock()
	at := map[string]time.Time{}
	for name, rd := range v.rounds {
		at[name] = rd.At
		if prev, ok := w.lastAt[name]; ok && prev.Equal(rd.At) {
			v.repeat[name] = true
		}
	}
	w.lastAt = at
	events := w.eval.step(v)
	w.pending = append(w.pending, events...)
	pending := w.pending
	w.pending = nil
	w.last, w.lastView = now, v.rounds
	w.mu.Unlock()

	var retry []Event
	for i := range pending {
		e := &pending[i]
		if err := w.deliver(ctx, e); err != nil {
			w.log.Error("watch: delivery failed; retrying next round", "key", e.Key, "err", err)
			retry = append(retry, *e)
		}
	}
	w.mu.Lock()
	w.pending = append(retry, w.pending...)
	w.pending = trimPending(w.pending)
	w.mu.Unlock()
}

// trimPending bounds the retry queue while email is down for hours: it
// keeps the newest 50 events, and the page events among the older ones.
func trimPending(p []Event) []Event {
	if len(p) <= 100 {
		return p
	}
	cut := len(p) - 50
	keep := slices.DeleteFunc(slices.Clone(p[:cut]), func(e Event) bool { return !e.Page })
	return append(keep, p[cut:]...)
}

// deliver sends one event: a page to the pager (and a copy by email),
// everything else by email. A page that reached the pager is not sent
// there again when only its email copy failed.
func (w *watcher) deliver(ctx context.Context, e *Event) error {
	if e.Page && !e.paged {
		if err := w.notify.page(ctx, *e); err != nil {
			return err
		}
		e.paged = true
		if e.Raised {
			w.mu.Lock()
			w.pages++
			w.mu.Unlock()
		}
		w.log.Warn("watch: paged", "raised", e.Raised, "summary", e.Summary)
	}
	return w.notify.email(ctx, *e)
}

func (w *watcher) run(ctx context.Context) {
	tick := time.NewTicker(w.cfg.Watch.Every)
	defer tick.Stop()
	for {
		w.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// watchStatus is /v1/status.
type watchStatus struct {
	Last    time.Time        `json:"last"`
	Paging  bool             `json:"paging"`
	Pages   int              `json:"pages"`
	Alarms  []string         `json:"alarms"`
	Regions map[string]Round `json:"regions"`
	Pending int              `json:"pending"`
}

func (w *watcher) handler() http.Handler {
	mux := http.NewServeMux()
	// /healthz is for an external uptime check (email only): the watcher
	// is alive and has run a round lately.
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.Lock()
		last := w.last
		w.mu.Unlock()
		if w.now().Sub(last) > 3*w.cfg.Watch.Every {
			http.Error(rw, "no round lately", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(rw, "ok\n")
	})
	mux.HandleFunc("GET /v1/status", func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.Lock()
		s := watchStatus{Last: w.last, Paging: w.eval.page.on, Pages: w.pages, Regions: w.lastView, Pending: len(w.pending)}
		for k, a := range w.eval.relay {
			if a.on {
				s.Alarms = append(s.Alarms, "relay-"+k)
			}
		}
		for k, a := range w.eval.silent {
			if a.on {
				s.Alarms = append(s.Alarms, "probe-"+k)
			}
		}
		for k, a := range w.eval.check {
			if a.on {
				s.Alarms = append(s.Alarms, "check-"+k)
			}
		}
		if w.eval.cert.on {
			s.Alarms = append(s.Alarms, "cert")
		}
		w.mu.Unlock()
		slices.Sort(s.Alarms)
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(s)
	})
	return mux
}
