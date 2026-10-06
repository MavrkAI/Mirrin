package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func fakeSlack(t *testing.T, reply func(method string) string) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		body, _ := io.ReadAll(r.Body)
		if method == "chat.postMessage" {
			mu.Lock()
			posted = append(posted, string(body))
			mu.Unlock()
		}
		_, _ = io.WriteString(w, reply(method))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), posted...) }
}

func TestRevokedTokenStopsTheChannel(t *testing.T) {
	srv, _ := fakeSlack(t, func(string) string { return `{"ok":false,"error":"invalid_auth"}` })
	c := New("xoxb-1", "xapp-1", "U0OWNER123", false, nil)
	c.api = srv.URL + "/"
	err := c.Start(context.Background(), func(context.Context, channels.Inbound) {})
	if !channels.IsFatal(err) || !strings.Contains(err.Error(), "bot token") {
		t.Fatalf("got %v", err)
	}
}

func TestFileWithoutTextIsAnswered(t *testing.T) {
	srv, posted := fakeSlack(t, func(string) string { return `{"ok":true}` })
	c := New("xoxb", "xapp", "U0OWNER123", false, nil)
	c.api = srv.URL + "/"
	c.selfID = "UBOT"
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	var env envelope
	_ = json.Unmarshal([]byte(`{"type":"events_api","payload":{"event":{"type":"message","subtype":"file_share","channel_type":"im","channel":"D1","user":"U0OWNER123","text":"","ts":"9999999999.0001","files":[{"mimetype":"audio/webm"}]}}}`), &env)
	c.onEvent(context.Background(), env, h)
	if len(got) != 1 || got[0].Text != "" || got[0].Media != channels.Voice || len(posted()) != 0 {
		t.Fatalf("handler %+v, posted %v", got, posted())
	}
	_ = json.Unmarshal([]byte(`{"type":"events_api","payload":{"event":{"type":"message","subtype":"file_share","channel_type":"im","channel":"D1","user":"U0OWNER123","text":"what's this rash?","ts":"9999999999.0002","files":[{"mimetype":"image/png"}]}}}`), &env)
	c.onEvent(context.Background(), env, h)
	if len(got) != 2 || got[1].Text != "what's this rash?" || got[1].Media != channels.Photo {
		t.Fatalf("got %+v", got)
	}
}
