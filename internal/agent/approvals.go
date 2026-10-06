package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// An approval's life, all of it here so every step reaches OnApproval once:
// ask (asking.go) raises it; DecideBy records the owner's yes or no; Settle
// records an outcome the twin reaches itself (a newer request replaced it,
// it lapsed); Carry and Perform act on a decision; Revise marks an approved
// call that never ran.

// ResolveApproval runs or rejects a pending tool call and lets the model continue.
func (a *Agent) ResolveApproval(ctx context.Context, chatKey string, id int64, approved bool) (string, error) {
	return a.resolveApproval(ctx, chatKey, id, approved, nil)
}

// ResolveApprovalStreaming is ResolveApproval with streamed text.
func (a *Agent) ResolveApprovalStreaming(ctx context.Context, chatKey string, id int64, approved bool, onDelta func(string)) (string, error) {
	return a.resolveApproval(ctx, chatKey, id, approved, onDelta)
}

func (a *Agent) resolveApproval(ctx context.Context, chatKey string, id int64, approved bool, onDelta func(string)) (string, error) {
	ap, err := a.Decide(ctx, chatKey, id, approved)
	if err != nil {
		return "", err
	}
	return a.Carry(ctx, ap, onDelta)
}

// Decide records the owner's decision on a pending approval raised in
// chatKey. It is the claim: of two decisions racing for one approval only
// the first gets through, so a call never runs twice. Carry acts on it.
func (a *Agent) Decide(ctx context.Context, chatKey string, id int64, approved bool) (*memory.Approval, error) {
	return a.DecideBy(ctx, chatKey, id, approved, "")
}

// DecideBy is Decide that records who decided and how (by, "the owner
// (channel, telegram)"), for the audit log and for OnApproval.
func (a *Agent) DecideBy(ctx context.Context, chatKey string, id int64, approved bool, by string) (*memory.Approval, error) {
	ap, err := a.store.GetApproval(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("no approval #%d", id)
	}
	if ap.ChatKey != chatKey {
		return nil, fmt.Errorf("approval #%d belongs to another conversation", id)
	}
	status, audit := "denied", "approval.denied"
	if approved {
		status, audit = "approved", "approval.granted"
	}
	if err := a.store.ResolveApprovalBy(ctx, id, status, by); err != nil {
		if cur, gerr := a.store.GetApproval(ctx, id); gerr == nil && cur.Status != "pending" {
			return nil, fmt.Errorf("approval #%d is already %s", id, cur.Status)
		}
		return nil, err
	}
	ap.Status, ap.DecidedBy = status, by
	detail := fmt.Sprintf("#%d %s", id, ap.Tool)
	if by != "" {
		detail = fmt.Sprintf("#%d by %s: %s", id, by, ap.Tool)
	}
	a.store.Audit(ctx, audit, chatKey, detail)
	a.emit(ctx, *ap, status, by, "")
	return ap, nil
}

// Settle gives a pending approval an outcome nobody chose: "superseded" (the
// same call was asked again) or "expired" (it waited too long). why says so
// in the audit log and to OnApproval. It fails if the approval already has
// an outcome.
func (a *Agent) Settle(ctx context.Context, ap *memory.Approval, status, why string) error {
	if err := a.store.ResolveApprovalBy(ctx, ap.ID, status, ""); err != nil {
		return err
	}
	ap.Status = status
	a.store.Audit(ctx, "approval."+status, ap.ChatKey, fmt.Sprintf("#%d %s: %s", ap.ID, ap.Tool, why))
	a.emit(ctx, *ap, status, "", why)
	return nil
}

// Revise changes an outcome already given, only if it is still from: an
// approved call that was never run becomes "expired". It reports whether it
// changed anything.
func (a *Agent) Revise(ctx context.Context, ap *memory.Approval, from, to, why string) bool {
	if a.store.ReviseApproval(ctx, ap.ID, from, to) != nil {
		return false
	}
	ap.Status = to
	a.store.Audit(ctx, "approval."+to, ap.ChatKey, fmt.Sprintf("#%d %s: %s", ap.ID, ap.Tool, why))
	a.emit(ctx, *ap, to, ap.DecidedBy, why)
	return true
}

