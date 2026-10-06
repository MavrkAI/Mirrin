package channels

import (
	"context"
	"errors"
	"io"
	"strings"
)

// Kinds of attachment a channel can receive. A channel puts one in
// Inbound.Media; the daemon decides what to do with it.
const (
	Voice = "voice"
	Photo = "photo"
	Video = "video"
	File  = "file"
)

// Attachment is the file behind Inbound.Media. A transport that can hand
// over the bytes sets one, and the daemon fetches it when it gets to the
// message, so a download never holds up the transport's receive loop.
type Attachment struct {
	// Mime is the type the platform gave it ("audio/ogg; codecs=opus"), if any.
	Mime string
	// Size is its length in bytes as the platform reported it (0 if unknown).
	Size int64
	// Seconds is how long a voice note runs (0 if unknown).
	Seconds int
	// Fetch downloads it. Past max bytes it stops and returns ErrTooBig,
	// without downloading at all when the platform said how big it is.
	Fetch func(ctx context.Context, max int64) ([]byte, error)
}

// ErrTooBig is a file larger than the twin takes in.
var ErrTooBig = errors.New("the file is too large")

// ReadCapped reads all of r, or returns ErrTooBig once it passes max bytes.
func ReadCapped(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooBig
	}
	return data, nil
}

// MediaKind names an attachment by its MIME type.
func MediaKind(mime string) string {
	switch mime = strings.ToLower(mime); {
	case strings.HasPrefix(mime, "audio/"):
		return Voice
	case strings.HasPrefix(mime, "image/"):
		return Photo
	case strings.HasPrefix(mime, "video/"):
		return Video
	}
	return File
}

// CantOpen is the reply to a message that is only something the twin can't
// take in on this channel yet (a voice note, a photo), so it is answered
// rather than met with silence.
func CantOpen(kind string) string {
	switch kind {
	case Voice:
		return "I can't listen to voice messages here yet. Could you type it for me?"
	case Photo:
		return "I can't see photos here yet. Could you tell me in words what you need?"
	case Video:
		return "I can't watch videos here yet. Could you tell me in words what you need?"
	}
	return "I can't open files here yet. Could you paste the text, or tell me what you need?"
}

// WithAttachment notes on a message's text that it came with something the
// model can't see, so it says so instead of guessing at it.
func WithAttachment(text, kind string) string {
	return strings.TrimSpace(text) + "\n[" + attachment(kind) + " came with this message; you can't open it on this channel yet.]"
}

// Received stands in for a message that was only an attachment, in the
// conversation and the audit log, so a follow-up ("did you get my voice
// note?") has something to refer to.
func Received(kind string) string {
	return "[" + attachment(kind) + " arrived, which you can't open on this channel yet.]"
}

// Unopened is Received for an attachment the twin tried to open and
// couldn't; why finishes "…arrived, but " ("it couldn't be transcribed").
func Unopened(kind, why string) string {
	return "[" + attachment(kind) + " arrived, but " + why + ".]"
}

// voiceNoteMark starts every transcribed voice note (VoiceNote).
const voiceNoteMark = "(voice note)"

// VoiceNote is what a transcribed voice note is answered as: the words,
// marked so the model knows they were spoken (and may be misheard).
func VoiceNote(transcript, caption string) string {
	s := voiceNoteMark + " " + strings.TrimSpace(transcript)
	if c := strings.TrimSpace(caption); c != "" {
		s += "\n" + c
	}
	return s
}

// IsVoiceNote reports whether text is a transcribed voice note (VoiceNote):
// words that were heard, not typed. A transcript is a guess at what was
// said, so it never approves anything.
func IsVoiceNote(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), voiceNoteMark)
}

// VoiceNoteWords is what a transcribed voice note says, without its mark;
// text that isn't one is returned as it is.
func VoiceNoteWords(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, voiceNoteMark) {
		return text
	}
	return strings.TrimSpace(strings.TrimPrefix(t, voiceNoteMark))
}

// photoMark starts every photo's message (PhotoNote).
const photoMark = "(photo)"

// IsPhotoNote reports whether text is what a photo was answered as (PhotoNote).
func IsPhotoNote(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), photoMark)
}

// PhotoNote is what a photo is answered as; the picture itself goes to the
// model alongside it.
func PhotoNote(caption string) string {
	if c := strings.TrimSpace(caption); c != "" {
		return photoMark + " " + c
	}
	return photoMark
}

func attachment(kind string) string {
	switch kind {
	case Voice:
		return "A voice message"
	case Photo:
		return "A photo"
	case Video:
		return "A video"
	}
	return "A file"
}
