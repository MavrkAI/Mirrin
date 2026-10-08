package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// SetConfig swaps the configuration. Turns already under way finish with the
// settings they started with; the next turn picks up the new ones.
func (a *Agent) SetConfig(c config.Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = &c
}

// turn is the agent as one run sees it: config, model and persona are fixed
// when the turn starts, so a change made from the tray mid-turn can't land
// half-way through a tool loop. The approvals policy is the exception: every
// tool call is checked against the live one (see gate). query is the message
// the turn answers.
func (a *Agent) turn(query string) *Agent {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return &Agent{
		cfg: a.cfg, provider: a.provider, store: a.store, tools: a.tools, log: a.log,
		MaxIterations: a.MaxIterations, RuntimeInfo: a.RuntimeInfo, Budgets: a.Budgets, StrangerAsked: a.StrangerAsked,
		Browser: a.Browser, Tasks: a.Tasks, CheckApproved: a.CheckApproved, voiceProvider: a.voiceProvider, persona: a.persona,
		root: a, query: query,
	}
}

// gate is the approvals policy a tool call is checked against: always the
// live one, so turning autonomy down from the tray stops a turn (or a long
// background task) that is already under way at its next tool call.
func (a *Agent) gate() *approvals.Policy {
	live := a
	if a.root != nil {
		live = a.root
	}
	live.mu.RLock()
	defer live.mu.RUnlock()
	return live.policy
}

// StepLimitError means a background task used every step it was allowed
// before finishing. Its text is the model's account of what got done and
// what is left.
type StepLimitError struct{ Reply string }

func (e *StepLimitError) Error() string { return e.Reply }

// OutOfSteps lets the task manager recognise the condition without importing
// this package.
func (e *StepLimitError) OutOfSteps() bool { return true }

// stepLimitResult answers a tool call made after the turn's steps ran out, so
// the model spends its last call saying where things stand.
func stepLimitResult(id string) llm.Block {
	return llm.Block{Type: llm.BlockToolResult, ToolUseID: id, IsError: true,
		Text: "Not run: this request has used all the steps it is allowed. Don't call any more tools. In two or three short sentences, tell the user what you got done, what is still left, and offer to carry on."}
}

// reTask is a background task's suffix ("telegram:1#task-09271", perhaps
// followed by a run of its own), not an IRC room whose name merely starts
// with it ("irc:#task-force|tony").
var reTask = regexp.MustCompile(`[^:]#task-\d+(?:#|$)`)

// inTask reports whether chatKey is a background task's conversation: it
// gets the task's step budget and pauses, rather than ends, when out of steps.
func inTask(chatKey string) bool { return reTask.MatchString(chatKey) }

// reply is the text a turn ends with. It never claims "Done." for work that
// didn't finish: running out of steps says so, and inside a background task
// it also returns a StepLimitError so the task pauses rather than closing.
func (a *Agent) reply(chatKey, out string, stop llm.StopReason, outOfSteps bool) (string, error) {
	if outOfSteps {
		a.store.Audit(context.Background(), "agent.out_of_steps", chatKey, truncate(out, 300))
		if inTask(chatKey) {
			if out == "" {
				out = "The board shows what's done and what's left."
			}
			return out, &StepLimitError{Reply: out}
		}
		if out == "" {
			out = "I ran out of steps before I could finish this. Say \"carry on\" and I'll pick up where I left off."
		}
		return out, nil
	}
	if out != "" {
		return out, nil
	}
	switch stop {
	case llm.StopMaxTokens:
		return "My answer ran past the length limit before I got to the point. Ask me for a shorter version, or one part at a time.", nil
	case llm.StopRefusal:
		return "I can't help with that one.", nil
	}
	return "Done.", nil
}

// memoryBlock lists what the twin remembers for this turn: every fact while
// they fit in factBudget, and past that the ones related to the message plus
// the newest, with a note so the model knows to search for the rest. A turn
// works it out once and again only after a fact is added or removed.
func (a *Agent) memoryBlock(ctx context.Context) string {
	v := a.store.FactsVersion()
	if a.root != nil && a.facts.ok && a.facts.version == v {
		return a.facts.block
	}
	p, err := a.store.PickFacts(ctx, a.query, factBudget)
	if err != nil {
		return ""
	}
	block := factsBlock(p)
	if a.root != nil { // only a turn's own copy; the live agent is shared
		a.facts = factsCache{version: v, block: block, ok: true, matched: p.Matched, everything: p.Everything}
	}
	return block
}

// factsCache is a turn's memory block and the facts version it was made from,
// with which facts matched the message (for "Why?", why.go).
type factsCache struct {
	version    int64
	block      string
	ok         bool
	matched    []int64
	everything bool
}

func factsBlock(p memory.Picked) string {
	facts, total := p.Facts, p.Total
	if len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nWhat you remember (id: subject: fact; higher ids are newer, and a newer fact wins over an older one it contradicts):\n")
	for _, f := range facts {
		fmt.Fprintf(&b, "- %d: %s: %s\n", f.ID, f.Subject, f.Content)
	}
	if len(facts) < total {
		fmt.Fprintf(&b, "(That is %d of the %d facts you hold: the ones that look related to this message, then the newest. Use recall to search the rest before saying you don't know something.)\n", len(facts), total)
	}
	return b.String()
}

// factBudget is how much of the prompt remembered facts may take, in
// characters (about 3,000 tokens).
const factBudget = 12000
