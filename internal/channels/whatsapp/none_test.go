//go:build nowhatsapp

package whatsapp

import (
	"context"
	"errors"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// A build without WhatsApp says so, and the daemon stops the channel rather
// than retrying it forever.
func TestWithoutWhatsAppEverythingSaysSo(t *testing.T) {
	if Built {
		t.Fatal("Built is true in a nowhatsapp build")
	}
	c := New(t.TempDir(), "+61 400 000 000", false, nil)
	err := c.Start(context.Background(), nil)
	if !channels.IsFatal(err) || !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("Start = %v, want a fatal ErrNotBuilt", err)
	}
	if _, err := c.StartPairing(context.Background(), "", ""); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("StartPairing = %v", err)
	}
	if got := c.OwnerChatID(); got != "61400000000@s.whatsapp.net" {
		t.Fatalf("OwnerChatID = %q", got)
	}
	if Paired(t.TempDir()) {
		t.Fatal("Paired is true")
	}
}
