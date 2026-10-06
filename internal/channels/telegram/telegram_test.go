package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// fakeAPI records Bot API calls and answers them with reply(method).
type fakeAPI struct {
	mu    sync.Mutex
	calls []string // "method body"
}

func (f *fakeAPI) server(t *testing.T, reply func(method string) string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, method+" "+string(body))
		f.mu.Unlock()
		_, _ = io.WriteString(w, reply(method))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeAPI) sent(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, method+" ") {
			out = append(out, strings.TrimPrefix(c, method+" "))
		}
	}
	return out
}

func TestSendSplitsWithoutBreakingCharacters(t *testing.T) {
	for name, unit := range map[string]string{
		"hindi": "नमस्ते दुनिया ",
		"cjk":   "你好世界你好世界",
		"emoji": "a👍🏽🎉🚀",
	} {
		t.Run(name, func(t *testing.T) {
			var f fakeAPI
			srv := f.server(t, func(string) string { return `{"ok":true,"result":{}}` })
			c := New("TOKEN", "42", false, nil)
			c.api = srv.URL
			long := strings.Repeat(unit, 9000/len(unit)+1)
			if err := c.Send(context.Background(), "42", long); err != nil {
				t.Fatal(err)
			}
			var got strings.Builder
			parts := f.sent("sendMessage")
			if len(parts) < 2 {
				t.Fatalf("expected several parts, got %d", len(parts))
			}
			for _, p := range parts {
				var body struct{ Text string }
				_ = json.Unmarshal([]byte(p), &body)
				if !utf8.ValidString(body.Text) || strings.ContainsRune(body.Text, utf8.RuneError) || len(body.Text) > 4000 {
					t.Fatalf("broken part (%d bytes): %q", len(body.Text), body.Text[len(body.Text)-8:])
				}
				got.WriteString(body.Text)
			}
			if got.String() != long {
				t.Fatal("parts don't add up to the message")
			}
		})
	}
}

func TestRejectedTokenIsFatalAndNotLeaked(t *testing.T) {
	var f fakeAPI
	srv := f.server(t, func(string) string { return `{"ok":false,"error_code":401,"description":"Unauthorized"}` })
	c := New("123:SECRET", "42", false, nil)
	c.api = srv.URL
	err := c.Start(context.Background(), func(context.Context, channels.Inbound) {})
	if err == nil || !channels.IsFatal(err) {
		t.Fatalf("a rejected token should stop the channel for good, got %v", err)
	}
	// A network failure is retried, and its text must not carry the token.
	srv.Close()
	err = c.Start(context.Background(), func(context.Context, channels.Inbound) {})
	if err == nil || channels.IsFatal(err) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("network error should be retryable and token-free, got %v", err)
	}
	if st := c.Status(); st.State != channels.Connecting || st.Err == "" || strings.Contains(st.Err, "SECRET") {
		t.Fatalf("status: %+v", st)
	}
}

// Telegram answers 409 both when another poller is running (worth retrying:
// the other one may stop) and when the bot has a webhook, which only the
// owner can remove. The second must say so and stop.
func TestConflictsAreToldApart(t *testing.T) {
	for _, tt := range []struct {
		description string
		fatal       bool
		want        string
	}{
		{"Conflict: terminated by other getUpdates request; make sure that only one bot instance is running", false, "running twice"},
		{"Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first", true, "deleteWebhook"},
	} {
		var f fakeAPI
		srv := f.server(t, func(method string) string {
			if method == "getMe" {
				return `{"ok":true,"result":{"username":"twin_bot"}}`
			}
			b, _ := json.Marshal(map[string]any{"ok": false, "error_code": 409, "description": tt.description})
			return string(b)
		})
		c := New("TOKEN", "42", false, nil)
		c.api = srv.URL
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		err := c.Start(ctx, func(context.Context, channels.Inbound) {})
		cancel()
		st := c.Status()
		if tt.fatal != channels.IsFatal(err) || !strings.Contains(st.Err, tt.want) {
			t.Errorf("%q: err %v, status %+v", tt.description, err, st)
		}
	}
}

func TestMediaReachesTheDaemon(t *testing.T) {
	var f fakeAPI
	srv := f.server(t, func(string) string { return `{"ok":true,"result":{}}` })
	c := New("TOKEN", "42", false, nil)
	c.api = srv.URL
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	now := time.Now().Unix()
	upd := func(raw string) update {
		var u update
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	tests := []struct {
		name, raw string
		want      *channels.Inbound // nil when the twin shouldn't hear it
	}{
		{"voice note", `{"update_id":1,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"voice":{"file_id":"v"}}}`,
			&channels.Inbound{Media: channels.Voice}},
		{"photo alone", `{"update_id":2,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"photo":[{"file_id":"p"}]}}`,
			&channels.Inbound{Media: channels.Photo}},
		{"photo with caption", `{"update_id":3,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"photo":[{"file_id":"p"}],"caption":"is this mould?"}}`,
			&channels.Inbound{Text: "is this mould?", Media: channels.Photo}},
		{"stranger's voice note", `{"update_id":4,"message":{"date":%d,"from":{"id":7},"chat":{"id":7,"type":"private"},"voice":{"file_id":"v"}}}`, nil},
		{"sticker", `{"update_id":5,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"sticker":{"file_id":"s"}}}`, nil},
	}
	for _, tt := range tests {
		got = nil
		c.onUpdate(context.Background(), upd(strings.Replace(tt.raw, "%d", itoa(now), 1)), h)
		switch {
		case tt.want == nil && len(got) != 0:
			t.Errorf("%s: handler should not run, got %+v", tt.name, got)
		case tt.want != nil && (len(got) != 1 || got[0].Text != tt.want.Text || got[0].Media != tt.want.Media || !got[0].IsOwner || got[0].ChatID != "42"):
			t.Errorf("%s: handler got %+v", tt.name, got)
		}
	}
	// The channel only passes it on; what to say is the daemon's call.
	if sent := f.sent("sendMessage"); len(sent) != 0 {
		t.Fatalf("the channel answered by itself: %v", sent)
	}
}

func TestTyping(t *testing.T) {
	var f fakeAPI
	srv := f.server(t, func(string) string { return `{"ok":true,"result":true}` })
	c := New("TOKEN", "42", false, nil)
	c.api = srv.URL
	again, err := c.Typing(context.Background(), "42")
	if err != nil || again <= 0 || again >= 5*time.Second {
		t.Fatalf("typing: %v %v", again, err)
	}
	if calls := f.sent("sendChatAction"); len(calls) != 1 || !strings.Contains(calls[0], `"typing"`) {
		t.Fatalf("calls: %v", calls)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