// Carry acts on a decided approval in its conversation: it runs the approved
// call, or tells the model it was turned down, and lets the model continue.
// An approved call that no longer fits (its input changed since it was
// asked, or CheckApproved objects) is not run, and its approval is marked
// expired. Inside a background task the model is told to carry on with the
// task, not to wrap up: one step's outcome is not the task's.
func (a *Agent) Carry(ctx context.Context, ap *memory.Approval, onDelta func(string)) (string, error) {
	chatKey, id := ap.ChatKey, ap.ID
	ctx = a.withApprovalRequester(ctx, id) // a stranger's request stays their turn
	task := inTask(chatKey)
	switch ap.Status {
	case "denied":
		msg := fmt.Sprintf("[System: the user denied approval #%d for %s. Acknowledge briefly and do not retry it.]", id, ap.Tool)
		if task {
			msg = fmt.Sprintf("[System: the user denied approval #%d for %s. Don't retry it. %s]", id, ap.Tool, carryOnWithout)
		}
		return a.run(ctx, chatKey, llm.Text(llm.RoleUser, msg), onDelta)
	case "approved":
	default:
		return "", fmt.Errorf("approval #%d is %s", id, ap.Status)
	}
	result, status, notRun := a.perform(ctx, ap)
	if notRun != nil {
		msg := fmt.Sprintf("[System: the user approved #%d, but %s was NOT run: %s. Tell the user in one or two lines, and offer to look again and ask afresh.]", id, ap.Tool, notRun)
		if task {
			msg = fmt.Sprintf("[System: the user approved #%d, but %s was NOT run: %s. Look again and ask afresh if it is still needed. %s]", id, ap.Tool, notRun, carryOn)
		}
		return a.run(ctx, chatKey, llm.Text(llm.RoleUser, msg), onDelta)
	}
	next := "Tell the user the outcome in one or two lines."
	if task {
		next = "Tick the step this finished with task_update step_done. " + carryOn
	}
	msg := fmt.Sprintf("[System: the user approved #%d. %s was executed and %s. Result:\n%s\n\n%s]", id, ap.Tool, status, truncate(result, 8000), next)
	return a.run(ctx, chatKey, llm.Text(llm.RoleUser, msg), onDelta)
}

// What a background task is told after one of its steps is decided.
const (
	carryOn        = "Then carry on with the task from your board: call task_update finish once the goal is met, or ask_user if you need the user."
	carryOnWithout = "Carry on with the task from your board without it: call task_update finish once the goal is met (or can't be), or ask_user if you need the user."
)

// Perform acts on an approval decided in the very turn that is running in
// its conversation (the owner answered in their own words and the model
// settled it with resolve_approval): it runs an approved call there and
// then, and returns what that turn's model is told, as a tool result.
func (a *Agent) Perform(ctx context.Context, ap *memory.Approval) string {
	switch ap.Status {
	case "denied":
		return fmt.Sprintf("#%d is turned down: %s won't be done. Acknowledge it briefly and don't retry it.", ap.ID, ap.Tool)
	case "approved":
	default:
		return fmt.Sprintf("#%d is %s, so nothing was done.", ap.ID, ap.Status)
	}
	result, status, notRun := a.perform(ctx, ap)
	if notRun != nil {
		return fmt.Sprintf("The user approved #%d, but %s was NOT run: %s. Tell them in a line, and offer to look again and ask afresh.", ap.ID, ap.Tool, notRun)
	}
	return fmt.Sprintf("The user approved #%d. %s was executed and %s. Result:\n%s\n\nTell the user the outcome in a line or two.", ap.ID, ap.Tool, status, truncate(result, 8000))
}

// perform runs an approved call exactly as stored, if it still fits;
// notRun says why it wasn't run.
func (a *Agent) perform(ctx context.Context, ap *memory.Approval) (result, status string, notRun error) {
	if err := a.stillFits(ctx, *ap); err != nil {
		a.Revise(ctx, ap, "approved", "expired", err.Error())
		a.store.Audit(ctx, "tool.not_run", ap.ChatKey, fmt.Sprintf("#%d %s: %s", ap.ID, ap.Tool, err))
		return "", "", err
	}
	result, runErr := a.tools.Run(ctx, ap.Tool, tools.Call{ChatKey: ap.ChatKey, Input: ap.Input})
	status = "succeeded"
	if runErr != nil {
		status = "failed"
		result = runErr.Error()
	}
	a.store.Audit(ctx, "tool."+status, ap.ChatKey, fmt.Sprintf("%s %s", ap.Tool, auditInput(ap.Tool, ap.Input, result, 500)))
	if done := a.live().Performed; done != nil {
		done(ctx, *ap, runErr == nil)
	}
	return result, status, nil
}

// errChanged is why a request whose stored input no longer matches what the
// owner was asked about is not run.
var errChanged = errors.New("its details changed after it was asked (something in it has since been forgotten)")

// stillFits checks an approved call just before it runs.
func (a *Agent) stillFits(ctx context.Context, ap memory.Approval) error {
	if !ap.Intact() {
		return errChanged
	}
	if check := a.live().CheckApproved; check != nil {
		return check(ctx, ap)
	}
	return nil
}
