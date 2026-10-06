package slack

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
			c := New("token", "app", "U0OWNER123", false, nil)
			c.names["U0OWNER123"] = "owner"
			var env envelope
			json.Unmarshal([]byte(`{"payload":{"event":{"type":"message","subtype":"file_share","channel_type":"im","channel":"D","user":"U0OWNER123","text":"caption","ts":"9999999999","files":[{"mimetype":"`+mime+`","size":4,"url_private":"https://files.slack.com/file"}]}}}`), &env)
			c.onEvent(ctx, env, h)
			if in.Attachment == nil || in.Media != channels.MediaKind(mime) || in.Text != "caption" || !in.IsOwner {
				t.Fatalf("handoff: %+v", in)
			}
			calls := 0
			status := 200
			payload := "data"
			length := int64(-1)
			c.http = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer token" || r.URL.Path != "/file" {
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
	c := New("token", "app", "", false, nil)
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

func TestMediaRejectsForeignURL(t *testing.T) {
	c := New("token", "app", "", false, nil)
	for _, uri := range []string{"https://evil.example/file", "http://files.slack.com/file", "https://slack.com.evil.example/file"} {
		if c.attachment([]slackFile{{URL: uri}}) != nil {
			t.Fatal(uri)
		}
	}
	if !strings.Contains(ManifestURL("Mirrin"), "files%3Aread") {
		t.Fatal("missing files:read scope")
	}
}

func TestMediaRejectsSignInPage(t *testing.T) {
	c := New("token", "app", "", false, nil)
	for _, ctype := range []string{"text/html; charset=utf-8", "application/octet-stream"} {
		c.http = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ctype}}, Body: io.NopCloser(strings.NewReader("<!DOCTYPE html><html>Sign in</html>"))}, nil
		})}
		if _, err := c.attachment([]slackFile{{URL: "https://files.slack.com/file"}}).Fetch(context.Background(), 1024); !errors.Is(err, ErrFilePermission) {
			t.Fatalf("%s: %v", ctype, err)
		}
	}
	a := c.attachment([]slackFile{{URL: "https://files.slack.com/file", DurationMS: 1501}})
	if a.Seconds != 2 {
		t.Fatal(a.Seconds)
	}
}
