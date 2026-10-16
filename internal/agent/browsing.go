package agent

import (
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Ordinary browsing (browser_act, browser_run) is a read for the owner,
// so it runs unasked. In a watcher's run or a follow-up's the twin acts
// on what others wrote, an email or a page, and a click there is a write:
// a crafted message mustn't get it pressing buttons on the owner's
// signed-in sites without them.
func untrustedRisk(chatKey, tool string, risk tools.Risk) tools.Risk {
	if risk >= tools.RiskWrite || (tool != "browser_act" && tool != "browser_run") {
		return risk
	}
	if memory.IsWatchRun(chatKey) || memory.UsageKind(chatKey) == "followup" {
		return tools.RiskWrite
	}
	return risk
}
