package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

const noteToSelf = `{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+61400000002","sourceNumber":"+61400000002","sourceName":"Akshay","timestamp":%d,
	"syncMessage":{"sentMessage":{"destination":"%s","destinationNumber":"%s","timestamp":%d,"message":"%s"}}}}}`

// Linked as a device of the owner's own account (the documented setup), the
// owner talks to the twin in Note to Self, and only there.
func TestLinkedAccountHearsTheOwnerInNoteToSelf(t *testing.T) {
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	now := time.Now().UnixMilli()
	line := func(c *Channel, to, text string, ts int64) {
		c.onLine(context.Background(), []byte(fmt.Sprintf(noteToSelf, now, to, to, ts, text)), h)
	}

	linked := New("", "", "+61400000002", "+61400000002", false, nil)
	line(linked, "+61400000002", "remind me at six", now)
	line(linked, "+61400000009", "running late, see you at 8", now) // to a friend: not for the twin
	if len(got) != 1 || got[0].Text != "remind me at six" || !got[0].IsOwner || got[0].ChatID != "+61400000002" {
		t.Fatalf("got %+v", got)
	}
	// The twin's own replies in Note to Self are not new instructions.
	linked.remember(json.RawMessage(fmt.Sprintf(`{"timestamp":%d}`, now+1)))
	line(linked, "+61400000002", "Done, I'll remind you at six.", now+1)
	if len(got) != 1 {
		t.Fatalf("own reply taken as the owner's: %+v", got)
	}
	// With a separate number for the twin, sync messages are never the owner.
	dedicated := New("", "", "+61400000001", "+61400000002", false, nil)
	line(dedicated, "+61400000001", "sent from another device of the twin's number", now)
	if len(got) != 1 {
		t.Fatalf("dedicated number took a sync message: %+v", got)
	}
}

func TestAttachmentsAreAnswered(t *testing.T) {
	c := New("", "", "+61400000001", "+61400000002", false, nil)
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	now := time.Now().UnixMilli()
	c.onLine(context.Background(), []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceNumber":"+61400000002","timestamp":%d,
		"dataMessage":{"message":"what is this?","attachments":[{"contentType":"image/jpeg"}]}}}}`, now)), h)
	if len(got) != 1 || got[0].Text != "what is this?" || got[0].Media != channels.Photo {
		t.Fatalf("got %+v", got)
	}
}

func TestProblemsOnlyTheOwnerCanFixStopTheChannel(t *testing.T) {
	c := New("/nonexistent/signal-cli", "", "+61400000001", "+61400000002", false, nil)
	if err := c.Start(context.Background(), func(context.Context, channels.Inbound) {}); !channels.IsFatal(err) {
		t.Fatalf("missing signal-cli should stop the channel, got %v", err)
	}
	if st := c.Status(); st.State != channels.Failed {
		t.Fatalf("status %+v", st)
	}
	for msg, fatal := range map[string]bool{
		"User +61400000001 is not registered.":             true,
		"Authorization failed, was the number registered?": true,
		"Connection closed unexpectedly":                   false,
	} {
		if got := channels.IsFatal(exitError("+61400000001", msg)); got != fatal {
			t.Errorf("%q: fatal=%v", msg, got)
		}
	}
}
