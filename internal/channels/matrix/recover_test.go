package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestExpiredTokenStopsTheChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid access token passed."}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "@mavrk:hs", "tok", "@akshay:hs", false, nil)
	if err := c.Start(context.Background(), func(context.Context, channels.Inbound) {}); !channels.IsFatal(err) || !strings.Contains(err.Error(), "M_UNKNOWN_TOKEN") {
		t.Fatalf("got %v", err)
	}
}

func TestVoiceMessageIsAnswered(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = append(sent, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "@mavrk:hs", "tok", "@akshay:hs", false, nil)
	var got []channels.Inbound
	var ev event
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"type":"m.room.message","sender":"@akshay:hs","origin_server_ts":%d,"content":{"msgtype":"m.audio","body":"Voice message.ogg"}}`, time.Now().UnixMilli())), &ev)
	c.onEvent(context.Background(), "!dm:hs", ev, func(_ context.Context, in channels.Inbound) { got = append(got, in) })
	if len(got) != 1 || got[0].Media != channels.Voice || got[0].Text != "" {
		t.Fatalf("handler %+v", got)
	}
	// A captioned photo reaches the twin as its caption.
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"type":"m.room.message","sender":"@akshay:hs","origin_server_ts":%d,"content":{"msgtype":"m.image","body":"is this mould?","filename":"IMG_1.jpg"}}`, time.Now().UnixMilli())), &ev)
	c.onEvent(context.Background(), "!dm:hs", ev, func(_ context.Context, in channels.Inbound) { got = append(got, in) })
	if len(got) != 2 || got[1].Text != "is this mould?" || got[1].Media != channels.Photo {
		t.Fatalf("captioned photo: %+v", got)
	}
	// Typing is cleared once the reply is out, not left for ten seconds.
	if err := c.EndTyping(context.Background(), "!dm:hs"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "PUT /_matrix/client/v3/rooms/!dm:hs/typing/") || !strings.Contains(sent[0], `"typing":false`) {
		t.Fatalf("sent %v", sent)
	}
}
