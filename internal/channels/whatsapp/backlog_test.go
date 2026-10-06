//go:build !nowhatsapp

package whatsapp

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Only the replay at connect is dropped for its age: a live message that
// took five minutes to arrive still gets its answer.
func TestStalenessAppliesOnlyToTheBacklog(t *testing.T) {
	c := New(t.TempDir(), "+61400000001", false, nil)
	self := []types.JID{types.NewJID("61400000001", types.DefaultUserServer)}
	start := time.Now().Add(-5 * time.Minute) // connected five minutes ago
	clock := start
	c.backlog.Now = func() time.Time { return clock }
	c.backlog.Connected()
	msg := func(sent time.Time, text string) *events.Message {
		m := fromOwner(&waE2E.Message{Conversation: proto.String(text)})
		m.Info.Timestamp = sent
		return m
	}

	if _, ok := c.inbound(msg(start.Add(-5*time.Minute), "old"), self); ok {
		t.Fatal("a 5-minute-old backlog message should be dropped")
	}
	clock = start.Add(5 * time.Minute)
	if in, ok := c.inbound(msg(start, "late"), self); !ok || in.Text != "late" {
		t.Fatalf("a delayed live message should be kept: %+v %v", in, ok)
	}

	// WhatsApp says when the offline backlog is done.
	c.onEvent(context.Background(), &events.Connected{}, noHandler, make(chan error, 1))
	c.onEvent(context.Background(), &events.OfflineSyncCompleted{}, noHandler, make(chan error, 1))
	if _, ok := c.inbound(msg(clock.Add(-5*time.Minute), "after sync"), self); !ok {
		t.Fatal("after the offline sync nothing is dropped for its age")
	}
}
