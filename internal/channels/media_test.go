package channels

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func encode(t *testing.T, img image.Image, format string) []byte {
	t.Helper()
	var buf bytes.Buffer
	var err error
	switch format {
	case "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95})
	case "png":
		err = png.Encode(&buf, img)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func noisy(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	for i := range img.Pix {
		seed = seed*1664525 + 1013904223
		img.Pix[i] = uint8(seed >> 24)
	}
	return img
}

func TestFitImage(t *testing.T) {
	// A big photo is scaled to what the model looks at and kept under the size cap.
	fit, mime, err := FitImage(encode(t, noisy(3000, 2000), "jpeg"), PhotoEdge, PhotoBytes)
	if err != nil || mime != "image/jpeg" || len(fit) > PhotoBytes {
		t.Fatalf("big photo: %s, %d bytes, %v", mime, len(fit), err)
	}
	if cfg, _, _ := image.DecodeConfig(bytes.NewReader(fit)); cfg.Width != PhotoEdge || cfg.Height != 1045 {
		t.Fatalf("scaled to %dx%d", cfg.Width, cfg.Height)
	}
	// One that fits already is passed through as it is.
	small := encode(t, noisy(40, 30), "png")
	if fit, mime, err := FitImage(small, PhotoEdge, PhotoBytes); err != nil || mime != "image/png" || !bytes.Equal(fit, small) {
		t.Fatalf("small png: %s %v", mime, err)
	}
	// Too many bytes at a fitting size: re-encoded smaller.
	if fit, _, err := FitImage(encode(t, noisy(1200, 1200), "png"), PhotoEdge, 400_000); err != nil || len(fit) > 400_000 {
		t.Fatalf("heavy png: %d bytes, %v", len(fit), err)
	}
	// Transparency turns white, not black.
	clear := image.NewNRGBA(image.Rect(0, 0, 2000, 10))
	clear.Set(0, 0, color.NRGBA{255, 0, 0, 255})
	fit, _, err = FitImage(encode(t, clear, "png"), PhotoEdge, PhotoBytes)
	if err != nil {
		t.Fatal(err)
	}
	img, _, _ := image.Decode(bytes.NewReader(fit))
	if r, g, b, _ := img.At(1000, 3).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("transparent area came out %d,%d,%d", r>>8, g>>8, b>>8)
	}
	// GIF and WebP go as they are while they fit.
	g := encode(t, noisy(20, 20), "gif")
	if _, mime, err := FitImage(g, PhotoEdge, PhotoBytes); err != nil || mime != "image/gif" {
		t.Fatalf("gif: %s %v", mime, err)
	}
	if _, _, err := FitImage(g, PhotoEdge, 10); !errors.Is(err, ErrTooBig) {
		t.Fatalf("big gif: %v", err)
	}
	if fit, mime, err := FitImage(encode(t, noisy(2000, 100), "gif"), PhotoEdge, PhotoBytes); err != nil || mime != "image/jpeg" || len(fit) == 0 {
		t.Fatalf("wide gif: %s %v", mime, err)
	}
	for _, tt := range []struct {
		name string
		webp []byte
		err  error
	}{
		{"lossy", webpHeader("VP8 ", 800, 600), nil},
		{"lossless", webpHeader("VP8L", 1024, 768), nil},
		{"extended", webpHeader("VP8X", 1200, 900), nil},
		{"too wide for any model", webpHeader("VP8X", 9000, 100), ErrTooBig},
		{"not really", append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), make([]byte, 20)...), ErrNotAPicture},
	} {
		if _, mime, err := FitImage(tt.webp, PhotoEdge, PhotoBytes); !errors.Is(err, tt.err) || (tt.err == nil && mime != "image/webp") {
			t.Errorf("webp %s: %s %v", tt.name, mime, err)
		}
	}
	// A damaged JPEG is refused here, not by the model mid-conversation.
	whole := encode(t, noisy(64, 64), "jpeg")
	if _, _, err := FitImage(whole[:len(whole)/2], PhotoEdge, PhotoBytes); !errors.Is(err, ErrNotAPicture) {
		t.Fatalf("truncated jpeg: %v", err)
	}
	// Not a picture (a HEIC, a PDF), or one claiming to be enormous.
	for _, data := range [][]byte{[]byte("\x00\x00\x00\x18ftypheic"), []byte("%PDF-1.7"), nil} {
		if _, _, err := FitImage(data, PhotoEdge, PhotoBytes); !errors.Is(err, ErrNotAPicture) {
			t.Errorf("%q: %v", data, err)
		}
	}
	if _, _, err := FitImage(pngClaiming(100_000, 100_000), PhotoEdge, PhotoBytes); !errors.Is(err, ErrTooBig) {
		t.Fatalf("decompression bomb: %v", err)
	}
}

// webpHeader is the start of a WebP of the given kind and size.
func webpHeader(kind string, w, h int) []byte {
	b := make([]byte, 40)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 32)
	copy(b[8:], "WEBP"+kind)
	switch kind {
	case "VP8 ":
		b[23], b[24], b[25] = 0x9d, 0x01, 0x2a
		binary.LittleEndian.PutUint16(b[26:], uint16(w))
		binary.LittleEndian.PutUint16(b[28:], uint16(h))
	case "VP8L":
		b[20] = 0x2f
		binary.LittleEndian.PutUint32(b[21:], uint32(w-1)|uint32(h-1)<<14)
	case "VP8X":
		b[24], b[25], b[26] = byte(w-1), byte((w-1)>>8), byte((w-1)>>16)
		b[27], b[28], b[29] = byte(h-1), byte((h-1)>>8), byte((h-1)>>16)
	}
	return b
}

// pngClaiming is a PNG header for a w×h picture with no pixels behind it.
func pngClaiming(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func TestReadCapped(t *testing.T) {
	if b, err := ReadCapped(strings.NewReader("12345"), 5); err != nil || string(b) != "12345" {
		t.Fatalf("at the cap: %q %v", b, err)
	}
	if _, err := ReadCapped(strings.NewReader("123456"), 5); !errors.Is(err, ErrTooBig) {
		t.Fatalf("past the cap: %v", err)
	}
}

func TestMediaNotes(t *testing.T) {
	if got := VoiceNote(" call Mum at five ", ""); got != "(voice note) call Mum at five" {
		t.Fatalf("VoiceNote: %q", got)
	}
	if got := VoiceNote("hi", "urgent"); got != "(voice note) hi\nurgent" {
		t.Fatalf("VoiceNote with caption: %q", got)
	}
	if PhotoNote("") != "(photo)" || PhotoNote(" is this mould? ") != "(photo) is this mould?" {
		t.Fatal("PhotoNote")
	}
	if got := Unopened(Voice, "it couldn't be transcribed"); got != "[A voice message arrived, but it couldn't be transcribed.]" {
		t.Fatalf("Unopened: %q", got)
	}
}
