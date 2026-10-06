package mattermost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestRejectedTokenStopsTheChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"id":"api.context.session_expired.app_error","message":"Invalid or expired session, please login again.","status_code":401}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "tok", "akshay", false, nil)
	if err := c.Start(context.Background(), func(context.Context, channels.Inbound) {}); !channels.IsFatal(err) {
		t.Fatalf("got %v", err)
	}
}

func TestFileOnlyPostIsAnswered(t *testing.T) {
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
	c := New(srv.URL, "tok", "akshay", false, nil)
	c.selfID, c.selfName = "bot1", "mavrk"
	var got []channels.Inbound
	p := post{ID: "1", UserID: "u1", ChannelID: "d1", CreateAt: time.Now().UnixMilli(), FileIDs: []string{"f1"}}
	p.Metadata.Files = append(p.Metadata.Files, fileInfo{MimeType: "image/jpeg"})
	c.onPost(context.Background(), p, "D", "akshay", func(_ context.Context, in channels.Inbound) { got = append(got, in) })
	if len(got) != 1 || got[0].Media != channels.Photo || got[0].Text != "" {
		t.Fatalf("handler %+v", got)
	}
	if again, err := c.Typing(context.Background(), "d1"); err != nil || again <= 0 {
		t.Fatalf("typing: %v %v", again, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "/api/v4/users/bot1/typing") {
		t.Fatalf("calls %v", calls)
	}
}
