//go:build !darwin

// Package imessage is only available on macOS.
package imessage

import (
	"context"
	"errors"
	"log/slog"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Channel is a stub on non-macOS platforms.
type Channel struct{}

// New returns a stub.
func New(owner string, replyToOthers bool, log *slog.Logger) *Channel { return &Channel{} }

func (c *Channel) Name() string        { return "imessage" }
func (c *Channel) OwnerChatID() string { return "" }
func (c *Channel) Start(context.Context, channels.Handler) error {
	return channels.Fatal(errors.New("iMessage is only available on macOS"))
}
func (c *Channel) Send(context.Context, string, string) error {
	return errors.New("iMessage is only available on macOS")
}
