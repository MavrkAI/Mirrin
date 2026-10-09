package approvals

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func TestSafetyFloorCannotBeConfiguredAway(t *testing.T) {
	for _, tc := range []struct {
		name string
		risk tools.Risk
	}{
		{"check_spend", tools.RiskRead},
		{"run_shell", tools.RiskDangerous}, {"browser_act", tools.RiskDangerous}, {"browser_run", tools.RiskDangerous},
		{"read_file", tools.RiskDangerous}, {"write_file", tools.RiskDangerous}, {"list_dir", tools.RiskDangerous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := tools.New(tc.name, "", nil, tc.risk, nil)
			for _, allow := range [][]string{nil, {tc.name}} {
				p := New(config.Autonomy{Read: "auto", Write: "auto", Dangerous: "auto", AlwaysAllow: allow})
				if got := p.DecideRisk(tool, tc.risk); got != Ask {
					t.Fatalf("decision %v with always_allow %v", got, allow)
				}
				if got := p.DecideUntrusted(tool, tc.risk); got != Ask {
					t.Fatalf("untrusted decision %v", got)
				}
			}
		})
	}
}

func TestSafetyFloorLeavesOrdinaryCallsAlone(t *testing.T) {
	p := New(config.Autonomy{Read: "auto", Write: "auto", Dangerous: "auto"})
	for _, tc := range []struct {
		name string
		risk tools.Risk
	}{
		{"record_spend", tools.RiskRead}, {"read_file", tools.RiskRead}, {"write_file", tools.RiskWrite}, {"browser_act", tools.RiskWrite}, {"other", tools.RiskDangerous},
	} {
		if p.DecideRisk(tools.New(tc.name, "", nil, tc.risk, nil), tc.risk) != Allow {
			t.Fatal(tc.name)
		}
	}
}

func TestSafetyFloorPreservesNever(t *testing.T) {
	for _, name := range []string{"run_shell", "browser_act", "read_file", "write_file", "list_dir", "check_spend"} {
		t.Run(name, func(t *testing.T) {
			p := New(config.Autonomy{Read: "auto", Write: "auto", Dangerous: "never"})
			tool := tools.New(name, "", nil, tools.RiskDangerous, nil)
			if got := p.DecideRisk(tool, tools.RiskDangerous); got != Deny {
				t.Fatalf("got %v, want Deny", got)
			}
			if got := p.DecideUntrusted(tool, tools.RiskDangerous); got != Deny {
				t.Fatalf("untrusted got %v, want Deny", got)
			}
		})
	}
	// check_spend's declared read risk also respects a disabled read policy.
	p := New(config.Autonomy{Read: "never", Write: "never", Dangerous: "never"})
	if got := p.Decide(tools.New("check_spend", "", nil, tools.RiskRead, nil)); got != Deny {
		t.Fatalf("read check_spend: %v", got)
	}
}
