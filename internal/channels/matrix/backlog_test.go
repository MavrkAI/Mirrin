package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Only the replay at connect is dropped for its age: a live message that
// took five minutes to arrive still gets its answer.
func TestStalenessAppliesOnlyToTheBacklog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "@mavrk:hs", "tok", "@akshay:hs", false, nil)
	start := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond) // connected five minutes ago
	clock := start
	c.backlog.Now = func() time.Time { return clock }
	c.backlog.Connected()
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	send := func(sent time.Time, text string) {
		var ev event
		raw := fmt.Sprintf(`{"type":"m.room.message","sender":"@akshay:hs","origin_server_ts":%d,"content":{"msgtype":"m.text","body":%q}}`, sent.UnixMilli(), text)
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal(err)
		}
		c.onEvent(context.Background(), "!dm:hs", ev, h)
	}

	send(start.Add(-5*time.Minute), "old")
	clock = start.Add(5 * time.Minute)
	send(start, "late")
	if len(got) != 1 || got[0].Text != "late" {
		t.Fatalf("got %+v", got)
	}
}
