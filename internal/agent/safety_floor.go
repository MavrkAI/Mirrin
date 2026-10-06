package agent

import (
	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Keep the stored risk, voice checks and approval cards consistent with the
// policy floor, including check_spend, whose declared risk predates the floor.
func (a *Agent) safetyFloor(name string, risk tools.Risk) tools.Risk {
	if reason := approvals.SafetyFloor(name, risk); reason != "" {
		a.log.Info("hard safety floor: automatic execution prohibited", "tool", name, "category", reason)
		return tools.RiskDangerous
	}
	return risk
}
