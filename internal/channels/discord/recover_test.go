package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Without the Message Content intent Discord closes the gateway with 4014 on
// every attempt: that has to stop and say what to switch on, not loop.
func TestGatewayCloseCodes(t *testing.T) {
	tests := []struct {
		code      websocket.StatusCode
		fatal     bool
		mentions  string
		resetting bool
	}{
		{4014, true, "Message Content intent", false},
		{4013, true, "Message Content intent", false},
		{4004, true, "token", false},
		{4009, false, "", true},
		{websocket.StatusGoingAway, false, "", false},
	}
	for _, tt := range tests {
		err := closeError(fmt.Errorf("read: %w", websocket.CloseError{Code: tt.code}))
		if channels.IsFatal(err) != tt.fatal || !strings.Contains(err.Error(), tt.mentions) || errors.Is(err, errSessionGone) != tt.resetting {
			t.Errorf("%d: %v", tt.code, err)
		}
	}
}

func TestRejectedTokenStopsTheChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message": "401: Unauthorized", "code": 0}`)
	}))
	defer srv.Close()
	c := New("t", "owner", false, nil, nil)
	c.api = srv.URL
	if err := c.Start(context.Background(), func(context.Context, channels.Inbound) {}); !channels.IsFatal(err) {
		t.Fatalf("got %v", err)
	}
}

func TestVoiceMessageIsAnsweredAndTypingWorks(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.URL.Path+" "+string(body))
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := New("t", "owner", false, nil, nil)
	c.api = srv.URL
	c.selfID = "self"
	var got []channels.Inbound
	var m message
	_ = json.Unmarshal([]byte(`{"id":"1","channel_id":"dm1","content":"","author":{"id":"u1","username":"owner"},"attachments":[{"content_type":"audio/ogg"}]}`), &m)
	c.onMessage(context.Background(), m, func(_ context.Context, in channels.Inbound) { got = append(got, in) })
	if len(got) != 1 || got[0].Media != channels.Voice || got[0].Text != "" || !got[0].IsOwner {
		t.Fatalf("handler %+v", got)
	}
	if again, err := c.Typing(context.Background(), "dm1"); err != nil || again <= 0 {
		t.Fatalf("typing: %v %v", again, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "/channels/dm1/typing " {
		t.Fatalf("calls %v", calls)
	}
}
