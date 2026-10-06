//go:build !nowhatsapp

package whatsapp

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/store/sqlstore"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Each failed attempt is a fresh instance; closing it must release the
// store it opened, or retrying while offline leaks a handle per attempt.
func TestCloseReleasesTheStore(t *testing.T) {
	c := New(t.TempDir(), "+61400000002", false, nil)
	if err := c.Start(context.Background(), func(context.Context, channels.Inbound) {}); err == nil {
		t.Fatal("an unpaired channel should not start")
	}
	if c.client == nil {
		t.Fatal("Start should have opened the store")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.client.Store.Container.(*sqlstore.Container).GetFirstDevice(context.Background()); err == nil {
		t.Fatal("the store is still open after Close")
	}
	if err := New(t.TempDir(), "", false, nil).Close(); err != nil {
		t.Fatalf("closing an instance that never started: %v", err)
	}
}
