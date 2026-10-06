// Package approvals decides whether a tool call may run on its own or needs
// the user's explicit permission. This is what makes it safe to hand Mirrin
// the keys to your life.
package approvals

import (
	"slices"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Decision is the policy outcome for a tool call.
type Decision int

const (
	// Allow runs the tool immediately.
	Allow Decision = iota
	// Ask pauses the tool call until the user approves it.
	Ask
	// Deny refuses the tool call outright.
	Deny
)

// Policy maps tool risk and name to a decision.
type Policy struct {
	cfg config.Autonomy
}

// New builds a policy from configuration.
func New(cfg config.Autonomy) *Policy { return &Policy{cfg: cfg} }

// Decide returns what should happen for a call to the given tool.
func (p *Policy) Decide(t tools.Tool) Decision { return p.DecideRisk(t, t.Risk()) }

// DecideUntrusted is DecideRisk for a run whose input is other people's
// words (a watcher reading mail and invites): always_allow, which the owner
// gave for what they ask for themselves, doesn't cover a call there that
// changes anything, so a crafted email can't send mail or book a meeting
// unseen. The autonomy levels themselves still apply.
func (p *Policy) DecideUntrusted(t tools.Tool, risk tools.Risk) Decision {
	if risk >= tools.RiskWrite && slices.Contains(p.cfg.AlwaysAllow, t.Spec().Name) {
		strict := Policy{cfg: p.cfg}
		strict.cfg.AlwaysAllow = nil
		return strict.DecideRisk(t, risk)
	}
	return p.DecideRisk(t, risk)
}

// DecideRisk is Decide with the risk of this particular call.
func (p *Policy) DecideRisk(t tools.Tool, risk tools.Risk) Decision {
	decision := p.configuredDecision(t, risk)
	if decision == Allow && SafetyFloor(t.Spec().Name, risk) != "" {
		return Ask
	}
	return decision
}

// configuredDecision preserves the owner's existing override precedence.
// The safety floor may tighten Allow, but must never weaken Deny.
func (p *Policy) configuredDecision(t tools.Tool, risk tools.Risk) Decision {
	name := t.Spec().Name
	if slices.Contains(p.cfg.AlwaysAsk, name) {
		return Ask
	}
	if slices.Contains(p.cfg.AlwaysAllow, name) && risk < tools.RiskDangerous {
		return Allow
	}
	level := p.cfg.Read
	switch risk {
	case tools.RiskWrite:
		level = p.cfg.Write
	case tools.RiskDangerous:
		level = p.cfg.Dangerous
	}
	switch level {
	case "auto":
		return Allow
	case "never":
		return Deny
	default:
		return Ask
	}
}
