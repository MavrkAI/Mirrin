package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Voice notes and photos come with a way to fetch them (getFile, then the
// file), which the daemon uses when it gets to the message.
func TestVoiceNotesAndPhotosCanBeFetched(t *testing.T) {
	var getFiles atomic.Int32
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			getFiles.Add(1)
			var body struct {
				FileID string `json:"file_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			asked = append(asked, body.FileID)
			_, _ = io.WriteString(w, `{"ok":true,"result":{"file_id":"`+body.FileID+`","file_size":10,"file_path":"media/`+body.FileID+`"}}`)
		case r.URL.Path == "/file/botTOKEN/media/voice-1":
			_, _ = io.WriteString(w, "OggS voice")
		case r.URL.Path == "/file/botTOKEN/media/p-1280":
			_, _ = io.WriteString(w, "\xff\xd8\xff photo")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New("TOKEN", "42", false, nil)
	c.api = srv.URL
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	send := func(raw string) channels.Inbound {
		t.Helper()
		got = nil
		var u update
		if err := json.Unmarshal([]byte(strings.Replace(raw, "%d", itoa(time.Now().Unix()), 1)), &u); err != nil {
			t.Fatal(err)
		}
		c.onUpdate(context.Background(), u, h)
		if len(got) != 1 {
			t.Fatalf("heard %d messages", len(got))
		}
		return got[0]
	}
	ctx := context.Background()

	voice := send(`{"update_id":1,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"voice":{"file_id":"voice-1","duration":3,"mime_type":"audio/ogg","file_size":10}}}`)
	if voice.Media != channels.Voice || voice.Attachment == nil || voice.Attachment.Seconds != 3 || voice.Attachment.Mime != "audio/ogg" {
		t.Fatalf("voice note: %+v %+v", voice, voice.Attachment)
	}
	if getFiles.Load() != 0 {
		t.Fatal("downloaded on the receive loop")
	}
	if data, err := voice.Attachment.Fetch(ctx, 1<<20); err != nil || string(data) != "OggS voice" {
		t.Fatalf("fetch: %q %v", data, err)
	}

	// Of the sizes offered, the largest the model sees in full.
	photo := send(`{"update_id":2,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"caption":"is this mould?","photo":[
		{"file_id":"p-90","width":90,"height":68},{"file_id":"p-320","width":320,"height":240},
		{"file_id":"p-1280","width":1280,"height":960},{"file_id":"p-2560","width":2560,"height":1920}]}}`)
	if photo.Media != channels.Photo || photo.Text != "is this mould?" || photo.Attachment == nil {
		t.Fatalf("photo: %+v", photo)
	}
	if data, err := photo.Attachment.Fetch(ctx, 1<<20); err != nil || !strings.HasPrefix(string(data), "\xff\xd8") || asked[len(asked)-1] != "p-1280" {
		t.Fatalf("photo fetch: %q %v (asked for %v)", data, err, asked)
	}

	// Too big: refused before anything is downloaded.
	before := getFiles.Load()
	big := send(`{"update_id":3,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"voice":{"file_id":"v-big","file_size":30000000}}}`)
	if _, err := big.Attachment.Fetch(ctx, 50<<20); !errors.Is(err, channels.ErrTooBig) || getFiles.Load() != before {
		t.Fatalf("over the Bot API limit: %v", err)
	}

	// A PDF is named, not fetched; a picture sent as a file is fetched.
	if pdf := send(`{"update_id":4,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"document":{"file_id":"d","mime_type":"application/pdf"}}}`); pdf.Media != channels.File || pdf.Attachment != nil {
		t.Fatalf("pdf: %+v", pdf)
	}
	if png := send(`{"update_id":5,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"document":{"file_id":"d","mime_type":"image/png"}}}`); png.Attachment == nil || png.Attachment.Mime != "image/png" {
		t.Fatalf("png document: %+v", png)
	}

	// A failed download says so without the bot token.
	gone := send(`{"update_id":6,"message":{"date":%d,"from":{"id":42},"chat":{"id":42,"type":"private"},"voice":{"file_id":"missing"}}}`)
	if _, err := gone.Attachment.Fetch(ctx, 1<<20); err == nil || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("missing file: %v", err)
	}
	srv.Close()
	if _, err := voice.Attachment.Fetch(ctx, 1<<20); err == nil || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("server down: %v", err)
	}
}
