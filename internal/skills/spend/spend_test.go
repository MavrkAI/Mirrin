package spend

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

type mem map[string]string

func (m mem) Get(_ context.Context, k string) (string, error) { return m[k], nil }
func (m mem) Set(_ context.Context, k, v string) error        { m[k] = v; return nil }

func TestLimits(t *testing.T) {
	l := New(mem{}, func() config.Spending {
		return config.Spending{Currency: "AUD", PerActionLimit: 100, MonthlyLimit: 500}
	})
	ctx := context.Background()
	if ok, ask, r := l.Check(ctx, 40); !ok || ask || !strings.HasPrefix(r, "OK") {
		t.Fatalf("small: %v %v %s", ok, ask, r)
	}
	if ok, ask, _ := l.Check(ctx, 150); !ok || !ask {
		t.Fatal("over per-action should ask")
	}
	for i := 0; i < 4; i++ {
		_ = l.Record(ctx, Entry{At: time.Now(), Amount: 120, Currency: "AUD", Merchant: "x", Purpose: "y"})
	}
	if ok, _, r := l.Check(ctx, 30); ok || !strings.HasPrefix(r, "BLOCKED") {
		t.Fatalf("monthly cap: %v %s", ok, r)
	}
	// Last month's spend doesn't count.
	l2 := New(mem{}, func() config.Spending { return config.Spending{Currency: "AUD", MonthlyLimit: 100} })
	_ = l2.Record(ctx, Entry{At: time.Now().AddDate(0, -1, 0), Amount: 90})
	if ok, _, _ := l2.Check(ctx, 50); !ok {
		t.Fatal("previous month should not count")
	}
}

// With no currency set, amounts are in the owner's country's currency, as a
// new config would have, not a fixed one.
func TestEmptyCurrencyUsesTheDefault(t *testing.T) {
	if !inUS(t, "TestEmptyCurrencyUsesTheDefault") {
		return
	}
	if got := config.Default().Spending.Currency; got != "USD" {
		t.Fatalf("the default currency in the US is %s", got)
	}
	l := New(mem{}, func() config.Spending { return config.Spending{PerActionLimit: 100, MonthlyLimit: 500} })
	_, _, r := l.Check(context.Background(), 40)
	if !strings.Contains(r, " USD;") || strings.Contains(r, "AUD") {
		t.Fatalf("want USD in %q", r)
	}
}

// inUS runs the calling test again in a child process whose clock and
// locale say United States, so the defaults can't happen to be Australia's
// (the region is read once per process). It reports whether this is the
// child, which does the checks.
func inUS(t *testing.T, name string) bool {
	t.Helper()
	if os.Getenv("MIRRIN_TEST_IN_US") != "" {
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), "MIRRIN_TEST_IN_US=1", "TZ=America/New_York", "LC_ALL=en_US.UTF-8", "LC_MONETARY=", "LANG=en_US.UTF-8", "MIRRIN_HOME="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return false
}

// A payment's approval says what is paid and for what, not
// "check_spend(amount=389, purpose=…)".
func TestAPaymentIsAskedInWords(t *testing.T) {
	l := New(mem{}, func() config.Spending { return config.Spending{Currency: "AUD", MonthlyLimit: 100} })
	for _, tool := range l.Tools() {
		if tool.Spec().Name != "check_spend" {
			continue
		}
		s, ok := tool.(tools.Summarizer)
		if !ok {
			t.Fatal("check_spend writes no approval text")
		}
		got := s.ApprovalSummary(tools.Call{Input: []byte(`{"amount":389,"purpose":"Jetstar  MEL-SYD\nTue 6am"}`)})
		if got != "Pay 389.00 AUD for Jetstar MEL-SYD Tue 6am" {
			t.Fatalf("asked %q", got)
		}
		return
	}
	t.Fatal("no check_spend")
}
