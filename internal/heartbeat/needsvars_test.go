package heartbeat

import (
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/protocols"
)

func TestNeedsVars(t *testing.T) {
	p := protocols.Protocol{Name: "weather", Prompt: "Check the weather in {{city}} and {{ city }}.",
		Vars: map[string]protocols.Var{"city": {Description: "The city you live in."}}}
	if got := Unfilled(p); len(got) != 1 || got[0] != "city" {
		t.Fatalf("Unfilled = %v", got)
	}
	err := NeedsVars(p)
	if err == nil || !strings.Contains(err.Error(), "city (the city you live in)") {
		t.Fatalf("NeedsVars = %v", err)
	}
	p.Prompt = "Check the weather in Sydney."
	if err := NeedsVars(p); err != nil {
		t.Fatalf("a filled prompt needs nothing: %v", err)
	}
}

// A name in any script keeps its first letter whole in what the owner is
// told, and the message says the values need a /reload, since they are
// read when protocols load.
func TestNeedsVarsNameInAnyScript(t *testing.T) {
	for name, want := range map[string]string{
		"朝のまとめ":      "朝のまとめ can't run yet",
		"élan check": "Élan check can't run yet",
		"rain check": "Rain check can't run yet",
	} {
		p := protocols.Protocol{Name: name, Prompt: "Look up {{city}}.",
			Vars: map[string]protocols.Var{"city": {Description: "Échelle de la ville"}}}
		err := NeedsVars(p)
		if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "city (échelle de la ville)") || !strings.Contains(err.Error(), "say /reload") {
			t.Errorf("NeedsVars(%q) = %v", name, err)
		}
	}
	if got := capitalize(""); got != "" {
		t.Errorf("capitalize(\"\") = %q", got)
	}
}
