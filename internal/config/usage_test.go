package config

import (
	"os"
	"testing"
)

// A config saved before these settings existed gets the defaults; one that
// sets them keeps what it says, including 0 for "never".
func TestUsageAndRetentionDefaults(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(Path(), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("llm:\n  provider: ollama\n  model: llama3.1\nchannels:\n  whatsapp:\n    enabled: false\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Usage.MonthlyBudget != DefaultMonthlyBudget || cfg.Retention.AuditDays != 90 || cfg.Retention.TrimAfterDays != 30 {
		t.Fatalf("defaults: %+v %+v", cfg.Usage, cfg.Retention)
	}
	write("llm:\n  provider: ollama\n  model: llama3.1\nchannels:\n  whatsapp:\n    enabled: false\n" +
		"usage:\n  monthly_budget: 0\n  prices:\n    custom/my-model: {input: 1.5, output: 3}\n" +
		"retention:\n  audit_days: 0\n  trim_after_days: 7\n")
	if cfg, err = Load(); err != nil {
		t.Fatal(err)
	}
	if cfg.Usage.MonthlyBudget != 0 || cfg.Usage.Prices["custom/my-model"].Output != 3 || cfg.Retention.AuditDays != 0 || cfg.Retention.TrimAfterDays != 7 {
		t.Fatalf("own settings: %+v %+v", cfg.Usage, cfg.Retention)
	}
	// And they survive a save.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil || again.Usage.Prices["custom/my-model"].Input != 1.5 || again.Retention.TrimAfterDays != 7 || again.Retention.AuditDays != 0 {
		t.Fatalf("after save: %v %+v %+v", err, again.Usage, again.Retention)
	}
}
