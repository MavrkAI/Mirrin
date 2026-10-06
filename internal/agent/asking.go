package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// ask queues an approval, at the risk the call was judged to carry, and
// returns what the model is told about it.
func (a *Agent) ask(ctx context.Context, chatKey string, tool tools.Tool, name string, input json.RawMessage, risk tools.Risk) (string, bool) {
	summary := summarize(name, input)
	s, detailed := tool.(tools.Summarizer)
	if detailed {
		summary = s.ApprovalSummary(tools.Call{ChatKey: chatKey, Input: input})
	}
	who, forStranger := stranger(ctx)
	asked := summary
	if forStranger {
		// One request at a time, so a stranger can't flood the owner's queue.
		if pending, err := a.store.PendingApprovals(ctx, chatKey); err == nil && len(pending) > 0 {
			return fmt.Sprintf("Already waiting for %s to approve #%d. Don't ask for anything else until they have; tell the person you're waiting on %s.", a.principal(), pending[0].ID, a.principal()), true
		}
		summary = fmt.Sprintf("for %q (not you): %s", who, summary)
	}
	id, err := a.store.CreateApproval(ctx, chatKey, name, input, summary, risk)
	if err != nil {
		return "could not queue approval: " + err.Error(), true
	}
	a.store.Audit(ctx, "approval.requested", chatKey, fmt.Sprintf("#%d %s", id, auditSummary(name, summary, input)))
	if forStranger {
		// Before anyone hears of it: a decision from the screen the moment
		// it shows must still carry it out as that person's turn, and the
		// daemon must know from the start that only "yes N" decides it.
		if err := a.store.Set(ctx, strangerApprovalKey(id), who); err != nil {
			a.log.Warn("remember approval requester", "err", err)
		}
	}
	stored, err := a.store.GetApproval(ctx, id)
	if err != nil { // it was written a moment ago; describe it as it was asked
		stored = &memory.Approval{ID: id, ChatKey: chatKey, Tool: name, Input: input, Summary: summary, Status: "pending", CreatedAt: time.Now(), Risk: risk}
	}
	a.emit(ctx, *stored, "pending", "", "")
	switch {
	case forStranger:
		if a.StrangerAsked != nil {
			a.StrangerAsked(ctx, memory.Approval{ID: id, ChatKey: chatKey, Tool: name, Input: input, Summary: asked, Status: "pending", CreatedAt: time.Now()}, who)
		}
		return fmt.Sprintf("PENDING_APPROVAL id=%d. Only %s can approve this, not the person you are talking to. Tell them you've asked and will come back to them. Do not proceed as if it happened.", id, a.principal()), false
	case onCall(chatKey):
		// The person on the line is not the user and can't approve anything.
		return fmt.Sprintf("PENDING_APPROVAL id=%d: %s. Only %s can approve this, and they are not on this call. Tell the person on the line you'll need to check and come back to them; don't ask them to say yes. Do not proceed as if it happened.", id, firstLine(summary), a.principal()), false
	case risk >= tools.RiskDangerous && strings.HasPrefix(chatKey, "voice:"):
		// Anyone nearby can say yes, so a spoken "yes N" to this is refused
		// (daemon/voicehooks.go refuseAloud): don't ask for one.
		return fmt.Sprintf("PENDING_APPROVAL id=%d: %s. This can't be approved out loud, since anyone nearby could say yes. In one short sentence, say what you want to do and that it needs their own yes: they can approve number %d on the presence screen, or say yes and you'll send it to their phone to approve there. Don't ask them to say \"yes %d\". Do not proceed as if it happened.", id, firstLine(summary), id, id), false
	case detailed && strings.HasPrefix(chatKey, "voice:"):
		// A whole script or prompt is no good read aloud.
		return fmt.Sprintf("PENDING_APPROVAL id=%d: %s. The full details are on the screen. Say in one short sentence what you want to do, that the details are on the screen, and that they can say \"yes %d\" or \"no %d\". Do not proceed as if it happened.", id, firstLine(summary), id, id), false
	case detailed:
		return fmt.Sprintf("PENDING_APPROVAL id=%d:\n%s\n\nShow the user these details as written, then tell them they can reply \"yes %d\" or \"no %d\". Do not proceed as if it happened.", id, summary, id, id), false
	}
	return fmt.Sprintf("PENDING_APPROVAL id=%d: %s. Tell the user what you intend to do and that they can reply \"yes %d\" or \"no %d\". Do not proceed as if it happened.", id, summary, id, id), false
}

// summarize describes a call for its approval when the tool doesn't write
// its own. Values are shown whole, so what the owner says yes to is what
// will run; only something enormous (a pasted file) is cut, and then the
// text says how much is missing.
func summarize(tool string, input json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil || len(m) == 0 {
		return tool
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := []rune(fmt.Sprint(m[k]))
		if len(v) > maxApprovalValue {
			v = append(v[:maxApprovalValue], []rune(fmt.Sprintf("…[%d more characters not shown]", len(v)-maxApprovalValue))...)
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, string(v)))
	}
	return fmt.Sprintf("%s(%s)", tool, strings.Join(parts, ", "))
}

// maxApprovalValue is where an argument is cut in an approval: far beyond
// any command, message or email a person would read.
const maxApprovalValue = 10_000

// onCall reports whether chatKey is a live phone call the twin is making for
// the user (daemon/phone.go): the other party is on the line, not the user.
func onCall(chatKey string) bool { return strings.HasPrefix(chatKey, "voice:phone#call-") }

// firstLine is the first line of s, for saying it aloud.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
