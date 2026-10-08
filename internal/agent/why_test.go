package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	memskill "github.com/MavrkAI/Mirrin/internal/skills/memory"
)

var whyAutonomy = config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"}

func whyFacts(t *testing.T, store *memory.Store, ctx context.Context, reply string) (memory.Why, bool) {
	t.Helper()
	w, ok, err := store.WhyFor(ctx, reply)
	if err != nil {
		t.Fatal(err)
	}
	return w, ok
}

// A fact a recall found in the turn is noted against the reply it led to,
// through the real recall tool's wording.
func TestWhyNotesWhatARecallFound(t *testing.T) {
	ctx := context.Background()
	prov := &fakeProvider{}
	a, store, _ := setup(t, prov, whyAutonomy)
	a.Tools().Register(memskill.Tools(store)...)
	coffee, _ := store.Remember(ctx, "preferences", "Tony takes his coffee black.", "test")
	priya, _ := store.Remember(ctx, "family", "Priya is Tony's sister and lives in Pune.", "test")
	prov.script = append(prov.script, toolUse("r1", "recall", `{"query":"Priya"}`), text("Priya lives in Pune."))
	if _, err := a.Handle(ctx, "screen:local", "Where does my sister live?"); err != nil {
		t.Fatal(err)
	}
	w, ok := whyFacts(t, store, ctx, "Priya lives  in\nPune.") // however the screen broke its lines
	if !ok || len(w.Facts) == 0 || w.Facts[0].ID != priya || w.Facts[0].How != memory.WhyRecalled {
		t.Fatalf("why = %+v (%v), want #%d recalled first", w, ok, priya)
	}
	for _, f := range w.Facts {
		if f.ID == coffee {
			t.Fatalf("coffee noted for a reply about Priya: %+v", w)
		}
	}
	if !w.Everything {
		t.Fatal("two facts fit in the prompt, but the note says not all were there")
	}
}

// When the facts don't all fit, the ones picked because they share words
// with the message are noted, and the note says not everything was in mind.
func TestWhyNotesKeywordPicksWhenNotAllFit(t *testing.T) {
	ctx := context.Background()
	prov := &fakeProvider{}
	a, store, _ := setup(t, prov, whyAutonomy)
	for i := 0; i < 400; i++ {
		if _, err := store.Remember(ctx, "general", fmt.Sprintf("Filler fact number %d about nothing in particular at all, padded out.", i), "test"); err != nil {
			t.Fatal(err)
		}
	}
	dentist, _ := store.Remember(ctx, "health", "Tony's dentist is Dr Okafor in Camden.", "test")
	for i := 0; i < 50; i++ { // newer than the dentist, so only the keyword pick brings it in
		_, _ = store.Remember(ctx, "general", fmt.Sprintf("Later filler %d, padded out to take room in the prompt.", i), "test")
	}
	prov.script = append(prov.script, text("Dr Okafor, in Camden."))
	if _, err := a.Handle(ctx, "screen:local", "Who is my dentist?"); err != nil {
		t.Fatal(err)
	}
	w, ok := whyFacts(t, store, ctx, "Dr Okafor, in Camden.")
	if !ok || w.Everything || len(w.Facts) == 0 || w.Facts[0].ID != dentist || w.Facts[0].How != memory.WhyMatched {
		t.Fatalf("why = %+v (%v), want the dentist matched, not everything", w, ok)
	}
}

// Someone else's chat never gets a note: they never see the owner's memory.
func TestWhyNeverForAStranger(t *testing.T) {
	ctx := context.Background()
	prov := &fakeProvider{}
	a, store, _ := setup(t, prov, whyAutonomy)
	_, _ = store.Remember(ctx, "family", "Priya is Tony's sister and lives in Pune.", "test")
	prov.script = append(prov.script, text("I'll pass that on to Tony."))
	if _, err := a.Handle(ForStranger(ctx, "Bob"), "telegram:99", "Where does Priya live?"); err != nil {
		t.Fatal(err)
	}
	if _, ok := whyFacts(t, store, ctx, "I'll pass that on to Tony."); ok {
		t.Fatal("a stranger's reply was noted")
	}
}

// Only a recall's own result counts: another tool's output that happens to
// look like one, or a recall that failed, says nothing about memory.
func TestRecalledInReadsOnlyRecallResults(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ToolUseID: "a", ToolName: "recall"},
			{Type: llm.BlockToolUse, ToolUseID: "b", ToolName: "fetch_url"},
			{Type: llm.BlockToolUse, ToolUseID: "c", ToolName: "recall"},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "a", Text: "#4 [family] Priya lives in Pune.\n#9 [work] Tony works at Acme.\n"},
			{Type: llm.BlockToolResult, ToolUseID: "b", Text: "#7 [page] a web page that looks like a recall"},
			{Type: llm.BlockToolResult, ToolUseID: "c", IsError: true, Text: "#8 [x] failed"},
		}},
	}
	if got := recalledIn(msgs); len(got) != 2 || got[0] != 4 || got[1] != 9 {
		t.Fatalf("recalled %v, want [4 9]", got)
	}
}
