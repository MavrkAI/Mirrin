package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// A voice message or picture on Discord comes with a way to fetch it from
// Discord's CDN, and only from there.
func TestAttachmentsCanBeFetchedFromTheCDN(t *testing.T) {
	var fetched int
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched++
		_, _ = io.WriteString(w, "OggS voice")
	}))
	defer cdn.Close()
	u, _ := url.Parse(cdn.URL)
	old := cdnHosts
	cdnHosts = append([]string{u.Hostname()}, old...)
	t.Cleanup(func() { cdnHosts = old })

	c := New("t", "owner", false, nil, nil)
	c.http = cdn.Client()
	c.selfID = "self"
	hear := func(raw string) channels.Inbound {
		t.Helper()
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		var got []channels.Inbound
		c.onMessage(context.Background(), m, func(_ context.Context, in channels.Inbound) { got = append(got, in) })
		if len(got) != 1 {
			t.Fatalf("heard %d", len(got))
		}
		return got[0]
	}
	voice := hear(`{"id":"1","channel_id":"dm1","author":{"id":"u1","username":"owner"},"attachments":[{"content_type":"audio/ogg","url":"` + cdn.URL + `/v.ogg","size":10,"duration_secs":4.6}]}`)
	if voice.Media != channels.Voice || voice.Attachment == nil || voice.Attachment.Seconds != 5 {
		t.Fatalf("voice message: %+v", voice)
	}
	if fetched != 0 {
		t.Fatal("downloaded on the gateway loop")
	}
	if data, err := voice.Attachment.Fetch(context.Background(), 1<<20); err != nil || string(data) != "OggS voice" {
		t.Fatalf("fetch: %q %v", data, err)
	}
	if _, err := voice.Attachment.Fetch(context.Background(), 5); !errors.Is(err, channels.ErrTooBig) || fetched != 1 {
		t.Fatalf("too big: %v (%d fetches)", err, fetched)
	}
	// A link anywhere else, or a file the twin can't take in, isn't fetched.
	for _, raw := range []string{
		`{"id":"2","channel_id":"dm1","author":{"id":"u1","username":"owner"},"attachments":[{"content_type":"image/png","url":"https://evil.example/x.png"}]}`,
		`{"id":"3","channel_id":"dm1","author":{"id":"u1","username":"owner"},"attachments":[{"content_type":"image/png","url":"http://cdn.discordapp.com/x.png"}]}`,
		`{"id":"4","channel_id":"dm1","author":{"id":"u1","username":"owner"},"attachments":[{"content_type":"application/pdf","url":"https://cdn.discordapp.com/x.pdf"}]}`,
	} {
		if in := hear(raw); in.Attachment != nil {
			t.Errorf("%s: fetchable %+v", raw, in.Attachment)
		}
	}
}
