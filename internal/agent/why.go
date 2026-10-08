package agent

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// "Why did you say that?" When a turn for the owner ends, the twin keeps a
// note of the facts its reply drew on, so the screen can show them with a
// Forget beside each: what a recall in the turn found, and what was picked
// for the prompt as related to the message. Nobody else's turn is noted:
// they never see the owner's memory, so nothing of it went into the reply.

// reRecalled finds the facts in a recall result ("#12 [family] …").
var reRecalled = regexp.MustCompile(`(?m)^#(\d+) \[`)

// noteWhy keeps the note for a finished turn's reply.
func (a *Agent) noteWhy(ctx context.Context, chatKey, reply string) {
	if _, ok := stranger(ctx); ok || strings.TrimSpace(reply) == "" || strings.Contains(reply, "NOTHING_TO_REPORT") {
		return
	}
	ctx = context.WithoutCancel(ctx) // a turn stopped as it finished still said this
	msgs, last, err := a.store.LastTurn(ctx, chatKey)
	if err != nil || last == 0 {
		return
	}
	n := memory.WhyNote{MessageID: last, ChatKey: chatKey, Reply: reply, Recalled: recalledIn(msgs)}
	if a.facts.ok {
		n.Everything, n.Matched = a.facts.everything, a.facts.matched
	}
	if err := a.store.NoteWhy(ctx, n); err != nil {
		a.log.Warn("why note", "chat", chatKey, "err", err)
	}
}

// recalledIn lists the facts recall calls in a turn brought back, in the
// order they came.
func recalledIn(msgs []llm.Message) []int64 {
	recalls := map[string]bool{}
	var out []int64
	seen := map[int64]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockToolUse && b.ToolName == "recall":
				recalls[b.ToolUseID] = true
			case b.Type == llm.BlockToolResult && recalls[b.ToolUseID] && !b.IsError:
				for _, sm := range reRecalled.FindAllStringSubmatch(b.Text, -1) {
					if id, err := strconv.ParseInt(sm[1], 10, 64); err == nil && !seen[id] {
						seen[id] = true
						out = append(out, id)
					}
				}
			}
		}
	}
	return out
}
