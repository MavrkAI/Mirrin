// Package spend is the purse: a ledger of what the twin has paid for on the
// user's behalf and the limits that decide when it must stop and ask.
package spend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Entry is one payment.
type Entry struct {
	At       time.Time `json:"at"`
	Amount   float64   `json:"amount"`
	Currency string    `json:"currency"`
	Merchant string    `json:"merchant"`
	Purpose  string    `json:"purpose"`
	ChatKey  string    `json:"chat_key,omitempty"`
}

// KV is the persistence the ledger needs.
type KV interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
}

// Ledger tracks spending against limits.
type Ledger struct {
	kv  KV
	cfg func() config.Spending
	mu  sync.Mutex
}

const key = "spend.ledger"

// New builds a ledger; cfg is read live so limit changes apply at once.
func New(kv KV, cfg func() config.Spending) *Ledger { return &Ledger{kv: kv, cfg: cfg} }

func (l *Ledger) load(ctx context.Context) []Entry {
	var out []Entry
	if data, err := l.kv.Get(ctx, key); err == nil && data != "" {
		_ = json.Unmarshal([]byte(data), &out)
	}
	return out
}

// MonthTotal is what has been recorded this calendar month.
func (l *Ledger) MonthTotal(ctx context.Context) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return monthTotal(l.load(ctx), time.Now())
}

func monthTotal(entries []Entry, now time.Time) float64 {
	y, m, _ := now.Date()
	var total float64
	for _, e := range entries {
		ey, em, _ := e.At.Date()
		if ey == y && em == m {
			total += e.Amount
		}
	}
	return total
}

// Check says whether a payment of amount may go ahead.
// ok=false blocks it; ask=true means it needs the user's explicit yes.
func (l *Ledger) Check(ctx context.Context, amount float64) (ok, ask bool, reason string) {
	c := l.cfg()
	l.mu.Lock()
	total := monthTotal(l.load(ctx), time.Now())
	l.mu.Unlock()
	cur := c.Currency
	if cur == "" {
		cur = config.Default().Spending.Currency // the owner's country's, as a new config has
	}
	if c.MonthlyLimit > 0 && total+amount > c.MonthlyLimit {
		return false, false, fmt.Sprintf("BLOCKED: this would take this month's spend to %.2f %s, over the monthly limit of %.2f. Do not pay. Tell the user; only they can raise the limit, on the Spending page (Spending… in the menu bar) on the computer you run on.", total+amount, cur, c.MonthlyLimit)
	}
	if c.PerActionLimit > 0 && amount > c.PerActionLimit {
		return true, true, fmt.Sprintf("NEEDS EXPLICIT YES: %.2f %s is over the per-payment limit of %.2f. Show the user exactly what and how much, and only proceed on a clear yes to that amount. This month so far: %.2f of %.2f.", amount, cur, c.PerActionLimit, total, c.MonthlyLimit)
	}
	return true, false, fmt.Sprintf("OK within limits. This month so far: %.2f of %.2f %s; per-payment limit %.2f.", total, c.MonthlyLimit, cur, c.PerActionLimit)
}

// Entries is every payment on record, oldest first (for the Spending page).
func (l *Ledger) Entries(ctx context.Context) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.load(ctx)
}

// Record adds a payment to the ledger.
func (l *Ledger) Record(ctx context.Context, e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := append(l.load(ctx), e)
	if len(entries) > 2000 {
		entries = entries[len(entries)-2000:]
	}
	data, _ := json.Marshal(entries)
	return l.kv.Set(ctx, key, string(data))
}

// Summary lists this month's payments for the user.
func (l *Ledger) Summary(ctx context.Context) string {
	c := l.cfg()
	l.mu.Lock()
	entries := l.load(ctx)
	l.mu.Unlock()
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "This month: %.2f of %.2f %s (per payment up to %.2f).\n", monthTotal(entries, now), c.MonthlyLimit, c.Currency, c.PerActionLimit)
	n := 0
	for i := len(entries) - 1; i >= 0 && n < 15; i-- {
		e := entries[i]
		if ey, em, _ := e.At.Date(); ey != now.Year() || em != now.Month() {
			continue
		}
		fmt.Fprintf(&b, "%s  %.2f %s  %s  %s\n", e.At.Format("02 Jan"), e.Amount, e.Currency, e.Merchant, e.Purpose)
		n++
	}
	if n == 0 {
		b.WriteString("No payments recorded this month.")
	}
	return strings.TrimSpace(b.String())
}

// paySummary is what the owner is asked to approve: "Pay 389.00 AUD for
// Jetstar MEL-SYD Tue 6am", not the call's arguments.
func (l *Ledger) paySummary(call tools.Call) string {
	var in struct {
		Amount  float64
		Purpose string
	}
	_ = tools.Decode(call, &in)
	s := fmt.Sprintf("Pay %.2f %s", in.Amount, l.cfg().Currency)
	if p := strings.Join(strings.Fields(in.Purpose), " "); p != "" {
		s += " for " + p
	}
	return s
}

// Tools returns check_spend and record_spend.
func (l *Ledger) Tools() []tools.Tool {
	return []tools.Tool{
		tools.WithSummary(tools.New("check_spend",
			"Call this BEFORE any step that pays, buys, books with a card, subscribes or transfers money, with the amount. It tells you whether the payment is within the user's limits, needs their explicit yes, or is blocked. Never pay without checking.",
			tools.Schema(map[string]tools.Prop{
				"amount":  {Type: "number", Description: "Total to be charged", Required: true},
				"purpose": {Type: "string", Description: "What it's for, e.g. \"Jetstar MEL-SYD Tue 6am\"", Required: true},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					Amount  float64
					Purpose string
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				_, _, reason := l.Check(ctx, in.Amount)
				return reason, nil
			}), l.paySummary),
		tools.New("record_spend",
			"Call this right AFTER a payment has actually gone through, so the ledger and the monthly limit stay right.",
			tools.Schema(map[string]tools.Prop{
				"amount":   {Type: "number", Required: true},
				"currency": {Type: "string", Description: "Default: the configured currency"},
				"merchant": {Type: "string", Required: true},
				"purpose":  {Type: "string", Required: true},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					Amount             float64
					Currency, Merchant string
					Purpose            string
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if in.Currency == "" {
					in.Currency = l.cfg().Currency
				}
				if err := l.Record(ctx, Entry{At: time.Now(), Amount: in.Amount, Currency: in.Currency, Merchant: in.Merchant, Purpose: in.Purpose, ChatKey: call.ChatKey}); err != nil {
					return "", err
				}
				return "recorded. " + l.Summary(ctx), nil
			}),
	}
}
