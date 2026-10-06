package tools

import (
	"context"
	"slices"
	"testing"
)

// keep_model was offered to the model with every request. A hidden tool is
// left out of Specs and Names, and Offered tells the agent to refuse it,
// while Get and Run still find it for the daemon.
func TestHiddenToolsAreNotOffered(t *testing.T) {
	r := NewRegistry()
	echo := func(name string) *Func {
		return New(name, "", nil, RiskRead, func(context.Context, Call) (string, error) { return name, nil })
	}
	r.Register(echo("seen"), echo("secret").Hide())
	if names := r.Names(); !slices.Equal(names, []string{"seen"}) {
		t.Fatalf("offered %v", names)
	}
	if !r.Offered("seen") || r.Offered("secret") || r.Offered("missing") {
		t.Fatal("Offered is true only for a registered tool that isn't hidden")
	}
	if _, ok := r.Get("secret"); !ok {
		t.Fatal("a hidden tool is still found by name")
	}
	if out, err := r.Run(context.Background(), "secret", Call{}); err != nil || out != "secret" {
		t.Fatalf("a hidden tool still runs for the daemon: %q %v", out, err)
	}
	r.Register(echo("secret")) // registered plainly again: offered again
	if len(r.Specs()) != 2 || !r.Offered("secret") {
		t.Fatalf("specs %v", r.Names())
	}
}
