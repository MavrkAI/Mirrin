// Package health is Mirrin's self-check: are the ears, the voice, the model and
// the integrations actually working? It runs at startup and on a schedule,
// shows in the menu bar and on /health, and fixes what it can.
package health

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// State of one check.
type State string

const (
	OK   State = "ok"
	Warn State = "warn"
	Fail State = "fail"
	Off  State = "off" // not configured, not a problem
)

// Result of one check.
type Result struct {
	Name    string        `json:"name"`
	Label   string        `json:"label"`
	State   State         `json:"state"`
	Detail  string        `json:"detail"`
	Fix     string        `json:"fix,omitempty"`
	Took    time.Duration `json:"took"`
	Checked time.Time     `json:"checked"`
	// Fixed is set when the auto-repair for this check ran during this round.
	Fixed bool `json:"fixed,omitempty"`
}

// Check is one probe. Repair, if set, is attempted when the check fails.
type Check struct {
	Name   string
	Label  string
	Run    func(ctx context.Context) Result
	Repair func(ctx context.Context) error
}

// Report is the outcome of a round.
type Report struct {
	Results []Result  `json:"results"`
	At      time.Time `json:"at"`
}

// Summary is a one-line human status.
func (r Report) Summary() string {
	fails, warns := 0, 0
	for _, x := range r.Results {
		switch x.State {
		case Fail:
			fails++
		case Warn:
			warns++
		}
	}
	switch {
	case fails > 0:
		return fmt.Sprintf("%d problem(s)", fails)
	case warns > 0:
		return fmt.Sprintf("%d warning(s)", warns)
	default:
		return "all good"
	}
}

// Problems lists failing and warning results.
func (r Report) Problems() []Result {
	var out []Result
	for _, x := range r.Results {
		if x.State == Fail || x.State == Warn {
			out = append(out, x)
		}
	}
	return out
}

// Monitor runs checks on a schedule and keeps the latest report.
type Monitor struct {
	mu       sync.RWMutex
	runMu    sync.Mutex // one round at a time, so the latest round's report is the one kept
	checks   []Check
	last     Report
	OnChange func(prev, cur Report) // called when a check's state changes
}

// New builds a monitor.
func New(checks ...Check) *Monitor { return &Monitor{checks: checks} }

// Add registers more checks.
func (m *Monitor) Add(checks ...Check) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checks = append(m.checks, checks...)
}

// Last returns the latest report.
func (m *Monitor) Last() Report {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.last
}

// Run executes every check once (in parallel), attempting repairs on failures,
// and stores the report.
func (m *Monitor) Run(ctx context.Context) Report {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	m.mu.RLock()
	checks := append([]Check(nil), m.checks...)
	prev := m.last
	m.mu.RUnlock()
	return m.keep(prev, runChecks(ctx, checks))
}

// Recheck runs only the named checks again and keeps the rest of the latest
// report as it is, so a change to one part (the local API's port coming
// free) doesn't probe everything else (a paid model call). Before any full
// round, or with no names, it is Run.
func (m *Monitor) Recheck(ctx context.Context, names ...string) Report {
	m.mu.RLock()
	empty := len(m.last.Results) == 0
	m.mu.RUnlock()
	if empty || len(names) == 0 {
		return m.Run(ctx)
	}
	m.runMu.Lock()
	defer m.runMu.Unlock()
	m.mu.RLock()
	var checks []Check
	for _, c := range m.checks {
		for _, n := range names {
			if c.Name == n {
				checks = append(checks, c)
				break
			}
		}
	}
	prev := m.last
	m.mu.RUnlock()
	fresh := map[string]Result{}
	for _, r := range runChecks(ctx, checks) {
		fresh[r.Name] = r
	}
	results := make([]Result, 0, len(prev.Results)+len(fresh))
	for _, r := range prev.Results {
		if f, ok := fresh[r.Name]; ok {
			r = f
			delete(fresh, r.Name)
		}
		results = append(results, r)
	}
	for _, r := range fresh { // added since the last round
		results = append(results, r)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return m.keep(prev, results)
}

// keep stores a round's results as the latest report and says if it changed.
func (m *Monitor) keep(prev Report, results []Result) Report {
	rep := Report{Results: results, At: time.Now()}
	m.mu.Lock()
	m.last = rep
	m.mu.Unlock()
	if m.OnChange != nil && changed(prev, rep) {
		m.OnChange(prev, rep)
	}
	return rep
}

// runChecks runs checks in parallel, attempting repairs on failures, and
// returns their results sorted by name.
func runChecks(ctx context.Context, checks []Check) []Result {
	results := make([]Result, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c Check) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			start := time.Now()
			res := c.Run(cctx)
			if res.State == Fail && c.Repair != nil {
				if err := c.Repair(cctx); err == nil {
					res = c.Run(cctx)
					res.Fixed = true
				}
			}
			res.Name, res.Label = c.Name, c.Label
			res.Took = time.Since(start).Round(time.Millisecond)
			res.Checked = time.Now()
			results[i] = res
		}(i, c)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results
}

// Start runs at once, then every interval, until ctx ends.
func (m *Monitor) Start(ctx context.Context, interval time.Duration) {
	go func() {
		m.Run(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Run(ctx)
			}
		}
	}()
}

func changed(a, b Report) bool {
	if len(a.Results) != len(b.Results) {
		return true
	}
	for i := range a.Results {
		if a.Results[i].State != b.Results[i].State {
			return true
		}
	}
	return false
}

// ---- reusable probes -------------------------------------------------------

// Binary checks a program is on PATH.
func Binary(name, label, bin, fix string) Check {
	return Check{Name: name, Label: label, Run: func(context.Context) Result {
		if _, err := exec.LookPath(bin); err != nil {
			return Result{State: Fail, Detail: bin + " not found", Fix: fix}
		}
		return Result{State: OK, Detail: bin + " present"}
	}}
}

// File checks a file exists and is non-empty.
func File(name, label, path, fix string) Check {
	return Check{Name: name, Label: label, Run: func(context.Context) Result {
		st, err := os.Stat(path)
		if err != nil || st.Size() == 0 {
			return Result{State: Fail, Detail: path + " missing", Fix: fix}
		}
		return Result{State: OK, Detail: fmt.Sprintf("%s (%d KB)", path, st.Size()/1024)}
	}}
}

// Func wraps a probe function.
func Func(name, label string, run func(ctx context.Context) (State, string, string), repair func(ctx context.Context) error) Check {
	return Check{Name: name, Label: label, Run: func(ctx context.Context) Result {
		st, detail, fix := run(ctx)
		return Result{State: st, Detail: detail, Fix: fix}
	}, Repair: repair}
}

// DiskFree checks free space under path in GB.
func DiskFree(path string, minGB float64) Check {
	return Check{Name: "disk", Label: "Disk space", Run: func(context.Context) Result {
		free, err := diskFree(path)
		if err != nil {
			return Result{State: Warn, Detail: err.Error()}
		}
		gb := float64(free) / (1 << 30)
		if gb < minGB {
			return Result{State: Warn, Detail: fmt.Sprintf("%.1f GB free", gb), Fix: "free up disk space; memory and voice models need room"}
		}
		return Result{State: OK, Detail: fmt.Sprintf("%.0f GB free", gb)}
	}}
}
