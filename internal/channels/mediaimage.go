package channels

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/draw"
	_ "image/gif" // decode GIFs to check and scale them
	"image/jpeg"
	_ "image/png" // decode PNG photos and screenshots
	"net/http"
)

// A photo the model looks at is kept small: models scale anything with a
// long side past about 1,568 pixels down anyway, and 3.5 MB stays inside
// every provider's limit once it is base64-encoded.
const (
	PhotoEdge  = 1568
	PhotoBytes = 3_500_000
	// maxPixels refuses to decode anything larger (a decompression bomb).
	maxPixels = 40_000_000
	// maxWebPEdge is the longest side a model accepts; a WebP (which the
	// standard library can't scale) must be within it.
	maxWebPEdge = 8000
)

// ErrNotAPicture is a file that isn't a JPEG, PNG, GIF or WebP, or is one
// that can't be read.
var ErrNotAPicture = errors.New("not a kind of picture the twin can open")

// FitImage returns data as a picture every model accepts: JPEG, PNG, GIF or
// WebP, at most maxBytes, its long side at most maxEdge pixels, and its MIME
// type. A JPEG, PNG or GIF is read in full, so a damaged one is refused here
// rather than by the model, and one that is larger is scaled down and saved
// as JPEG (a GIF keeps its first frame). A WebP must already fit.
func FitImage(data []byte, maxEdge, maxBytes int) ([]byte, string, error) {
	mime := http.DetectContentType(data)
	switch mime {
	case "image/webp":
		w, h, ok := webpSize(data)
		switch {
		case !ok:
			return nil, "", ErrNotAPicture
		case len(data) > maxBytes || max(w, h) > maxWebPEdge:
			return nil, "", ErrTooBig
		}
		return data, mime, nil
	case "image/jpeg", "image/png", "image/gif":
	default:
		return nil, "", ErrNotAPicture
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", ErrNotAPicture
	}
	if cfg.Width*cfg.Height > maxPixels {
		return nil, "", ErrTooBig
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", ErrNotAPicture
	}
	if max(cfg.Width, cfg.Height) <= maxEdge && len(data) <= maxBytes {
		return data, mime, nil
	}
	flat := flatten(img)
	for edge := maxEdge; edge >= 200; edge = edge * 3 / 4 {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, shrink(flat, edge), &jpeg.Options{Quality: 85}); err != nil {
			return nil, "", err
		}
		if buf.Len() <= maxBytes {
			return buf.Bytes(), "image/jpeg", nil
		}
	}
	return nil, "", ErrTooBig
}

// webpSize reads a WebP's width and height from its header (lossy, lossless
// or extended) without decoding it.
func webpSize(b []byte) (w, h int, ok bool) {
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8 ":
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, false
		}
		w = int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
		h = int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
	case "VP8L":
		if b[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		w, h = int(bits&0x3fff)+1, int(bits>>14&0x3fff)+1
	case "VP8X":
		w = int(uint32(b[24])|uint32(b[25])<<8|uint32(b[26])<<16) + 1
		h = int(uint32(b[27])|uint32(b[28])<<8|uint32(b[29])<<16) + 1
	default:
		return 0, 0, false
	}
	return w, h, w > 0 && h > 0
}

// flatten draws img onto white, so transparent parts of a PNG don't turn
// black as JPEG.
func flatten(img image.Image) *image.RGBA {
	b := img.Bounds()
	flat := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, b.Min, draw.Over)
	return flat
}

// shrink scales src so its long side is at most edge pixels, averaging the
// pixels each new one covers (a box filter: sharp enough for text in a
// screenshot, no aliasing on a photo).
func shrink(src *image.RGBA, edge int) *image.RGBA {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	if max(w, h) <= edge {
		return src
	}
	nw, nh := max(1, w*edge/max(w, h)), max(1, h*edge/max(w, h))
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0 := y * h / nh
		y1 := max((y+1)*h/nh, y0+1)
		for x := 0; x < nw; x++ {
			x0 := x * w / nw
			x1 := max((x+1)*w/nw, x0+1)
			var r, g, b, n uint32
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+3]
					r, g, b, n = r+uint32(p[0]), g+uint32(p[1]), b+uint32(p[2]), n+1
				}
			}
			d := dst.Pix[y*dst.Stride+x*4 : y*dst.Stride+x*4+4]
			d[0], d[1], d[2], d[3] = uint8(r/n), uint8(g/n), uint8(b/n), 255
		}
	}
	return dst
}
