// Package qr draws QR codes as SVG for the twin's pages: the "Add your phone"
// page's pairing link, drawn on this computer and never sent anywhere else.
//
// The SVG is plain markup (one path, no scripts, no external references),
// black on a white square with the four-module quiet zone scanners need,
// so it reads the same in a dark theme.
package qr

import (
	"errors"
	"fmt"
	"strings"

	"rsc.io/qr"
)

// Quiet is the white border, in modules, round every code.
const Quiet = 4

// ErrEmpty is returned for empty text: a code that says nothing is a bug.
var ErrEmpty = errors.New("qr: nothing to encode")

// Modules encodes text at medium error correction (15% of the code can be
// covered or smudged) and returns its grid, true for black, without the
// quiet zone.
func Modules(text string) ([][]bool, error) {
	if text == "" {
		return nil, ErrEmpty
	}
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, fmt.Errorf("qr: %w", err)
	}
	grid := make([][]bool, c.Size)
	for y := range grid {
		grid[y] = make([]bool, c.Size)
		for x := range grid[y] {
			grid[y][x] = c.Black(x, y)
		}
	}
	return grid, nil
}

// SVG draws text as a QR code px pixels square (the drawing scales; px is
// its natural size). Each row's runs of black become one rectangle in a
// single path, so a pairing link is a few kilobytes.
func SVG(text string, px int) ([]byte, error) {
	grid, err := Modules(text)
	if err != nil {
		return nil, err
	}
	if px <= 0 {
		px = 256
	}
	n := len(grid) + 2*Quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges" role="img" aria-label="QR code">`, n, n, px, px)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y, row := range grid {
		for x := 0; x < len(row); {
			if !row[x] {
				x++
				continue
			}
			start := x
			for x < len(row) && row[x] {
				x++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", start+Quiet, y+Quiet, x-start, x-start)
		}
	}
	b.WriteString(`"/></svg>`)
	return []byte(b.String()), nil
}
