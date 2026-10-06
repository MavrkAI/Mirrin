package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Only the replay at connect is dropped for its age: a live message that
// took five minutes to arrive still gets its answer.
func TestStalenessAppliesOnlyToTheBacklog(t *testing.T) {
	var f fakeAPI
	srv := f.server(t, func(string) string { return `{"ok":true,"result":{}}` })
	c := New("TOKEN", "42", false, nil)
	c.api = srv.URL
	start := time.Now().Add(-5 * time.Minute).Truncate(time.Second) // connected five minutes ago
	clock := start
	c.backlog.Now = func() time.Time { return clock }
	c.backlog.Connected()
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	msg := func(id int, sent time.Time, text string) update {
		var u update
		raw := fmt.Sprintf(`{"update_id":%d,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":%q}}`, id, sent.Unix(), text)
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		return u
	}

	c.onUpdate(context.Background(), msg(1, start.Add(-5*time.Minute), "old"), h)
	clock = start.Add(5 * time.Minute)
	c.onUpdate(context.Background(), msg(2, start, "late"), h)
	if len(got) != 1 || got[0].Text != "late" {
		t.Fatalf("got %+v", got)
	}
}

// Through the real poll loop: a full first batch means more of the queue is
// waiting, so the next batch is still backlog and its old messages are
// dropped. Only a short batch ends the backlog; after that an old message
// is answered.
func TestBacklogLastsUntilAShortBatch(t *testing.T) {
	old := time.Now().Add(-time.Hour).Unix()
	upd := func(id int, text string) string {
		return fmt.Sprintf(`{"update_id":%d,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":%q}}`, id, old, text)
	}
	full := make([]string, updatesLimit)
	for i := range full {
		full[i] = upd(i+1, "queued")
	}
	batches := []string{
		"[" + strings.Join(full, ",") + "]",
		"[" + upd(updatesLimit+1, "still queued") + "]",
		"[]",
		"[" + upd(updatesLimit+2, "late") + "]",
	}
	var mu sync.Mutex
	n := 0
	var f fakeAPI
	srv := f.server(t, func(method string) string {
		if method != "getUpdates" {
			return `{"ok":true,"result":{"username":"twin_bot"}}`
		}
		mu.Lock()
		defer mu.Unlock()
		if n >= len(batches) {
			time.Sleep(20 * time.Millisecond)
			return `{"ok":true,"result":[]}`
		}
		n++
		return `{"ok":true,"result":` + batches[n-1] + `}`
	})
	c := New("TOKEN", "42", false, nil)
	c.api = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []string
	h := func(_ context.Context, in channels.Inbound) {
		got = append(got, in.Text)
		if in.Text == "late" {
			cancel()
		}
	}
	_ = c.Start(ctx, h)
	if len(got) != 1 || got[0] != "late" {
		t.Fatalf("got %q", got)
	}
}
