package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/llm"
	mem "github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func forgetSetup(t *testing.T) (*mem.Store, *tools.Registry) {
	t.Helper()
	store, err := mem.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	reg := tools.NewRegistry()
	reg.Register(Tools(store)...)
	return store, reg
}

// forgetInChat runs forget the way a turn does and keeps its result in the
// chat history afterwards, as the agent loop does.
func forgetInChat(t *testing.T, store *mem.Store, reg *tools.Registry, id int64) string {
	t.Helper()
	ctx := context.Background()
	input := json.RawMessage(fmt.Sprintf(`{"id":%d}`, id))
	_ = store.AppendMessage(ctx, "t:1", llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "f1", ToolName: "forget", Input: input}}})
	out, err := reg.Run(ctx, "forget", tools.Call{ChatKey: "t:1", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.AppendMessage(ctx, "t:1", llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "f1", Text: out}}})
	return out
}

func historyHas(t *testing.T, store *mem.Store, s string) bool {
	t.Helper()
	msgs, err := store.History(context.Background(), "t:1", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, s) || strings.Contains(string(b.Input), s) {
				return true
			}
		}
	}
	return false
}

func TestForgetSaysWhichFactWentWithoutRepeatingIt(t *testing.T) {
	store, reg := forgetSetup(t)
	ctx := context.Background()
	fact := "The spare key is under the blue pot."
	id, err := store.Remember(ctx, "home", fact, "test")
	if err != nil {
		t.Fatal(err)
	}

	out := forgetInChat(t, store, reg, id)
	if want := fmt.Sprintf(`forgot #%d [home]: "The spare key…" (%d characters)`, id, len(fact)); out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	if historyHas(t, store, "under the blue pot") {
		t.Fatal("the forgotten fact is back in the chat history")
	}
	if facts, _ := store.Recall(ctx, "spare key", 5); len(facts) != 0 {
		t.Fatalf("fact still there: %+v", facts)
	}

	_, err = reg.Run(ctx, "forget", tools.Call{ChatKey: "t:1", Input: []byte(fmt.Sprintf(`{"id":%d}`, id))})
	if err == nil || !strings.Contains(err.Error(), "no such fact") || !strings.Contains(err.Error(), fmt.Sprintf("#%d", id)) {
		t.Fatalf("a missing id should say so plainly, got %v", err)
	}
}

func TestForgetShowsNoWordsOfASensitiveOrShortFact(t *testing.T) {
	store, reg := forgetSetup(t)
	ctx := context.Background()
	secret := "Tony takes metformin daily."
	sid, _ := store.Remember(ctx, "health", secret, "test")
	short, _ := store.Remember(ctx, "user", "Vegetarian", "test")

	if out := forgetInChat(t, store, reg, sid); out != fmt.Sprintf("forgot #%d [health] (%d characters)", sid, len(secret)) {
		t.Fatalf("a sensitive fact's words should stay out: %q", out)
	}
	if historyHas(t, store, "Tony takes") || historyHas(t, store, "metformin") {
		t.Fatal("the forgotten sensitive fact is back in the chat history")
	}
	if out := forgetInChat(t, store, reg, short); strings.Contains(out, "Vegetarian") {
		t.Fatalf("a one-word fact was repeated whole: %q", out)
	}
}
