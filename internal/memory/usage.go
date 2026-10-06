package memory

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Model usage is kept as one row per day, model and kind of work, so the
// table stays small (a few hundred rows a year) and says nothing about what
// was asked.
func init() {
	Register(Migration{Version: 3, Name: "model-usage", Up: func(ctx context.Context, db Execer) error {
		return execAll(ctx, db, `CREATE TABLE IF NOT EXISTS model_usage (
			day TEXT NOT NULL,
			model TEXT NOT NULL,
			kind TEXT NOT NULL,
			calls INTEGER NOT NULL DEFAULT 0,
			input INTEGER NOT NULL DEFAULT 0,
			output INTEGER NOT NULL DEFAULT 0,
			cache_read INTEGER NOT NULL DEFAULT 0,
			cache_write INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, model, kind)
		)`)
	}})
}

// DayFormat is how usage days are written: the owner's calendar day.
const DayFormat = "2006-01-02"

// UsageRow is what one model used on one day for one kind of work.
type UsageRow struct {
	Day   string `json:"day"`
	Model string `json:"model"`
	// Kind is "chat" for conversations, "voice" for spoken ones, or the
	// background work it was for: protocol, task, watch, portrait, …
	Kind  string `json:"kind"`
	Calls int64  `json:"calls"`
	llm.Tokens
}

// RecordUsage adds one model call's tokens to the day's tally.
func (s *Store) RecordUsage(ctx context.Context, day, model, kind string, t llm.Tokens) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO model_usage(day, model, kind, calls, input, output, cache_read, cache_write)
		VALUES(?,?,?,1,?,?,?,?)
		ON CONFLICT(day, model, kind) DO UPDATE SET calls=calls+1, input=input+excluded.input, output=output+excluded.output,
			cache_read=cache_read+excluded.cache_read, cache_write=cache_write+excluded.cache_write`,
		day, model, kind, t.Input, t.Output, t.CacheRead, t.CacheWrite)
	return err
}

// Usage lists the tallies for the days from..to (inclusive, DayFormat).
func (s *Store) Usage(ctx context.Context, from, to string) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, model, kind, calls, input, output, cache_read, cache_write
		FROM model_usage WHERE day >= ? AND day <= ? ORDER BY day, model, kind`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Day, &r.Model, &r.Kind, &r.Calls, &r.Input, &r.Output, &r.CacheRead, &r.CacheWrite); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageKind names the kind of work a conversation is, for the usage table:
// the purpose of a background run ("protocol", "task", "watch", …), "voice"
// for a spoken conversation, else "chat".
func UsageKind(chatKey string) string {
	if live := LiveKey(chatKey); live != chatKey {
		rest := chatKey[len(live)+1:]
		if i := strings.IndexAny(rest, "-#"); i > 0 {
			return rest[:i]
		}
		return "background"
	}
	if strings.HasPrefix(chatKey, "voice:") {
		return "voice"
	}
	return "chat"
}

// Spend is model use turned into money: an estimate from list prices (or the
// owner's own), for today and this calendar month, against the owner's
// monthly budget. Amounts are US dollars.
type Spend struct {
	Day    string  `json:"day"`   // today, DayFormat
	Month  string  `json:"month"` // this month, "2006-01"
	Today  float64 `json:"today"`
	ToDate float64 `json:"month_to_date"`
	// Budget is the owner's monthly budget; 0 when none is set.
	Budget float64 `json:"budget"`
	// Used is ToDate as a share of Budget (0 without a budget).
	Used       float64    `json:"used"`
	TodayCalls int64      `json:"today_calls"`
	MonthCalls int64      `json:"month_calls"`
	TodayUse   llm.Tokens `json:"today_tokens"`
	MonthUse   llm.Tokens `json:"month_tokens"`
	// Unpriced lists models used this month with no known price; their
	// use is counted in tokens but not in money. Never nil, so the JSON
	// always has a list.
	Unpriced []string `json:"unpriced"`
	Currency string   `json:"currency"`
	Estimate bool     `json:"estimate"`
}

// Spend estimates today's and this month's model spend in the owner's time
// zone.
func (s *Store) Spend(ctx context.Context, now time.Time, loc *time.Location, prices llm.PriceBook, budget float64) (Spend, error) {
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	day := now.Format(DayFormat)
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).Format(DayFormat)
	rows, err := s.Usage(ctx, first, day)
	if err != nil {
		return Spend{}, err
	}
	sp := Spend{Day: day, Month: now.Format("2006-01"), Budget: budget, Unpriced: []string{}, Currency: "USD", Estimate: true}
	unpriced := map[string]bool{}
	for _, r := range rows {
		cost, ok := RowCost(r, prices)
		if !ok {
			unpriced[r.Model] = true
		}
		sp.ToDate += cost
		sp.MonthCalls += r.Calls
		sp.MonthUse = sp.MonthUse.Add(r.Tokens)
		if r.Day == day {
			sp.Today += cost
			sp.TodayCalls += r.Calls
			sp.TodayUse = sp.TodayUse.Add(r.Tokens)
		}
	}
	for m := range unpriced {
		sp.Unpriced = append(sp.Unpriced, m)
	}
	sort.Strings(sp.Unpriced)
	if budget > 0 {
		sp.Used = sp.ToDate / budget
	}
	return sp, nil
}

// RowCost is the estimated cost of a usage row; ok is false when its model
// has no known price (the cost is then 0).
func RowCost(r UsageRow, prices llm.PriceBook) (float64, bool) {
	p, _, ok := prices.Lookup(r.Model)
	if !ok {
		return 0, false
	}
	return p.Cost(r.Tokens), true
}
