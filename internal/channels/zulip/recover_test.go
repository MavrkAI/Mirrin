package zulip

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestBadAPIKeyStopsTheChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"result":"error","msg":"Invalid API key","code":"INVALID_API_KEY"}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "bot@zulip.example", "key", "me@example.com", false, nil)
	if err := c.Start(context.Background(), func(context.Context, channels.Inbound) {}); !channels.IsFatal(err) {
		t.Fatalf("got %v", err)
	}
}

func TestTypingInDirectMessages(t *testing.T) {
	var form string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.URL.Path + " " + r.PostForm.Encode()
		_, _ = io.WriteString(w, `{"result":"success"}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "bot@zulip.example", "key", "me@example.com", false, nil)
	// Nobody heard from yet: nothing to show.
	if again, err := c.Typing(context.Background(), "pm:me@example.com"); again != 0 || err != nil {
		t.Fatalf("typing before any message: %v %v", again, err)
	}
	c.onMessage(context.Background(), zmessage{SenderEmail: "me@example.com", SenderID: 8, Type: "private", Content: "hi", Timestamp: time.Now().Unix()}, false, func(context.Context, channels.Inbound) {})
	if again, err := c.Typing(context.Background(), "pm:me@example.com"); again <= 0 || err != nil {
		t.Fatalf("typing: %v %v", again, err)
	}
	if !strings.HasPrefix(form, "/api/v1/typing ") || !strings.Contains(form, "op=start") || !strings.Contains(form, "to=%5B8%5D") {
		t.Fatalf("request %s", form)
	}
}
