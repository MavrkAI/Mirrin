package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// usageCmd shows what the model has cost: `mirrin usage` for today, this
// month and the last week, `mirrin usage prices` for the prices it uses.
func usageCmd(ctx context.Context, out io.Writer, cfg *config.Config, args []string) error {
	if len(args) > 0 {
		if args[0] == "prices" {
			printPrices(out, cfg)
			return nil
		}
		return fmt.Errorf("usage: mirrin usage [prices]")
	}
	s, err := memory.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	loc, now := zone(cfg), time.Now()
	book := daemon.PriceBook(cfg.Usage)
	sp, err := s.Spend(ctx, now, loc, book, cfg.Usage.MonthlyBudget)
	if err != nil {
		return err
	}
	today := now.In(loc)
	weekAgo := today.AddDate(0, 0, -6).Format(memory.DayFormat)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, loc).Format(memory.DayFormat)
	from := min(weekAgo, monthStart)
	rows, err := s.Usage(ctx, from, sp.Day)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "What the model has cost. These are estimates from list prices; your provider's bill is what counts.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-24s %-10s %s\n", "Today, "+today.Format("Mon 2 Jan"), USDText(sp.Today), callsText(sp.TodayCalls, sp.TodayUse))
	fmt.Fprintf(out, "  %-24s %-10s %s\n", today.Format("January")+" so far", USDText(sp.ToDate), callsText(sp.MonthCalls, sp.MonthUse))
	if sp.Budget > 0 {
		fmt.Fprintf(out, "  %-24s %-10s %.0f%% used\n", "Monthly budget", USDText(sp.Budget), sp.Used*100)
		if sp.Used >= 1 {
			fmt.Fprintln(out, "\n  This month is over your budget. A cheaper model (menu → Model) costs less; background work (protocols, tasks, watching) uses the same model as your chats.")
		} else if sp.Used >= 0.8 {
			fmt.Fprintln(out, "\n  This month is close to your budget.")
		}
	}

	var month []memory.UsageRow
	for _, r := range rows {
		if r.Day >= monthStart {
			month = append(month, r)
		}
	}
	if len(month) > 0 {
		fmt.Fprintf(out, "\n%s by model\n", today.Format("January"))
		for _, g := range group(month, book, func(r memory.UsageRow) string { return r.Model }) {
			fmt.Fprintf(out, "  %-32s %s\n", g.key, costText(g))
		}
		fmt.Fprintf(out, "\n%s by kind of work\n", today.Format("January"))
		for _, g := range group(month, book, func(r memory.UsageRow) string { return kindLabel(r.Kind) }) {
			fmt.Fprintf(out, "  %-32s %s\n", g.key, costText(g))
		}
	}

	fmt.Fprintln(out, "\nLast 7 days")
	byDay := map[string]float64{}
	for _, r := range rows {
		c, _ := memory.RowCost(r, book)
		byDay[r.Day] += c
	}
	for i := 6; i >= 0; i-- {
		d := today.AddDate(0, 0, -i)
		fmt.Fprintf(out, "  %-12s %s\n", d.Format("Mon 2 Jan"), USDText(byDay[d.Format(memory.DayFormat)]))
	}

	if len(sp.Unpriced) > 0 {
		fmt.Fprintf(out, "\nNo price is known for %s, so it isn't in the totals. Add one under usage.prices in config.yaml.\n", strings.Join(sp.Unpriced, ", "))
	}
	fmt.Fprintln(out)
	if sp.Budget > 0 {
		fmt.Fprintln(out, "Change the budget with usage.monthly_budget in config.yaml (0 turns the warning off).")
	} else {
		fmt.Fprintln(out, "Set a monthly budget with usage.monthly_budget in config.yaml to be warned before it runs out.")
	}
	fmt.Fprintln(out, "Use your own prices under usage.prices; `mirrin usage prices` shows the ones in use.")
	return nil
}

// usageGroup is usage summed under one label.
type usageGroup struct {
	key      string
	calls    int64
	tokens   llm.Tokens
	cost     float64
	unpriced bool
	free     bool
}

