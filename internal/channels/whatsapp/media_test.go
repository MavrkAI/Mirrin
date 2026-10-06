//go:build !nowhatsapp

package whatsapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// fromOwner is a message in the owner's "message yourself" chat.
func fromOwner(m *waE2E.Message) *events.Message {
	self := types.NewJID("61400000001", types.DefaultUserServer)
	return &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: self, Sender: self, IsFromMe: true}, Timestamp: time.Now()},
		Message: m,
	}
}

// Voice notes and photos reach the daemon with a way to fetch them; the
// transport never downloads on its event loop, and never answers itself.
func TestVoiceNotesAndPhotosReachTheDaemon(t *testing.T) {
	c := New(t.TempDir(), "+61400000001", false, nil)
	self := []types.JID{types.NewJID("61400000001", types.DefaultUserServer)}
	var fetched []string
	c.download = func(_ context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
		switch msg.(type) {
		case *waE2E.AudioMessage:
			fetched = append(fetched, "audio")
			return []byte("OggS voice"), nil
		case *waE2E.ImageMessage:
			fetched = append(fetched, "image")
			return []byte("\xff\xd8\xff photo"), nil
		}
		return nil, errors.New("unexpected download")
	}
	voice := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg; codecs=opus"), PTT: proto.Bool(true),
		Seconds: proto.Uint32(7), FileLength: proto.Uint64(9000), DirectPath: proto.String("/v/t62")}}
	photo := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), Caption: proto.String("is this mould?"),
		FileLength: proto.Uint64(120000), DirectPath: proto.String("/v/t62")}}
	tests := []struct {
		name  string
		msg   *waE2E.Message
		text  string
		media string
		fetch bool
	}{
		{"voice note", voice, "", channels.Voice, true},
		{"photo with caption", photo, "is this mould?", channels.Photo, true},
		{"photo as a document", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Mimetype: proto.String("image/png"), FileLength: proto.Uint64(10)}}, "", channels.Photo, true},
		{"pdf", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Mimetype: proto.String("application/pdf"), Caption: proto.String("my lease")}}, "my lease", channels.File, false},
		{"video", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("look")}}, "look", channels.Video, false},
		{"text", &waE2E.Message{Conversation: proto.String("hello")}, "hello", "", false},
	}
	for _, tt := range tests {
		in, ok := c.inbound(fromOwner(tt.msg), self)
		if !ok || in.Text != tt.text || in.Media != tt.media || !in.IsOwner || in.ChatID != c.OwnerChatID() {
			t.Errorf("%s: %+v (heard %v)", tt.name, in, ok)
			continue
		}
		if (in.Attachment != nil) != tt.fetch {
			t.Errorf("%s: attachment %+v", tt.name, in.Attachment)
		}
	}
	if len(fetched) != 0 {
		t.Fatalf("downloaded on the event loop: %v", fetched)
	}
	in, _ := c.inbound(fromOwner(voice), self)
	if in.Attachment.Seconds != 7 || in.Attachment.Mime != "audio/ogg; codecs=opus" {
		t.Fatalf("voice note: %+v", in.Attachment)
	}
	data, err := in.Attachment.Fetch(context.Background(), 1<<20)
	if err != nil || string(data) != "OggS voice" {
		t.Fatalf("fetch: %q %v", data, err)
	}
	// Past the cap it isn't downloaded at all.
	if _, err := in.Attachment.Fetch(context.Background(), 100); !errors.Is(err, channels.ErrTooBig) || len(fetched) != 1 {
		t.Fatalf("too big: %v, downloads %v", err, fetched)
	}
	// A sticker is not something to answer.
	if _, ok := c.inbound(fromOwner(&waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp")}}), self); ok {
		t.Fatal("a sticker reached the daemon")
	}
	// A voice note the owner sent a friend is not for the twin.
	friend := fromOwner(voice)
	friend.Info.Chat = types.NewJID("61400000002", types.DefaultUserServer)
	if _, ok := c.inbound(friend, self); ok {
		t.Fatal("a voice note to a friend reached the twin")
	}
}

// Fetching after the channel lost its connection fails plainly.
func TestFetchNeedsAConnection(t *testing.T) {
	c := New(t.TempDir(), "+61400000001", false, nil)
	_, att := c.attachment(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg")}})
	if _, err := att.Fetch(context.Background(), 1<<20); err == nil {
		t.Fatal("fetched without a connection")
	}
}
