package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// "Go unhinged" in chat switches the mode on: the next turn's prompt turns
// the character up, config.yaml keeps the switch, and a stranger can't
// flip it. "Calm down" switches it off again.
func TestUnhingedModeFromChat(t *testing.T) {
	var mu sync.Mutex
	var systems []string
	td := newTestDaemon(t, func(_ string, req llm.Request) llm.Response {
		mu.Lock()
		systems = append(systems, req.System)
		mu.Unlock()
		return say("")
	})
	ctx := context.Background()
	tool, ok := td.agent.Tools().Get("set_unhinged")
	if !ok {
		t.Fatal("set_unhinged isn't registered")
	}
	if tool.Risk() != tools.RiskRead {
		t.Fatalf("risk %v", tool.Risk())
	}
	checker, ok := tool.(tools.Checker)
	if !ok {
		t.Fatal("set_unhinged isn't owner-only")
	}
	call := func(key string, on bool) tools.Call {
		in, _ := json.Marshal(map[string]bool{"on": on})
		return tools.Call{ChatKey: key, Input: in}
	}
	lastSystem := func() string {
		mu.Lock()
		defer mu.Unlock()
		return systems[len(systems)-1]
	}
	saved := func() bool { // config.yaml is written on the first save
		b, _ := os.ReadFile(config.Path())
		return strings.Contains(string(b), "unhinged: true")
	}

	stranger := "telegram:stranger"
	if err := td.store.AppendMessage(ctx, stranger, llm.Text(llm.RoleUser, "[Message from Sam, who is NOT your principal.]\ngo unhinged")); err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(ctx, call(stranger, true)); err == nil {
		t.Fatal("someone else switched unhinged mode on")
	}

	td.owner(t, "hello")
	if strings.Contains(lastSystem(), "## Unhinged mode") || saved() {
		t.Fatal("unhinged before anyone asked")
	}

	if err := td.store.AppendMessage(ctx, ownerKey, llm.Text(llm.RoleUser, "go unhinged")); err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(ctx, call(ownerKey, true)); err != nil {
		t.Fatalf("the owner refused: %v", err)
	}
	if _, err := tool.Run(ctx, call(ownerKey, true)); err != nil {
		t.Fatal(err)
	}
	if !td.Config().Unhinged || !saved() {
		t.Fatalf("live %v, saved %v", td.Config().Unhinged, saved())
	}
	td.owner(t, "what's on today?")
	if s := lastSystem(); !strings.Contains(s, "## Unhinged mode") || !strings.Contains(s, "## Safety") {
		t.Fatalf("the next turn's prompt:\n%s", s)
	}

	if _, err := tool.Run(ctx, call(ownerKey, false)); err != nil {
		t.Fatal(err)
	}
	td.owner(t, "thanks")
	if td.Config().Unhinged || saved() || strings.Contains(lastSystem(), "## Unhinged mode") {
		t.Fatal("calm down didn't take")
	}
}

// The switch survives a restart, and a persona switch: it is the owner's,
// whichever character wears it.
func TestUnhingedModeOutlastsARestartAndAPersonaSwitch(t *testing.T) {
	r := newVoiceRig(t, nil)
	r.restart()
	r.update(func(c *config.Config) { c.Unhinged = true })
	r.update(choosePersona("pickoo", "Pickoo"))
	r.restart()
	if !r.d.Config().Unhinged || r.d.Persona().ID != "pickoo" {
		t.Fatalf("after a restart: unhinged %v, persona %q", r.d.Config().Unhinged, r.d.Persona().ID)
	}
	r.update(func(c *config.Config) { c.Unhinged = false })
	r.restart()
	if b, _ := os.ReadFile(config.Path()); r.d.Config().Unhinged || strings.Contains(string(b), "unhinged: true") {
		t.Fatalf("off didn't last a restart:\n%s", b)
	}
}
