package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
			c := New("https://mm.example", "token", "owner", false, nil)
			c.selfID = "bot"
			var p post
			json.Unmarshal([]byte(`{"user_id":"owner","channel_id":"D","message":"caption","file_ids":["id"],"metadata":{"files":[{"id":"id","mime_type":"`+mime+`","size":4}]}}`), &p)
			c.onPost(ctx, p, "D", "owner", h)
			if in.Attachment == nil || in.Media != channels.MediaKind(mime) || in.Text != "caption" || !in.IsOwner {
				t.Fatalf("handoff: %+v", in)
			}
			calls := 0
			status := 200
			payload := "data"
			length := int64(-1)
			c.http = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer token" || r.URL.Path != "/api/v4/files/id" {
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
	c := New("https://mm.example", "token", "", false, nil)
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

func TestMediaResolvesFileIDs(t *testing.T) {
	c := New("https://mm.example", "token", "owner", false, nil)
	calls := 0
	c.http = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/v4/files/id/info" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"id","mime_type":"audio/ogg","size":4}`))}, nil
	})}
	var in channels.Inbound
	c.onPost(context.Background(), post{UserID: "owner", FileIDs: []string{"id"}}, "D", "owner", func(_ context.Context, m channels.Inbound) { in = m })
	if calls != 1 || in.Media != channels.Voice || in.Attachment == nil || in.Attachment.Size != 4 {
		t.Fatalf("%+v calls=%d", in, calls)
	}
}

func TestMediaMetadataHasShortDeadline(t *testing.T) {
	c := New("https://mm.example", "token", "owner", false, nil)
	c.http = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Fatal("metadata lookup has no short deadline")
		}
		return nil, context.DeadlineExceeded
	})}
	a := c.attachment(context.Background(), post{FileIDs: []string{"id"}})
	if a == nil {
		t.Fatal("metadata error discarded the attachment")
	}
}
