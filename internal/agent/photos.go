package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Photos someone sends the twin go to the model with the message they came
// with, and stay in the conversation as image blocks that name the file
// under the data folder; the bytes are loaded again for each model call.

type photosKey struct{}

// photoSet is a turn's photos; only the turn's first message takes them.
type photoSet struct {
	blocks []llm.Block
	taken  atomic.Bool
}

// WithPhotos attaches pictures to the message the turn in ctx answers. Each
// path is where the picture is kept, relative to the data folder
// ("media/2026-09/3f2a….jpg").
func WithPhotos(ctx context.Context, paths ...string) context.Context {
	var blocks []llm.Block
	for _, p := range paths {
		p = filepath.ToSlash(p)
		blocks = append(blocks, llm.ImageBlock(p, llm.ImageTypeOf(p)))
	}
	if len(blocks) == 0 {
		return ctx
	}
	return context.WithValue(ctx, photosKey{}, &photoSet{blocks: blocks})
}

// CopyPhotos puts the photos of src's turn on dst, for another run that
// answers the same message: a background task the owner's reply (with a
// picture) answers. The copy is fresh, so that run's first message takes
// them whether or not this turn has.
func CopyPhotos(dst, src context.Context) context.Context {
	set, ok := src.Value(photosKey{}).(*photoSet)
	if !ok || len(set.blocks) == 0 {
		return dst
	}
	return context.WithValue(dst, photosKey{}, &photoSet{blocks: set.blocks})
}

// HasPhotos reports whether the turn in ctx came with photos.
func HasPhotos(ctx context.Context) bool {
	set, ok := ctx.Value(photosKey{}).(*photoSet)
	return ok && len(set.blocks) > 0
}

// withPhotos puts the turn's photos on m, the message it answers, ahead of
// its text. Anything else the turn runs (an approval carried out, a task
// resumed) doesn't get them again.
func withPhotos(ctx context.Context, m llm.Message) llm.Message {
	set, ok := ctx.Value(photosKey{}).(*photoSet)
	if !ok || m.Role != llm.RoleUser || !set.taken.CompareAndSwap(false, true) {
		return m
	}
	m.Blocks = append(append([]llm.Block(nil), set.blocks...), m.Blocks...)
	return m
}

// recentPhotos is how many of the latest photos in view the model is shown,
// and photoTurns how long: while the message a photo came with is among the
// last few the person sent. Older ones are described instead, so every turn
// after a photo doesn't pay for it again (a local model encodes it anew each
// time).
const (
	recentPhotos = 3
	photoTurns   = 3
)

// Lines that stand in for a photo the model isn't shown, so it never
// guesses at one.
const (
	photoUnseen  = "[A photo came with this message, but the model you're running on can't see pictures. Say so if it matters, and ask what's in it.]"
	photoEarlier = "[A photo was here; it is no longer shown to you. If you need to look at it again, ask them to send it again.]"
	photoGone    = "[A photo was here, but it is no longer on this computer.]"
)

// photoRefused stands in for a photo the model turned down.
const photoRefused = "[A photo was here, but the model couldn't open it.]"

// refused holds the photos (by full path) a model turned down, so one bad
// picture can't fail every turn after it.
var (
	refusedMu sync.Mutex
	refused   = map[string]bool{}
)

// photosRefused is err, the failure of a model call with msgs, unless the
// model turned it down over the photos in it: then those photos are left
// out from now on, and the error says so and what to do.
func (a *Agent) photosRefused(msgs []llm.Message, err error) error {
	if !llm.ImageRefused(err) {
		return err
	}
	var photos []string
	screenshots := false
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockImage && len(b.Image) > 0:
				photos = append(photos, filepath.Join(a.cfg.DataDir, filepath.FromSlash(b.Path)))
			case b.Type == llm.BlockToolResult && len(b.Image) > 0:
				screenshots = true
			}
		}
	}
	// Only a refusal that is about the photos puts them aside: the error
	// points at one, or no tool's picture (a screenshot) is in view that it
	// could be about instead.
	switch llm.RefusedImageIn(err) {
	case llm.ImageFromTool:
		return err
	case "":
		if screenshots {
			return err
		}
	}
	if len(photos) == 0 {
		return err
	}
	refusedMu.Lock()
	for _, p := range photos {
		refused[p] = true
	}
	refusedMu.Unlock()
	return &llm.UserError{Err: err, Msg: "The model couldn't open that photo, so I've put it aside. Send your message again and I'll answer without it, or send the photo as an ordinary JPEG."}
}

// showPhotos loads the pictures behind the most recent image blocks in
// history and turns every other image block into a line saying why it isn't
// shown. It runs on the copy of history made for one model call.
func (a *Agent) showPhotos(ctx context.Context, provider llm.Provider, history []llm.Message) []llm.Message {
	sees, asked, shown, turns := false, false, 0, 0
	for mi := len(history) - 1; mi >= 0; mi-- {
		if sentByThem(history[mi]) {
			turns++
		}
		for bi := range history[mi].Blocks {
			b := &history[mi].Blocks[bi]
			if b.Type != llm.BlockImage {
				continue
			}
			if !asked {
				sees, asked = llm.SeesImages(ctx, provider), true
			}
			refusedMu.Lock()
			turnedDown := refused[filepath.Join(a.cfg.DataDir, filepath.FromSlash(b.Path))]
			refusedMu.Unlock()
			switch {
			case !sees:
				*b = llm.Block{Type: llm.BlockText, Text: photoUnseen}
			case turnedDown:
				*b = llm.Block{Type: llm.BlockText, Text: photoRefused}
			case shown >= recentPhotos || turns > photoTurns:
				*b = llm.Block{Type: llm.BlockText, Text: photoEarlier}
			default:
				data, mime, err := loadPhoto(a.cfg.DataDir, b.Path)
				if err != nil {
					*b = llm.Block{Type: llm.BlockText, Text: photoGone}
					continue
				}
				b.Image, b.ImageType = data, mime
				shown++
			}
		}
	}
	return history
}

// sentByThem reports whether m is a message the person sent (or one said on
// their behalf), not tool results handed back in the same turn.
func sentByThem(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	for _, b := range m.Blocks {
		if b.Type != llm.BlockToolResult {
			return true
		}
	}
	return false
}

// loadPhoto reads a photo kept under dataDir/media, checking that it is one
// (a picture the model takes, no larger than a fitted photo).
func loadPhoto(dataDir, rel string) ([]byte, string, error) {
	rel = filepath.FromSlash(rel)
	if dataDir == "" || !filepath.IsLocal(rel) || !strings.HasPrefix(rel, "media"+string(filepath.Separator)) {
		return nil, "", errors.New("not a photo the twin kept")
	}
	path := filepath.Join(dataDir, rel)
	st, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !st.Mode().IsRegular() || st.Size() > channels.PhotoBytes {
		return nil, "", errors.New("not a photo the twin kept")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	data, err := channels.ReadCapped(f, channels.PhotoBytes)
	if err != nil {
		return nil, "", err
	}
	switch mime := http.DetectContentType(data); mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return data, mime, nil
	}
	return nil, "", channels.ErrNotAPicture
}
