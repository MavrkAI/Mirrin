package llm

import (
	"encoding/base64"
	"path"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// BlockImage is a picture the user sent (a photo over WhatsApp), on a user
// message. History keeps only where it is (Path, relative to the data
// folder); the agent loads Image and ImageType before each model call, and
// a provider sends only an image block that has them.
const BlockImage BlockType = "image"

// ImageBlock is a picture kept at path, relative to the data folder.
func ImageBlock(path, mime string) Block {
	return Block{Type: BlockImage, Path: path, ImageType: mime}
}

// ImageTypeOf is the MIME type of a picture kept at p, by its extension.
func ImageTypeOf(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// pictureTrouble are what providers say when a picture in a request is the
// problem: too large, unreadable, or a model that takes none.
var pictureTrouble = []string{
	"image_url", "image input", "input image", "image exceeds", "image dimensions", "image.source",
	"could not process image", "unable to process image", "invalid image", "unsupported image",
	"image is not valid", "does not support image", "missing data required for image",
}

// ImageRefused reports whether err is the model turning a request down over
// a picture in it, so the picture can be left out from then on. A model
// that can't use tools is not a picture problem, even when its name says
// "vision".
func ImageRefused(err error) bool {
	if err == nil {
		return false
	}
	// A request turned down as it is, not a busy or failing service; Ollama
	// answers 500 to a picture its model can't take.
	_, status, body := httpFailure(err)
	switch status {
	case 400, 413, 415, 422, 500:
	default:
		return false
	}
	b := strings.ToLower(body)
	if strings.Contains(b, "tool") {
		return false
	}
	for _, p := range pictureTrouble {
		if strings.Contains(b, p) {
			return true
		}
	}
	return false
}

// reImageAt finds where Claude says the refused picture is:
// "messages.2.content.0.image…" for a picture in a message, or
// "messages.4.content.0.content.1.image…" for one a tool returned.
var reImageAt = regexp.MustCompile(`messages\.\d+\.content\.\d+\.(content\.\d+\.)?image`)

// Where a refused picture is, as far as the error says.
const (
	ImageInMessage = "message" // one sent with a message (a photo)
	ImageFromTool  = "tool"    // one a tool returned (a screenshot)
)

// RefusedImageIn says which kind of picture err, a refusal ImageRefused
// recognises, is about: ImageInMessage, ImageFromTool, or "" when the
// provider doesn't say.
func RefusedImageIn(err error) string {
	if err == nil {
		return ""
	}
	_, _, body := httpFailure(err)
	m := reImageAt.FindStringSubmatch(body)
	switch {
	case m == nil:
		return ""
	case m[1] != "":
		return ImageFromTool
	}
	return ImageInMessage
}

// imageParam is a loaded image block as Claude takes it.
func imageParam(b Block) (anthropic.ContentBlockParamUnion, bool) {
	if len(b.Image) == 0 || b.ImageType == "" {
		return anthropic.ContentBlockParamUnion{}, false
	}
	return anthropic.NewImageBlockBase64(b.ImageType, base64.StdEncoding.EncodeToString(b.Image)), true
}

// userContent is a user turn in the chat-completions dialect: plain text,
// or text and pictures as content parts when images came with it.
func userContent(text string, images []Block) oaMessage {
	var parts []map[string]any
	for _, b := range images {
		if len(b.Image) == 0 || b.ImageType == "" {
			continue
		}
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{
			"url": "data:" + b.ImageType + ";base64," + base64.StdEncoding.EncodeToString(b.Image)}})
	}
	if len(parts) == 0 {
		return oaMessage{Role: "user", Content: text}
	}
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	return oaMessage{Role: "user", Content: parts}
}
