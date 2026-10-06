package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/health"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// PriceBook is the model prices the owner's config asks for: their own
// (usage.prices) over the built-in list.
func PriceBook(u config.Usage) llm.PriceBook {
	own := make(map[string]llm.Price, len(u.Prices))
	for k, p := range u.Prices {
		own[k] = llm.Price(p)
	}
	return llm.NewPriceBook(own)
}

// Spend is what the model has cost today and this month (an estimate in US
// dollars) against the owner's monthly budget, for the presence screen, the
// menu and Health.
func (d *Daemon) Spend(ctx context.Context) (memory.Spend, error) {
	cfg := d.Config()
	return d.store.Spend(ctx, time.Now(), d.location(), PriceBook(cfg.Usage), cfg.Usage.MonthlyBudget)
}

// spendHealth is the "Model spending" self-check: a warning at 80% of the
// monthly budget and past it, so the first the owner hears of a big month
// isn't the provider's bill. A running twin also says so on the desktop.
func (d *Daemon) spendHealth(ctx context.Context) (health.State, string, string) {
	sp, err := d.Spend(ctx)
	if err != nil {
		return health.Warn, "couldn't add up model use: " + err.Error(), ""
	}
	if d.runCtx != nil { // not for a one-off check (doctor, report)
		d.budgetNotice(ctx, sp)
	}
	return spendState(sp)
}

// ownNoticeOnly reports whether the only self-checks that changed between
// two rounds are ones that give their own notice (model spending, and
// Google signing the twin out, which tells the owner how to fix it), so the
// general "Self-check" notice would only repeat it.
func ownNoticeOnly(prev, cur health.Report) bool {
	if len(prev.Results) != len(cur.Results) {
		return false
	}
	was := make(map[string]health.State, len(prev.Results))
	for _, r := range prev.Results {
		was[r.Name] = r.State
	}
	for _, r := range cur.Results {
		if s, ok := was[r.Name]; (!ok || s != r.State) && !selfNotifying(r) {
			return false
		}
	}
	return true
}

// selfNotifying reports whether a check's result is one that has already
// told the owner itself: spending (budgetNotice), and Google's sign-out
// (googleSignedOut, from googleHealth). Other Google trouble isn't.
func selfNotifying(r health.Result) bool {
	switch r.Name {
	case "spend":
		return true
	case "google":
		return strings.HasPrefix(r.Detail, googleSignedOutDetail) || r.Detail == googleClientRefusedDetail
	}
	return false
}

// budgetMu keeps two health rounds at once from both giving a notice.
var budgetMu sync.Mutex

// budgetNotice tells the owner, once a month each, when model use reaches
// 80% of the monthly budget and when it passes it: whenever the twin sees
// it, including at start when the line was crossed while it wasn't running.
// What was said is kept in kv as usage.notified.<yyyy-mm>.80 and .100.
func (d *Daemon) budgetNotice(ctx context.Context, sp memory.Spend) {
	if sp.Budget <= 0 || sp.Used < 0.8 || sp.Month == "" {
		return
	}
	budgetMu.Lock()
	defer budgetMu.Unlock()
	level := "80"
	text := fmt.Sprintf("Your model use is about %s of your %s monthly budget (estimate). Model spending in the menu bar keeps the running total; mirrin usage in Terminal has the details.", USD(sp.ToDate), USD(sp.Budget))
	if sp.Used >= 1 {
		level = "100"
		text = fmt.Sprintf("Your model use has passed your %s monthly budget: about %s so far this month (estimate). Model spending in the menu bar keeps the running total; mirrin usage in Terminal has the details.", USD(sp.Budget), USD(sp.ToDate))
	}
	key := "usage.notified." + sp.Month + "." + level
	if v, _ := d.store.Get(ctx, key); v != "" {
		return
	}
	stamp := time.Now().Format(time.RFC3339)
	if err := d.store.Set(ctx, key, stamp); err != nil {
		d.log.Warn("budget notice", "err", err)
		return // shown once, not every hour
	}
	if level == "100" { // the 80% notice would be old news now
		_ = d.store.Set(ctx, "usage.notified."+sp.Month+".80", stamp)
	}
	d.log.Info("budget notice", "month", sp.Month, "level", level)
	if err := desktopNotify(d.Config().Name, text); err != nil {
		d.log.Warn("budget notice not shown", "err", err)
	}
}

// spendState words a Spend for Health.
func spendState(sp memory.Spend) (health.State, string, string) {
	var unpriced string
	if len(sp.Unpriced) > 0 {
		unpriced = fmt.Sprintf("; no price known for %s, so it isn't counted", strings.Join(sp.Unpriced, ", "))
	}
	const fix = "a cheaper model (menu bar → Model) costs less; to change the budget, set usage.monthly_budget in the settings file. Type mirrin usage in Terminal for the details"
	switch {
	case sp.MonthCalls == 0:
		return health.OK, "nothing used this month", ""
	case sp.Budget <= 0:
		return health.OK, fmt.Sprintf("%s today, %s this month (estimate)", USD(sp.Today), USD(sp.ToDate)) + unpriced, ""
	case sp.Used >= 1:
		return health.Warn, fmt.Sprintf("over your %s monthly budget: %s so far this month (estimate)", USD(sp.Budget), USD(sp.ToDate)) + unpriced, fix
	case sp.Used >= 0.8:
		return health.Warn, fmt.Sprintf("%.0f%% of your %s monthly budget used: %s so far, %s today (estimate)", sp.Used*100, USD(sp.Budget), USD(sp.ToDate), USD(sp.Today)) + unpriced, fix
	}
	return health.OK, fmt.Sprintf("%s of your %s monthly budget, %s today (estimate)", USD(sp.ToDate), USD(sp.Budget), USD(sp.Today)) + unpriced, ""
}

// USD formats an estimate in US dollars: cents under $100, whole dollars above.
func USD(v float64) string {
	switch {
	case v > 0 && v < 0.01:
		return "under US$0.01"
	case v >= 100:
		return fmt.Sprintf("US$%.0f", v)
	}
	return fmt.Sprintf("US$%.2f", v)
}

// tidySchedule is when the retention policy runs: twice a day on the wall
// clock (the heartbeat's scheduler, which keeps time while a Mac sleeps, as
// Go's timers don't), made up once on waking.
const tidySchedule = "17 3,15 * * *"

// keepMemoryTidy applies the retention policy (retention: in config.yaml)
// a couple of minutes after start and twice a day after that: old activity
// log entries go, long quotes and tool output are cut short.
func (d *Daemon) keepMemoryTidy() {
	ctx := d.runCtx
	if ctx == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Minute):
			d.tidyMemory(ctx)
		}
	}()
	d.beat.AddJob(tidySchedule, d.tidyMemory)
}

// tidyMemory runs the retention policy once.
func (d *Daemon) tidyMemory(ctx context.Context) {
	r := d.Config().Retention
	res, err := d.store.Tidy(ctx, memory.Retention{AuditDays: r.AuditDays, TrimAfterDays: r.TrimAfterDays})
	switch {
	case err != nil:
		d.log.Warn("memory upkeep failed", "err", err)
	case res != (memory.TidyResult{}):
		d.log.Info("memory upkeep", "activity_deleted", res.AuditDeleted, "activity_trimmed", res.AuditTrimmed, "tool_results_trimmed", res.ResultsTrimmed)
	}
}
