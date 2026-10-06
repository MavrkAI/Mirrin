package imessage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func (r row) attachment() *channels.Attachment {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return r.attachmentAt(filepath.Join(home, "Library", "Messages", "Attachments"))
}

// attachmentAt keeps tests off the real Messages directory.
func (r row) attachmentAt(root string) *channels.Attachment {
	if (!r.Attachments && !r.Audio) || r.Filename == "" {
		return nil
	}
	mime, ext := strings.ToLower(r.Mime), strings.ToLower(filepath.Ext(r.Filename))
	target := ""
	switch {
	case strings.Contains(mime, "caf") || ext == ".caf":
		target = "audio/wav"
	case mime == "image/heic" || mime == "image/heif" || mime == "image/tiff" || ext == ".heic" || ext == ".heif" || ext == ".tiff" || ext == ".tif":
		target = "image/jpeg"
	}
	if target != "" {
		mime = target
	}
	return &channels.Attachment{Mime: mime, Fetch: func(ctx context.Context, max int64) ([]byte, error) {
		data, err := waitAttachment(ctx, root, r.Filename, max, r.Transfer, r.recheck)
		if err != nil || target == "" {
			return data, err
		}
		return convertAttachment(ctx, data, target, max)
	}}
}

var convertAttachment = nativeConvertAttachment

// The database row can arrive before Messages finishes downloading the file,
// and the file can appear before its bytes do. An attachment is ready once
// Messages marks its transfer finished and the file holds total_bytes
// (state reads that from the database). When Messages recorded nothing
// about the transfer, it is ready once it's there, not empty, and the same
// size on two polls in a row. One still arriving when the wait ends is not
// handed on cut short. seen is the state read with the message row. Once
// Messages has recorded a state, a failed read of it (the database busy,
// say) means not ready yet, never a fall back to the size check.
func waitAttachment(ctx context.Context, root, name string, max int64, seen transfer, state func(context.Context) (transfer, error)) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	last := -1 // the size seen on the previous poll; -1 when there was no file
	ready := func(size int) bool {
		if state != nil {
			t, err := state(waitCtx)
			if err != nil {
				if seen.known() {
					return false
				}
			} else if t.known() {
				seen = t
				return t.done(size)
			} else if seen.known() {
				// Messages cleared what it recorded: go by the last state.
				return seen.done(size)
			}
		} else if seen.known() {
			return seen.done(size)
		}
		return size > 0 && size == last
	}
	for {
		data, err := readAttachment(waitCtx, root, name, max)
		switch {
		case errors.Is(err, os.ErrNotExist):
			last = -1
		case err != nil:
			return nil, err
		case ready(len(data)):
			return data, nil
		default:
			last = len(data)
		}
		timer := time.NewTimer(attachmentPoll)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return nil, waitCtx.Err()
		case <-timer.C:
		}
	}
}

// attachmentPoll is how often waitAttachment looks at the file again.
const attachmentPoll = 100 * time.Millisecond

// Messages owns this directory. Restrict database paths to it, resolving
// symlinks before opening, so an attachment can't name another private file.
func readAttachment(ctx context.Context, root, name string, max int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if max <= 0 || max > 25<<20 {
		max = 25 << 20
	}
	if strings.HasPrefix(name, "~/Library/Messages/Attachments/") {
		name = filepath.Join(root, strings.TrimPrefix(name, "~/Library/Messages/Attachments/"))
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("attachment is outside Messages")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("attachment is not a regular file")
	}
	if st.Size() > max {
		return nil, channels.ErrTooBig
	}
	return channels.ReadCapped(f, max)
}
