package signal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestOnLine(t *testing.T) {
	c := New("", "", "+61400000001", "+61400000002", false, nil)
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	now := time.Now().UnixMilli()
	c.onLine(context.Background(), []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+61400000002","sourceNumber":"+61400000002","sourceName":"Akshay","timestamp":%d,"dataMessage":{"message":"hello","timestamp":%d}},"account":"+61400000001"}}`, now, now)), h)
	if len(got) != 1 || !got[0].IsOwner || got[0].ChatID != "+61400000002" || got[0].Sender != "Akshay" {
		t.Fatalf("got %+v", got)
	}
	c.onLine(context.Background(), []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceNumber":"+61400000009","timestamp":%d,"dataMessage":{"message":"spam"}}}}`, now)), h)
	c.onLine(context.Background(), []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceNumber":"+61400000002","timestamp":%d,"dataMessage":{"message":"group chatter","groupInfo":{"groupId":"g"}}}}}`, now)), h)
	c.onLine(context.Background(), []byte(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceNumber":"+61400000002","timestamp":1,"typingMessage":{}}}}`), h)
	if len(got) != 1 {
		t.Fatalf("strangers, groups and receipts should be ignored: %+v", got)
	}
}
