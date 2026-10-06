//go:build !nowhatsapp

package whatsapp

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// attachment names what came with a message and, for what the twin can
// take in (a voice note, a photo), how to fetch it. The download happens
// when the daemon gets to the message, never on WhatsApp's event loop.
// Stickers and reactions are not attachments; videos and other files are
// named but not fetched.
func (c *Channel) attachment(m *waE2E.Message) (kind string, att *channels.Attachment) {
	switch {
	case m == nil:
		return "", nil
	case m.GetAudioMessage() != nil:
		a := m.GetAudioMessage()
		return channels.Voice, c.fetchable(a, a.GetMimetype(), a.GetFileLength(), a.GetSeconds())
	case m.GetImageMessage() != nil:
		i := m.GetImageMessage()
		return channels.Photo, c.fetchable(i, i.GetMimetype(), i.GetFileLength(), 0)
	case m.GetVideoMessage() != nil, m.GetPtvMessage() != nil:
		return channels.Video, nil
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		// A photo or recording sent as a document is still one.
		switch kind := channels.MediaKind(d.GetMimetype()); kind {
		case channels.Photo, channels.Voice:
			return kind, c.fetchable(d, d.GetMimetype(), d.GetFileLength(), 0)
		}
		return channels.File, nil
	}
	return "", nil
}

// fetchable is an Attachment that downloads msg through the channel's
// client when the daemon asks for it.
func (c *Channel) fetchable(msg whatsmeow.DownloadableMessage, mime string, size uint64, seconds uint32) *channels.Attachment {
	return &channels.Attachment{
		Mime:    mime,
		Size:    int64(size),
		Seconds: int(seconds),
		Fetch: func(ctx context.Context, max int64) ([]byte, error) {
			if max > 0 && size > uint64(max) {
				return nil, channels.ErrTooBig
			}
			data, err := c.fetch(ctx, msg)
			if err != nil {
				return nil, err
			}
			if max > 0 && int64(len(data)) > max {
				return nil, channels.ErrTooBig
			}
			return data, nil
		},
	}
}

func (c *Channel) fetch(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	if c.download != nil {
		return c.download(ctx, msg)
	}
	cl, err := c.connected()
	if err != nil {
		return nil, err
	}
	return cl.Download(ctx, msg)
}
