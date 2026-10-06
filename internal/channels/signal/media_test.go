package signal

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
			c := New("", "https://signal.example", "+1", "+2", false, nil)
			var in channels.Inbound
			c.onLine(context.Background(), []byte(`{"method":"receive","params":{"envelope":{"sourceNumber":"+2","dataMessage":{"message":"caption","attachments":[{"id":"id","contentType":"`+mime+`","size":4,"filename":"/private/file"}]}}}}`), func(_ context.Context, m channels.Inbound) { in = m })
			if in.Attachment == nil || in.Media != channels.MediaKind(mime) || in.Text != "caption" || !in.IsOwner {
				t.Fatalf("%+v", in)
			}
			calls := 0
			response := `{"result":{"data":"ZGF0YQ=="}}`
			status := 200
			c.http = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				var req struct {
					Method string
					Params struct{ Account, ID, Recipient string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				if r.URL.Path != "/api/v1/rpc" || req.Method != "getAttachment" || req.Params.Account != "+1" || req.Params.Recipient != "+2" || req.Params.ID != "id" {
					t.Fatalf("%+v %s", req, r.URL)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			if _, err := in.Attachment.Fetch(context.Background(), 3); !errors.Is(err, channels.ErrTooBig) || calls != 0 {
				t.Fatalf("metadata cap %v %d", err, calls)
			}
			if b, err := in.Attachment.Fetch(context.Background(), 4); err != nil || string(b) != "data" {
				t.Fatalf("%q %v", b, err)
			}
			response = `{"result":{"data":"ZGF0YWRhdGE="}}`
			if _, err := in.Attachment.Fetch(context.Background(), 4); !errors.Is(err, channels.ErrTooBig) {
				t.Fatalf("decoded cap %v", err)
			}
			for _, bad := range []string{`{"result":{"data":"!"}}`, `{"error":{"message":"missing attachment"}}`, `invalid`} {
				response = bad
				if _, err := in.Attachment.Fetch(context.Background(), 4); err == nil {
					t.Fatal(bad)
				}
			}
			status = 403
			response = `{"result":{"data":"ZGF0YQ=="}}`
			if _, err := in.Attachment.Fetch(context.Background(), 4); err == nil {
				t.Fatal("accepted HTTP error")
			}
		})
	}
}

func TestMediaStdio(t *testing.T) {
	c := New("", "", "+1", "+1", false, nil)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	c.stdin = writer
	go func() {
		var req struct{ ID int64 }
		json.NewDecoder(reader).Decode(&req)
		data, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]string{"data": "ZGF0YQ=="}})
		c.onLine(context.Background(), data, nil)
	}()
	var in channels.Inbound
	c.onLine(context.Background(), []byte(`{"method":"receive","params":{"envelope":{"syncMessage":{"sentMessage":{"destinationNumber":"+1","attachments":[{"id":"id","contentType":"application/octet-stream","isVoiceNote":true,"size":4}]}}}}}`), func(_ context.Context, m channels.Inbound) { in = m })
	if in.Attachment == nil || in.Media != channels.Voice {
		t.Fatalf("%+v", in)
	}
	b, err := in.Attachment.Fetch(context.Background(), 4)
	if err != nil || string(b) != "data" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestMediaUnknownStdioSizeDoesNotSendRPC(t *testing.T) {
	c := New("", "", "+1", "+2", false, nil)
	for _, size := range []int64{0, -1} {
		a := c.attachment([]signalAttachment{{ID: "id", Size: size}}, "+2")
		if _, err := a.Fetch(context.Background(), 100); !errors.Is(err, ErrAttachmentSizeUnknown) {
			t.Fatal(err)
		}
		if c.nextID.Load() != 0 {
			t.Fatal("sent an unbounded attachment RPC")
		}
	}
}

func TestMediaOldSignalCLI(t *testing.T) {
	c := New("", "https://signal.example", "+1", "+2", false, nil)
	for _, body := range []string{`{"error":{"code":-32601,"message":"unknown command"}}`, `{"error":{"message":"Method not found"}}`} {
		c.http = &http.Client{Transport: mediaTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if _, err := c.attachment([]signalAttachment{{ID: "id", Size: 4}}, "+2").Fetch(context.Background(), 4); !errors.Is(err, ErrAttachmentUpgrade) {
			t.Fatal(err)
		}
	}
}
