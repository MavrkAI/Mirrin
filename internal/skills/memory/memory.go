// Package memory exposes long-term memory as tools.
package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	mem "github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Tools returns remember / recall / forget.
func Tools(store *mem.Store) []tools.Tool {
	return []tools.Tool{
		tools.New("remember",
			"Store a durable fact about the user or their world (a preference, a person, a routine, a goal, a deadline). Use whenever the user tells you something worth keeping. Sensitive subjects (health, finance, relationships, secrets) ask the user first.",
			tools.Schema(map[string]tools.Prop{
				"subject": {Type: "string", Description: "Short category: user, family, work, health, home, finance, travel, preferences, people, goals, relationships, secrets", Required: true},
				"fact":    {Type: "string", Description: "The fact, written as a complete standalone sentence", Required: true},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Subject, Fact string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if strings.TrimSpace(in.Fact) == "" {
					return "", fmt.Errorf("fact is empty")
				}
				if Sensitive(in.Subject) {
					return "", fmt.Errorf("that is a sensitive subject: use remember_sensitive so the user approves it first")
				}
				id, err := store.Remember(ctx, in.Subject, in.Fact, call.ChatKey)
				if err != nil {
					return "", err
				}
				// Say it's new: a reply once claimed "I already had that one"
				// about a fact it had just learned.
				return fmt.Sprintf("remembered (#%d), new: you didn't know this before", id), nil
			}),
		tools.New("remember_sensitive",
			"Store a sensitive fact (health, finance, relationships, secrets). Goes through the user's approval so nothing private is kept without a yes.",
			tools.Schema(map[string]tools.Prop{
				"subject": {Type: "string", Description: "health, finance, relationships or secrets", Required: true, Enum: []string{"health", "finance", "relationships", "secrets"}},
				"fact":    {Type: "string", Description: "The fact, as a complete sentence", Required: true},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Subject, Fact string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				id, err := store.Remember(ctx, in.Subject, in.Fact, call.ChatKey)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("remembered (#%d, sensitive)", id), nil
			}),
		tools.New("recall",
			"Search long-term memory by keywords. Use before assuming you don't know something.",
			tools.Schema(map[string]tools.Prop{
				"query": {Type: "string", Description: "Keywords to search for", Required: true},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Query string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				facts, err := store.Recall(ctx, in.Query, 25)
				if err != nil {
					return "", err
				}
				if len(facts) == 0 {
					return "nothing in memory matches", nil
				}
				var b strings.Builder
				for _, f := range facts {
					fmt.Fprintf(&b, "#%d [%s] %s\n", f.ID, f.Subject, f.Content)
				}
				return b.String(), nil
			}),
		tools.New("forget",
			"Delete a fact from memory by its id (shown as #id).",
			tools.Schema(map[string]tools.Prop{
				"id": {Type: "integer", Description: "Fact id", Required: true},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ ID int64 }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				f, err := store.ForgetFact(ctx, in.ID)
				if errors.Is(err, mem.ErrNoFact) {
					return "", fmt.Errorf("no such fact: #%d isn't in memory (it may be forgotten already). Use recall to find the right id", in.ID)
				}
				if err != nil {
					return "", err
				}
				return forgotLine(f), nil
			}),
	}
}

// Sensitive reports whether subject is one remember_sensitive guards
// (health, finance, relationships, secrets). What it covers is never shown
// on a screen as just learned, nor undone from one (daemon/remembered.go).
func Sensitive(subject string) bool {
	switch strings.ToLower(strings.TrimSpace(subject)) {
	case "health", "finance", "relationships", "secrets", "medical", "money":
		return true
	}
	return false
}

// forgotLine says which fact went without repeating it. The result is kept
// in the chat history after ForgetFact has scrubbed the fact's wording from
// it, so the whole text would bring the fact straight back. It shows the
// first few words (never all of them), and none for a sensitive subject.
func forgotLine(f mem.Fact) string {
	n := len([]rune(f.Content))
	if Sensitive(f.Subject) {
		return fmt.Sprintf("forgot #%d [%s] (%d characters)", f.ID, f.Subject, n)
	}
	words := strings.Fields(f.Content)
	k := min(3, len(words)-1) // never the whole fact, however short
	if k < 1 {
		return fmt.Sprintf("forgot #%d [%s] (%d characters)", f.ID, f.Subject, n)
	}
	preview := strings.Join(words[:k], " ")
	if r := []rune(preview); len(r) > 30 {
		preview = string(r[:30])
	}
	preview += "…"
	return fmt.Sprintf("forgot #%d [%s]: %q (%d characters)", f.ID, f.Subject, preview, n)
}
