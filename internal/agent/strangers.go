package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Turns for anyone but the owner (reply_to_others chats) are marked on the
// context. The approvals policy describes what the owner lets the twin do on
// its own; it says nothing about doing it for someone else, so in such a
// turn every tool call waits for the owner's yes and none of the owner's
// memory is in the prompt.

type strangerKey struct{}

// ForStranger marks ctx as a turn for sender, who is not the principal.
// The name is theirs to choose, so it is cleaned before the prompt or an
// approval shows it.
func ForStranger(ctx context.Context, sender string) context.Context {
	return context.WithValue(ctx, strangerKey{}, cleanName(sender))
}

// cleanName keeps a sender's name to one short line.
func cleanName(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40]) + "…"
	}
	if s == "" {
		s = "someone"
	}
	return s
}

// strangerBudget is the tool calls a stranger's turn may make. Their text
// is framed "[Message from ...]", which classify would read as a protocol
// and give 25; every call waits for the owner anyway, so keep it small.
const strangerBudget = 3

// stranger returns who a stranger's turn is for.
func stranger(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(strangerKey{}).(string)
	return s, ok
}

// strangerApprovalKey remembers who asked for an approval, so the turn that
// follows the owner's decision is still that person's turn.
func strangerApprovalKey(id int64) string { return fmt.Sprintf("approval.%d.for", id) }

// withApprovalRequester restores the stranger mark for an approval they asked for.
func (a *Agent) withApprovalRequester(ctx context.Context, id int64) context.Context {
	if who, ok := a.ApprovalRequester(ctx, id); ok {
		return ForStranger(ctx, who)
	}
	return ctx
}

// ApprovalRequester is who asked for approval id when it wasn't the owner.
func (a *Agent) ApprovalRequester(ctx context.Context, id int64) (string, bool) {
	who, err := a.store.Get(ctx, strangerApprovalKey(id))
	return who, err == nil && who != ""
}

// decide is the approvals policy for this call and this caller. A watcher's
// run acts on what others wrote, so always_allow doesn't cover it there. A
// follow-up's run looks, reading only, at what others wrote (a reply, a
// page) with no one watching: neither always_allow nor autonomy lets it
// change anything on its own, so a write it wants is put to the owner.
func (a *Agent) decide(ctx context.Context, chatKey string, tool tools.Tool, risk tools.Risk) approvals.Decision {
	gate := a.gate() // the live policy, even mid-turn (turn.go)
	d := gate.DecideRisk(tool, risk)
	if memory.IsWatchRun(chatKey) {
		d = gate.DecideUntrusted(tool, risk)
	}
	if memory.UsageKind(chatKey) == "followup" && risk >= tools.RiskWrite && d == approvals.Allow {
		return approvals.Ask
	}
	if _, ok := stranger(ctx); ok && d == approvals.Allow {
		return approvals.Ask
	}
	return d
}

// principal is how the owner is named to the model.
func (a *Agent) principal() string {
	if a.cfg.User.Name != "" {
		return a.cfg.User.Name
	}
	return "the user"
}

// systemFor is the stable system prompt; a stranger's turn leaves out what
// the owner wrote about themselves.
func (a *Agent) systemFor(ctx context.Context) string {
	cfg := a.cfg
	if _, ok := stranger(ctx); ok && cfg.User.About != "" {
		cp := *cfg
		cp.User.About = ""
		cfg = &cp
	}
	return persona(cfg, a.persona, a.tools.Names())
}
