package signal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Only the replay at connect is dropped for its age: a live message that
// took five minutes to arrive still gets its answer.
func TestStalenessAppliesOnlyToTheBacklog(t *testing.T) {
	c := New("", "", "+61400000001", "+61400000002", false, nil)
	start := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond) // connected five minutes ago
	clock := start
	c.backlog.Now = func() time.Time { return clock }
	c.backlog.Connected()
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	send := func(sent time.Time, text string) {
		ms := sent.UnixMilli()
		c.onLine(context.Background(), []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceNumber":"+61400000002","timestamp":%d,"dataMessage":{"message":%q,"timestamp":%d}}}}`, ms, text, ms)), h)
	}

	send(start.Add(-5*time.Minute), "old")
	clock = start.Add(5 * time.Minute)
	send(start, "late")
	if len(got) != 1 || got[0].Text != "late" {
		t.Fatalf("got %+v", got)
	}
}
