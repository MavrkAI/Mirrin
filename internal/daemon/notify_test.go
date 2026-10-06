package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// flaky is a channel that is down until up is set.
type flaky struct{ up bool }

func (f *flaky) Name() string                                  { return "telegram" }
func (f *flaky) Start(context.Context, channels.Handler) error { return nil }
func (f *flaky) OwnerChatID() string                           { return "1" }
func (f *flaky) Send(_ context.Context, _ string, _ string) error {
	if !f.up {
		return errors.New("telegram: connection refused")
	}
	return nil
}

func TestNotifyRecordsOnlyWhatWentOut(t *testing.T) {
	fastRetry(t) // the owner's chat refusing is retried once; no need to wait a second
	ctx := context.Background()
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ch := &flaky{}
	d := &Daemon{cfg: config.Default(), log: slog.Default(), store: store, bus: events.New(), channels: map[string]channels.Channel{"telegram": ch}}

	// A reminder retried while the channel is down, then delivered.
	for i := 0; i < 3; i++ {
		if err := d.Notify(ctx, "telegram:1", "Reminder: call mum"); err == nil {
			t.Fatal("a failed send should say so")
		}
	}
	ch.up = true
	if err := d.Notify(ctx, "telegram:1", "Reminder: call mum"); err != nil {
		t.Fatal(err)
	}
	h, _ := store.History(ctx, "telegram:1", 10)
	if len(h) != 1 || h[0].PlainText() != "Reminder: call mum" {
		t.Fatalf("want the reminder recorded once, got %+v", h)
	}
}
