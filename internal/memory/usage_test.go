package memory

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

func tokens(in, out int64) llm.Tokens { return llm.Tokens{Input: in, Output: out} }

func TestUsageTalliesAndSpend(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	loc := time.FixedZone("AEST", 10*3600)
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, loc)
	rec := func(day, model, kind string, tk llm.Tokens) {
		t.Helper()
		if err := s.RecordUsage(ctx, day, model, kind, tk); err != nil {
			t.Fatal(err)
		}
	}
	rec("2026-09-27", "anthropic/claude-opus-5", "chat", tokens(1_000_000, 0))
	rec("2026-09-27", "anthropic/claude-opus-5", "chat", tokens(0, 100_000)) // same row, second call
	rec("2026-09-27", "anthropic/claude-opus-5", "protocol", llm.Tokens{CacheRead: 1_000_000})
	rec("2026-09-02", "openai/gpt-4.1", "chat", tokens(1_000_000, 0))
	rec("2026-09-02", "custom/mystery", "chat", tokens(5, 5))
	rec("2026-08-31", "anthropic/claude-opus-5", "chat", tokens(9_000_000, 0)) // last month
	rec("2026-09-27", "ollama/llama3.1", "chat", tokens(1_000_000, 1_000_000))

	rows, _ := s.Usage(ctx, "2026-09-27", "2026-09-27")
	var chat UsageRow
	for _, r := range rows {
		if r.Model == "anthropic/claude-opus-5" && r.Kind == "chat" {
			chat = r
		}
	}
	if chat.Calls != 2 || chat.Input != 1_000_000 || chat.Output != 100_000 {
		t.Fatalf("calls must add up in one row: %+v", chat)
	}

	sp, err := s.Spend(ctx, now, loc, llm.NewPriceBook(nil), 20)
	if err != nil {
		t.Fatal(err)
	}
	wantToday := 5 + 2.5 + 0.5 // input, output, cache reads on Opus; Ollama is free
	wantMonth := wantToday + 2 // plus GPT-4.1 input
	if math.Abs(sp.Today-wantToday) > 1e-9 || math.Abs(sp.ToDate-wantMonth) > 1e-9 {
		t.Fatalf("today %v (want %v), month %v (want %v)", sp.Today, wantToday, sp.ToDate, wantMonth)
	}
	if sp.Day != "2026-09-27" || sp.Month != "2026-09" || sp.TodayCalls != 4 || sp.MonthCalls != 6 {
		t.Fatalf("spend %+v", sp)
	}
	if math.Abs(sp.Used-wantMonth/20) > 1e-9 || !sp.Estimate || sp.Currency != "USD" {
		t.Fatalf("budget share %+v", sp)
	}
	if len(sp.Unpriced) != 1 || sp.Unpriced[0] != "custom/mystery" {
		t.Fatalf("unpriced models must be named: %v", sp.Unpriced)
	}
	// The owner's price wins.
	sp, _ = s.Spend(ctx, now, loc, llm.NewPriceBook(map[string]llm.Price{"custom/mystery": {Input: 1e6, Output: 0}}), 0)
	if len(sp.Unpriced) != 0 || sp.Used != 0 || math.Abs(sp.ToDate-(wantMonth+5)) > 1e-6 {
		t.Fatalf("own price: %+v", sp)
	}
}

func TestUsageKind(t *testing.T) {
	cases := map[string]string{
		"telegram:1":                             "chat",
		"irc:#golang|tony":                       "chat",
		"voice:local":                            "voice",
		"telegram:1#protocol-morning-brief-0927": "protocol",
		"whatsapp:61400#task-0927#protocol-x-1":  "task",
		"voice:phone#call-abc":                   "call",
		"telegram:1#watch-0927":                  "watch",
	}
	for key, want := range cases {
		if got := UsageKind(key); got != want {
			t.Errorf("UsageKind(%q) = %q, want %q", key, got, want)
		}
	}
}

// The presence screen reads Spend as JSON: every count is there, zero or not.
func TestSpendJSONHasEveryField(t *testing.T) {
	s := openTest(t)
	sp, err := s.Spend(context.Background(), time.Now(), time.UTC, llm.NewPriceBook(nil), 25)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(sp)
	for _, want := range []string{`"today_tokens":{"input":0,"output":0,"cache_read":0,"cache_write":0}`, `"unpriced":[]`, `"estimate":true`, `"currency":"USD"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
}
