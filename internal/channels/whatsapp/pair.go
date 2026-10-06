//go:build !nowhatsapp

package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
	"rsc.io/qr"
)

// Pairing is an in-progress link between this machine and a phone. The page
// shows the current QR code (and a phone code when requested) until the
// phone accepts, then Pairing keeps the client connected long enough for
// WhatsApp to finish registering the device before handing over.
type Pairing struct {
	mu        sync.Mutex
	status    string // waiting | paired | error | timeout
	code      string // current QR payload
	phoneCode string // "ABCD-EFGH" when pairing by number
	errText   string
	as        string // the account we paired as
	done      chan struct{}
	cancel    context.CancelFunc
}

// Snapshot is what the page polls.
type Snapshot struct {
	Status    string `json:"status"`
	QRPNG     []byte `json:"qr_png,omitempty"`
	Code      string `json:"qr_code,omitempty"` // raw payload, for terminal rendering
	PhoneCode string `json:"phone_code,omitempty"`
	Error     string `json:"error,omitempty"`
	As        string `json:"as,omitempty"`
}

// Snapshot returns the current state with the QR rendered as PNG.
func (p *Pairing) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Snapshot{Status: p.status, PhoneCode: p.phoneCode, Error: p.errText, As: p.as}
	if p.status == "waiting" && p.code != "" {
		s.Code = p.code
		if c, err := qr.Encode(p.code, qr.L); err == nil {
			s.QRPNG = c.PNG()
		}
	}
	return s
}

// Done closes when pairing has finished one way or the other.
func (p *Pairing) Done() <-chan struct{} { return p.done }

// Paired reports success.
func (p *Pairing) Paired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status == "paired"
}

// Stop abandons the pairing.
func (p *Pairing) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *Pairing) set(f func()) {
	p.mu.Lock()
	f()
	p.mu.Unlock()
}

// StartPairing begins linking. phone (E.164, optional) asks for a code the
// user types under Linked devices → Link with phone number instead of scanning.
// The channel must not be running.
func (c *Channel) StartPairing(ctx context.Context, phone, displayName string) (*Pairing, error) {
	if err := c.open(ctx); err != nil {
		return nil, err
	}
	client := c.cl()
	if client.Store.ID != nil {
		return nil, fmt.Errorf("already paired as %s", client.Store.ID.User)
	}
	if displayName == "" {
		displayName = "Mirrin"
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	p := &Pairing{status: "waiting", done: make(chan struct{}), cancel: cancel}
	qrChan, err := client.GetQRChannel(pctx)
	if err != nil {
		cancel()
		return nil, err
	}
	connected := make(chan struct{}, 4)
	loggedOut := make(chan string, 1)
	handlerID := client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.Connected:
			connected <- struct{}{}
		case *events.PairSuccess:
			p.set(func() { p.as = v.ID.User })
		case *events.PairError:
			select {
			case loggedOut <- "pairing rejected: " + v.Error.Error():
			default:
			}
		case *events.LoggedOut:
			select {
			case loggedOut <- "the phone logged this device out":
			default:
			}
		}
	})
	if err := client.Connect(); err != nil {
		cancel()
		client.RemoveEventHandler(handlerID)
		return nil, err
	}
	if phone != "" {
		go func() {
			code, err := client.PairPhone(pctx, digits(phone), true, whatsmeow.PairClientChrome, displayName+" (Chrome)")
			p.set(func() {
				if err != nil {
					p.errText = "phone code: " + err.Error()
				} else {
					p.phoneCode = code
				}
			})
		}()
	}
	go func() {
		defer close(p.done)
		defer closeStore(client) // Start opens its own once paired
		defer client.RemoveEventHandler(handlerID)
		finish := func(status, errText string) {
			p.set(func() { p.status, p.errText = status, errText })
		}
		for {
			select {
			case <-pctx.Done():
				if p.Paired() {
					return
				}
				client.Disconnect()
				finish("timeout", "no phone linked within five minutes")
				return
			case msg := <-loggedOut:
				client.Disconnect()
				finish("error", msg)
				return
			case evt, ok := <-qrChan:
				if !ok {
					continue
				}
				switch evt.Event {
				case "code":
					p.set(func() { p.code = evt.Code })
				case "success":
					// The server drops the connection after pairing and the
					// client reconnects with its new identity. Wait for that,
					// then a little longer so the phone finishes registration.
					select {
					case <-connected:
					case <-time.After(20 * time.Second):
					}
					select {
					case <-time.After(6 * time.Second):
					case msg := <-loggedOut:
						client.Disconnect()
						finish("error", msg)
						return
					}
					client.Disconnect()
					c.setClient(nil) // Start reopens the store with the paired device
					finish("paired", "")
					return
				case "timeout":
					client.Disconnect()
					finish("timeout", "the QR code expired; try again")
					return
				case "err-client-outdated", "err-scanned-without-multidevice", "err-unexpected-state":
					client.Disconnect()
					finish("error", "pairing failed: "+evt.Event)
					return
				}
			}
		}
	}()
	return p, nil
}

// Unpair forgets the stored device so a fresh pairing can start.
func (c *Channel) Unpair(ctx context.Context) error {
	if err := c.open(ctx); err != nil {
		return err
	}
	client := c.cl()
	if client.Store.ID == nil {
		return nil
	}
	err := client.Store.Delete(ctx)
	closeStore(client)
	c.setClient(nil)
	if err != nil {
		return errors.New("could not delete the stored session: " + err.Error())
	}
	return nil
}

// ChatLink opens the owner's own chat, where the twin lives.
func (c *Channel) ChatLink() (string, string) {
	if c.owner == "" {
		return "", ""
	}
	return "Open your chat in WhatsApp", "https://wa.me/" + c.owner
}
