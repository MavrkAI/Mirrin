package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// tgFile is the part of a Bot API voice, audio, photo size or document
// that says how to fetch it.
type tgFile struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	MimeType string `json:"mime_type"`
	Duration int    `json:"duration"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// botFileLimit is the largest file the Bot API lets a bot download.
const botFileLimit = 20 << 20

// attachment is how to fetch what came with m when the twin can take it in:
// a voice note or audio file, a photo, or a document that is one of those.
// Nil for anything else (a video, a sticker, a PDF).
func (c *Channel) attachment(m *message) *channels.Attachment {
	var f tgFile
	switch {
	case m.Voice != nil:
		_ = json.Unmarshal(m.Voice, &f)
		f.MimeType = or(f.MimeType, "audio/ogg")
	case m.Audio != nil:
		_ = json.Unmarshal(m.Audio, &f)
		f.MimeType = or(f.MimeType, "audio/mpeg")
	case m.Photo != nil:
		var sizes []tgFile
		_ = json.Unmarshal(m.Photo, &sizes)
		f = bestPhoto(sizes)
		f.MimeType = "image/jpeg"
	case m.Document != nil:
		_ = json.Unmarshal(m.Document, &f)
		if k := channels.MediaKind(f.MimeType); k != channels.Photo && k != channels.Voice {
			return nil
		}
	default:
		return nil
	}
	if f.FileID == "" {
		return nil
	}
	return &channels.Attachment{Mime: f.MimeType, Size: f.FileSize, Seconds: f.Duration,
		Fetch: func(ctx context.Context, max int64) ([]byte, error) { return c.download(ctx, f, max) }}
}

// bestPhoto picks, from the sizes Telegram offers, the largest the model
// looks at in full (long side at most channels.PhotoEdge), or the smallest.
func bestPhoto(sizes []tgFile) tgFile {
	var best tgFile
	for _, s := range sizes {
		long := max(s.Width, s.Height)
		fits, bestFits := long <= channels.PhotoEdge, max(best.Width, best.Height) <= channels.PhotoEdge
		switch {
		case best.FileID == "":
			best = s
		case fits && (!bestFits || long > max(best.Width, best.Height)):
			best = s
		case !fits && !bestFits && long < max(best.Width, best.Height):
			best = s
		}
	}
	return best
}

// download fetches a file through getFile, at most max bytes.
func (c *Channel) download(ctx context.Context, f tgFile, max int64) ([]byte, error) {
	if (max > 0 && f.FileSize > max) || f.FileSize > botFileLimit {
		return nil, channels.ErrTooBig
	}
	var got struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]any{"file_id": f.FileID}, &got); err != nil {
		return nil, err
	}
	if got.FilePath == "" {
		return nil, errors.New("Telegram didn't hand over the file")
	}
	if max > 0 && got.FileSize > max {
		return nil, channels.ErrTooBig
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+"/file/bot"+c.token+"/"+got.FilePath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL carries the bot token
		}
		return nil, fmt.Errorf("telegram file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram file: %s", resp.Status)
	}
	return channels.ReadCapped(resp.Body, max)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