// group sums rows by a label, most expensive first.
func group(rows []memory.UsageRow, book llm.PriceBook, by func(memory.UsageRow) string) []*usageGroup {
	idx := map[string]*usageGroup{}
	var out []*usageGroup
	for _, r := range rows {
		k := by(r)
		g := idx[k]
		if g == nil {
			g = &usageGroup{key: k, free: true}
			idx[k] = g
			out = append(out, g)
		}
		g.calls += r.Calls
		g.tokens = g.tokens.Add(r.Tokens)
		if _, src, _ := book.Lookup(r.Model); src == llm.PriceLocal {
			continue
		}
		g.free = false
		c, ok := memory.RowCost(r, book)
		g.cost += c
		if !ok {
			g.unpriced = true
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].cost > out[j].cost })
	return out
}

func costText(g *usageGroup) string {
	switch {
	case g.free:
		return "free (runs on this computer), " + callsText(g.calls, g.tokens)
	case g.unpriced && g.cost == 0:
		return "no price known, " + callsText(g.calls, g.tokens)
	case g.unpriced:
		return fmt.Sprintf("%-10s %s (some without a known price)", USDText(g.cost), callsText(g.calls, g.tokens))
	}
	return fmt.Sprintf("%-10s %s", USDText(g.cost), callsText(g.calls, g.tokens))
}

// kindLabel says what a kind of work is in plain words.
func kindLabel(kind string) string {
	switch kind {
	case "chat":
		return "conversations"
	case "voice":
		return "spoken conversations"
	case "protocol":
		return "protocols"
	case "task":
		return "tasks"
	case "watch":
		return "watching calendar and inbox"
	case "call", "phone":
		return "phone calls"
	case "portrait":
		return "weekly portrait"
	case "patterns":
		return "noticing patterns"
	case "nudge", "firstlook":
		return "first-week tips"
	}
	return "other background work"
}

// USDText is an estimate in US dollars.
func USDText(v float64) string { return daemon.USD(v) }

// callsText is "18 calls, 81k tokens in, 6k out".
func callsText(calls int64, t llm.Tokens) string {
	if calls == 0 {
		return "no calls"
	}
	unit := "calls"
	if calls == 1 {
		unit = "call"
	}
	in := t.Input + t.CacheRead + t.CacheWrite
	return fmt.Sprintf("%d %s, %s tokens in, %s out", calls, unit, count(in), count(t.Output))
}

// count abbreviates a token count: 950, 81k, 1.2M.
func count(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

// spendLine is Spend in one line, for a problem report.
func spendLine(sp memory.Spend) string {
	s := fmt.Sprintf("%s today, %s this month", USDText(sp.Today), USDText(sp.ToDate))
	if sp.Budget > 0 {
		s += fmt.Sprintf(" of a %s budget", USDText(sp.Budget))
	}
	s += " (estimate)"
	if len(sp.Unpriced) > 0 {
		s += "; no price known for " + strings.Join(sp.Unpriced, ", ")
	}
	return s
}

func priceCell(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", v)
}

// printPrices lists the prices the estimates use.
func printPrices(out io.Writer, cfg *config.Config) {
	book := daemon.PriceBook(cfg.Usage)
	fmt.Fprintln(out, "Prices the estimates use, in US dollars per million tokens. They go out of date:")
	fmt.Fprintln(out, "set your own under usage.prices in config.yaml, keyed by model.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-24s %8s %8s %11s %11s  %s\n", "model", "input", "output", "cache read", "cache write", "from")
	ids := llm.BuiltinPrices()
	for id := range cfg.Usage.Prices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seen := map[string]bool{}
	current := strings.ToLower(cfg.LLM.Model)
	for _, id := range ids {
		key := strings.ToLower(id)
		if seen[key] {
			continue
		}
		seen[key] = true
		p, src, ok := book.Lookup(id)
		if !ok {
			continue
		}
		mark := " "
		if key == current || strings.HasSuffix(key, "/"+current) {
			mark = "*"
		}
		fmt.Fprintf(out, "%s %-24s %8.2f %8.2f %11s %11s  %s\n", mark, id, p.Input, p.Output, priceCell(p.CacheRead), priceCell(p.CacheWrite), src)
	}
	fmt.Fprintln(out, "\n* is the model in use. Models run by Ollama cost nothing. Where a cache price")
	fmt.Fprintln(out, "shows -, reads count as a tenth of the input price and writes as 1.25 times.")
}
