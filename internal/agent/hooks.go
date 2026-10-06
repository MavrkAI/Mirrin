package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// ApprovalEvent is one step in an approval's life: raised (pending), then
// exactly one outcome (approved, denied, superseded or expired), and at most
// one revision after that (an approved call that no longer fitted, and so
// was not run, becomes expired).
type ApprovalEvent struct {
	ID      int64
	ChatKey string
	Tool    string
	Summary string
	Risk    tools.Risk
	Status  string // pending, approved, denied, superseded, expired
	// By says who decided and how ("Akshay's iPhone [d1] (passkey, relay
	// r2, 203.0.113.9)"); empty when it was raised, and when the twin settled
	// it itself (a newer request replaced it, it lapsed).
	By string
	// Why is the twin's reason for superseding or expiring it.
	Why string
	// Input is the call as stored. A notification must not show it: the
	// summary is what the owner reads.
	Input     json.RawMessage
	InputHash string
	CreatedAt time.Time
	At        time.Time
}

// approvalHooks are the functions that hear about approvals.
type approvalHooks struct {
	mu sync.Mutex
	fs []func(ctx context.Context, e ApprovalEvent)
}

// OnApproval adds f to what hears about every approval as it is raised and
// as it is settled, once per step, synchronously and in order, straight from
// where it happens: never through the events bus, which drops events for a
// slow listener. f runs on the turn or request that made the change, so it
// must return quickly; anything slow or that can fail (a push to a phone)
// goes through a RetryQueue.
func (a *Agent) OnApproval(f func(ctx context.Context, e ApprovalEvent)) {
	h := &a.live().hooks
	h.mu.Lock()
	h.fs = append(h.fs, f)
	h.mu.Unlock()
}

// live is the agent the tray changes: a turn's copy leads back to it.
func (a *Agent) live() *Agent {
	if a.root != nil {
		return a.root
	}
	return a
}

// emit tells the hooks about a step in ap's life.
func (a *Agent) emit(ctx context.Context, ap memory.Approval, status, by, why string) {
	h := &a.live().hooks
	h.mu.Lock()
	fs := append([]func(context.Context, ApprovalEvent){}, h.fs...)
	h.mu.Unlock()
	if len(fs) == 0 {
		return
	}
	hash := ap.InputHash
	if hash == "" {
		hash = memory.HashInput(ap.Input)
	}
	e := ApprovalEvent{ID: ap.ID, ChatKey: ap.ChatKey, Tool: ap.Tool, Summary: ap.Summary, Risk: ap.Risk, Status: status, By: by, Why: why,
		Input: ap.Input, InputHash: hash, CreatedAt: ap.CreatedAt, At: time.Now()}
	for _, f := range fs {
		f(ctx, e)
	}
}
