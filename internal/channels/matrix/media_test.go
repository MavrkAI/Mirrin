package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

type mediaTransport func(*http.Request) (*http.Response, error)

func (f mediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMediaHandoff(t *testing.T) {
	for _, mime := range []string{"image/png", "audio/ogg"} {
		t.Run(mime, func(t *testing.T) {
			ctx := context.Background()
			var in channels.Inbound
			h := func(_ context.Context, m channels.Inbound) { in = m }
			c := New("https://matrix.example", "@bot:example", "token", "@owner:example", false, nil)
			c.onEvent(ctx, "!room", event{Type: "m.room.message", Sender: "@owner:example", Content: json.RawMessage(`{"msgtype":"` + map[string]string{"image/png": "m.image", "audio/ogg": "m.audio"}[mime] + `","body":"caption","filename":"clip","url":"mxc://remote.example/id","info":{"mimetype":"` + mime + `","size":4,"duration":1501}}`)}, h)
			if in.Attachment == nil || in.Media != channels.MediaKind(mime) || in.Text != "caption" || !in.IsOwner {
				t.Fatalf("handoff: %+v", in)
			}
			calls := 0
			status := 200
			payload := "data"
			length := int64(-1)
			c.http = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer token" || r.URL.Path != "/_matrix/client/v1/media/download/remote.example/id" {
					t.Fatalf("request: %s %v", r.URL, r.Header)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(payload)), ContentLength: length, Header: make(http.Header)}, nil
			})}
			if _, err := in.Attachment.Fetch(ctx, 3); !errors.Is(err, channels.ErrTooBig) || calls != 0 {
				t.Fatalf("metadata cap: %v calls=%d", err, calls)
			}
			if b, err := in.Attachment.Fetch(ctx, 4); err != nil || string(b) != "data" {
				t.Fatalf("fetch: %q %v", b, err)
			}
			payload = "oversized"
			if _, err := in.Attachment.Fetch(ctx, 4); !errors.Is(err, channels.ErrTooBig) {
				t.Fatalf("stream cap: %v", err)
			}
			payload = "data"
			length = 99
			if _, err := in.Attachment.Fetch(ctx, 4); !errors.Is(err, channels.ErrTooBig) {
				t.Fatalf("header cap: %v", err)
			}
			status = 403
			if _, err := in.Attachment.Fetch(ctx, 4); err == nil {
				t.Fatal("accepted forbidden response")
			}
		})
	}
}

func TestMediaRedirectDropsCredentials(t *testing.T) {
	c := New("https://matrix.example", "", "token", "", false, nil)
	calls := 0
	c.http = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://sub.example.com/file"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatal("token leaked on redirect")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data"))}, nil
	})}
	if _, err := c.download(context.Background(), "https://example.com/file", 0, 4); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestMediaRejectsNonMXC(t *testing.T) {
	c := New("https://matrix.example", "", "token", "", false, nil)
	for _, uri := range []string{"https://evil.example/file", "mxc://remote/a/b", "mxc://remote/.."} {
		f := mediaContent{URL: uri, MsgType: "m.image"}
		if _, err := c.attachment(f).Fetch(context.Background(), 4); err == nil {
			t.Fatal(uri)
		}
	}
}

func TestMediaLegacyFallback(t *testing.T) {
	for _, code := range []string{"M_UNRECOGNIZED", "M_NOT_FOUND"} {
		c := New("https://matrix.example", "", "token", "", false, nil)
		calls := 0
		c.http = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Header.Get("Authorization") != "Bearer token" {
				t.Fatal("missing token")
			}
			if calls == 1 {
				return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"errcode":"` + code + `"}`))}, nil
			}
			if r.URL.Path != "/_matrix/media/v3/download/remote/id" {
				t.Fatal(r.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data"))}, nil
		})}
		b, err := c.attachment(mediaContent{MsgType: "m.image", URL: "mxc://remote/id"}).Fetch(context.Background(), 4)
		if code == "M_UNRECOGNIZED" && (err != nil || string(b) != "data" || calls != 2) {
			t.Fatal(string(b), err, calls)
		}
		if code == "M_NOT_FOUND" && (err == nil || calls != 1) {
			t.Fatal(err, calls)
		}
	}
}
