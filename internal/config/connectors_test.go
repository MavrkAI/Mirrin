package config

import (
	"strings"
	"testing"
)

// Signal can run on a number of its own (register, verify) or as a device
// linked to the owner's phone; the steps offer both.
func TestSignalStepsOfferLinking(t *testing.T) {
	k, ok := ConnectorByName("signal")
	if !ok {
		t.Fatal("no signal connector")
	}
	var register, link, noteToSelf bool
	for _, s := range k.Steps {
		register = register || (strings.Contains(s.Text, "register") && strings.Contains(s.Text, "verify"))
		link = link || strings.Contains(s.Text, "signal-cli link -n mirrin")
		noteToSelf = noteToSelf || strings.Contains(s.Text, "both to your own number")
	}
	if !register || !link || !noteToSelf {
		t.Fatalf("register step %v, link step %v, own number %v: %+v", register, link, noteToSelf, k.Steps)
	}
}
