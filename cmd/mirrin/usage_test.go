package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

func TestUsageCommandShowsTodayMonthAndBudget(t *testing.T) {
	home(t)
	cfg := config.Default()
	cfg.Usage.MonthlyBudget = 1
	cfg.Usage.Prices = map[string]config.ModelPrice{"custom/mine": {Input: 1, Output: 1}}
	s, err := memory.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	today := time.Now().Format(memory.DayFormat)
	for _, r := range []struct {
		model, kind string
		t           llm.Tokens
	}{
		{"anthropic/claude-opus-5", "chat", llm.Tokens{Input: 200_000, Output: 10_000}},
		{"anthropic/claude-opus-5", "protocol", llm.Tokens{Input: 100_000}},
		{"ollama/llama3.1", "watch", llm.Tokens{Input: 5000, Output: 500}},
		{"custom/mine", "task", llm.Tokens{Input: 1_000_000}},
		{"custom/unknown", "chat", llm.Tokens{Input: 10}},
	} {
		if err := s.RecordUsage(ctx, today, r.model, r.kind, r.t); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	var out strings.Builder
	if err := usageCmd(ctx, &out, cfg, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"estimates", "Today, ", "US$2.75", // 1.00 + 0.25 + 0.50 (Opus) + 1.00 (own price)
		"Monthly budget", "275% used", "over your budget",
		"anthropic/claude-opus-5", "free (runs on this computer)", "custom/unknown", "no price known",
		"conversations", "protocols", "watching calendar and inbox", "tasks",
		"Last 7 days", "No price is known for custom/unknown", "usage.monthly_budget",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("usage lacks %q:\n%s", want, got)
		}
	}
	out.Reset()
	if err := usageCmd(ctx, &out, cfg, []string{"prices"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "* claude-opus-5") || !strings.Contains(out.String(), "custom/mine") || !strings.Contains(out.String(), "your config") {
		t.Fatalf("prices:\n%s", out.String())
	}
}
