//go:build nowhatsapp

// Package whatsapp, built with -tags nowhatsapp, has no WhatsApp: whatsmeow
// and the GPL-3.0 Signal library it needs are left out, so the program can be
// shared under the MIT licence (docs/licensing.md). Everything here says
// so plainly instead of connecting. The types match whatsapp.go and pair.go;
// CI builds with the tag so they stay in step.
package whatsapp

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Built reports whether this program has the WhatsApp channel.
const Built = false

// ErrNotBuilt is what every WhatsApp action answers in this build.
var ErrNotBuilt = errors.New("this Mirrin was built without WhatsApp (-tags nowhatsapp). Use another channel, or a build with WhatsApp from the releases page")

// Channel stands in for the WhatsApp transport.
type Channel struct{ owner string }

// New builds the stand-in.
func New(dataDir, owner string, replyToOthers bool, log *slog.Logger) *Channel {
	var b strings.Builder
	for _, r := range owner {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return &Channel{owner: b.String()}
}

func (c *Channel) Name() string { return "whatsapp" }

// OwnerChatID is the owner's chat, as the real channel names it.
func (c *Channel) OwnerChatID() string { return c.owner + "@s.whatsapp.net" }

// Login says there is no WhatsApp in this build.
func (c *Channel) Login(ctx context.Context, name string) error { return ErrNotBuilt }

// Start stops the channel for good: retrying can't help.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	return channels.Fatal(ErrNotBuilt)
}

func (c *Channel) Send(ctx context.Context, chatID, text string) error { return ErrNotBuilt }

func (c *Channel) SendImage(ctx context.Context, chatID, path, caption string) error {
	return ErrNotBuilt
}

func (c *Channel) Close() error { return nil }

// StartPairing says there is no WhatsApp in this build.
func (c *Channel) StartPairing(ctx context.Context, phone, displayName string) (*Pairing, error) {
	return nil, ErrNotBuilt
}

// Unpair has nothing to forget.
func (c *Channel) Unpair(ctx context.Context) error { return nil }

func (c *Channel) ChatLink() (string, string) { return "", "" }

// Paired is always false: this build can't use a stored device.
func Paired(dataDir string) bool { return false }

// Pairing is never started in this build.
type Pairing struct{}

func (p *Pairing) Snapshot() Snapshot { return Snapshot{Status: "error", Error: ErrNotBuilt.Error()} }

var finished = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func (p *Pairing) Done() <-chan struct{} { return finished }

func (p *Pairing) Paired() bool { return false }

func (p *Pairing) Stop() {}

// Snapshot is what the page polls; the same shape as in pair.go.
type Snapshot struct {
	Status    string `json:"status"`
	QRPNG     []byte `json:"qr_png,omitempty"`
	Code      string `json:"qr_code,omitempty"`
	PhoneCode string `json:"phone_code,omitempty"`
	Error     string `json:"error,omitempty"`
	As        string `json:"as,omitempty"`
}
