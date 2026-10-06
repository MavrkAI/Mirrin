package qr

import (
	"bytes"
	"encoding/xml"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const link = "https://twin.example.ts.net/pair#v=2&o=of_abcdefghij&s=0123456789abcdefghijABCDEFGHIJ-_0123456789a&n=Mirrin"

// The path drawn is exactly the code's black modules, offset by the quiet
// zone: nothing lost, nothing extra.
func TestSVGDrawsEveryBlackModule(t *testing.T) {
	grid, err := Modules(link)
	if err != nil {
		t.Fatal(err)
	}
	svg, err := SVG(link, 240)
	if err != nil {
		t.Fatal(err)
	}
	n := len(grid) + 2*Quiet
	drawn := make([][]bool, n)
	for i := range drawn {
		drawn[i] = make([]bool, n)
	}
	d := regexp.MustCompile(` d="([^"]*)"`).FindSubmatch(svg)
	if d == nil {
		t.Fatalf("no path in %s", svg)
	}
	for _, m := range regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`).FindAllStringSubmatch(string(d[1]), -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		w, _ := strconv.Atoi(m[3])
		if m[3] != m[4] {
			t.Fatalf("a run that doesn't close: %s", m[0])
		}
		for i := 0; i < w; i++ {
			if drawn[y][x+i] {
				t.Fatalf("module %d,%d drawn twice", x+i, y)
			}
			drawn[y][x+i] = true
		}
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			want := y >= Quiet && x >= Quiet && y < n-Quiet && x < n-Quiet && grid[y-Quiet][x-Quiet]
			if drawn[y][x] != want {
				t.Fatalf("module %d,%d: drawn %v, want %v", x, y, drawn[y][x], want)
			}
		}
	}
	// Well-formed, sized as asked, and with nothing a page would run.
	if err := xml.Unmarshal(svg, new(struct{})); err != nil {
		t.Fatalf("not well-formed: %v", err)
	}
	if !bytes.Contains(svg, []byte(`width="240" height="240"`)) || !bytes.Contains(svg, []byte(`viewBox="0 0 `+strconv.Itoa(n)+" "+strconv.Itoa(n)+`"`)) {
		t.Fatalf("size: %.200s", svg)
	}
	for _, bad := range []string{"<script", "href", "on", "url("} {
		if bad == "on" {
			if regexp.MustCompile(`\son\w+=`).Match(svg) {
				t.Fatal("an event handler in the SVG")
			}
			continue
		}
		if bytes.Contains(svg, []byte(bad)) {
			t.Fatalf("%q in the SVG", bad)
		}
	}
}

// The same text always draws the same code (the page compares codes to
// know when an offer changed).
func TestSVGIsDeterministic(t *testing.T) {
	a, _ := SVG(link, 0)
	b, _ := SVG(link, 0)
	if !bytes.Equal(a, b) || !bytes.Contains(a, []byte(`width="256"`)) {
		t.Fatal("two drawings of one link differ, or the default size isn't 256")
	}
	c, _ := SVG(link+"x", 0)
	if bytes.Equal(a, c) {
		t.Fatal("different links drew the same code")
	}
}

func TestSVGRefusesWhatItCantDraw(t *testing.T) {
	if _, err := SVG("", 100); !errors.Is(err, ErrEmpty) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := SVG(strings.Repeat("x", 4000), 100); err == nil {
		t.Fatal("4,000 bytes don't fit a QR code")
	}
}
