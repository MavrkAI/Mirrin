//go:build !darwin

package imessage

import (
	"context"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// An iMessage setting carried over to Linux or Windows fails once and says
// why, instead of "reconnecting" forever.
func TestOnlyOnMacOS(t *testing.T) {
	if err := New("", false, nil).Start(context.Background(), func(context.Context, channels.Inbound) {}); !channels.IsFatal(err) {
		t.Fatalf("got %v", err)
	}
}
