package pagetest

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	rootBlock  = regexp.MustCompile(`(?s):root\s*\{(.*?)\}`)
	lightBlock = regexp.MustCompile(`(?s)@media\s*\(prefers-color-scheme:\s*light\)\s*\{\s*:root\s*\{(.*?)\}`)
	colorVar   = regexp.MustCompile(`--([a-z0-9-]+)\s*:\s*(#[0-9a-fA-F]{3}(?:[0-9a-fA-F]{3})?)\b`)
)

func tokens(block string) map[string]string {
	out := map[string]string{}
	for _, m := range colorVar.FindAllStringSubmatch(block, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// themes returns each theme's colour tokens: dark always, light when the page has one.
func themes(t *testing.T, html string) map[string]map[string]string {
	t.Helper()
	root := rootBlock.FindStringSubmatch(html)
	if root == nil {
		t.Fatal("no :root colour tokens")
	}
	dark := tokens(root[1])
	out := map[string]map[string]string{"dark": dark}
	if l := lightBlock.FindStringSubmatch(html); l != nil {
		light := map[string]string{}
		for k, v := range dark {
			light[k] = v
		}
		for k, v := range tokens(l[1]) {
			light[k] = v
		}
		out["light"] = light
	}
	return out
}

func luminance(hex string) float64 {
	h := strings.TrimPrefix(hex, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	ch := func(i int) float64 {
		v, _ := strconv.ParseUint(h[i:i+2], 16, 8)
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(0) + 0.7152*ch(2) + 0.0722*ch(4)
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Every colour used for words must read at WCAG AA (4.5:1) on every surface
// it sits on, in every theme the page has. The screen's old --dim (2.9:1) and
// the settings pages' light-theme green, amber, red and blue failed this.
func TestTextColoursMeetAA(t *testing.T) {
	type pair struct{ fg, bg string }
	cross := func(fgs, bgs []string) []pair {
		var ps []pair
		for _, f := range fgs {
			for _, b := range bgs {
				ps = append(ps, pair{f, b})
			}
		}
		return ps
	}
	settings := append(cross([]string{"fg", "muted", "accent", "ok", "warn", "fail"}, []string{"bg", "card"}), pair{"on-accent", "accent"})
	pages := map[string][]pair{
		"../ui.html": append(cross([]string{"fg", "muted", "dim", "accent", "ok", "warn", "fail"}, []string{"bg", "bg2", "card", "card2"}),
			pair{"on-accent", "accent"}, pair{"on-ok", "ok"}, pair{"on-warn", "warn"}),
		"../health.html":    settings,
		"../memory.html":    settings,
		"../channels.html":  settings,
		"../accounts.html":  settings,
		"../protocols.html": settings,
		// the device and safety pages; "Add your phone" puts a tick in --card on --ok
		"../devices_add.html":    append(append([]pair{}, settings...), pair{"card", "ok"}),
		"../devices.html":        settings,
		"../backup.html":         settings,
		"../trust.html":          settings,
		"../spending.html":       settings,
		"../restore_review.html": settings,
		"../reach.html":          settings,
		"../voice_setup.html":    settings,
	}
	for file, pairs := range pages {
		html := readPage(t, file)
		if !strings.Contains(html, "color-scheme:") {
			t.Errorf("%s: no color-scheme, so native controls ignore the theme", file)
		}
		for theme, tok := range themes(t, html) {
			for _, p := range pairs {
				fg, bg := tok[p.fg], tok[p.bg]
				if fg == "" || bg == "" {
					t.Errorf("%s (%s): missing --%s or --%s", file, theme, p.fg, p.bg)
					continue
				}
				if c := contrast(fg, bg); c < 4.5 {
					t.Errorf("%s (%s): --%s %s on --%s %s is %.2f:1, below 4.5:1", file, theme, p.fg, fg, p.bg, bg, c)
				}
			}
			// A text field's border must stand out from what is around it (WCAG 1.4.11, 3:1).
			for _, b := range []string{"bg", "card"} {
				if tok["edge"] == "" {
					t.Errorf("%s (%s): no --edge colour for field borders", file, theme)
					break
				}
				if c := contrast(tok["edge"], tok[b]); c < 3 {
					t.Errorf("%s (%s): field border --edge %s on --%s is %.2f:1, below 3:1", file, theme, tok["edge"], b, c)
				}
			}
		}
	}
}

// The settings pages carry the same request helper and states; the copies
// must not drift apart.
func TestSettingsPagesShareOneHelper(t *testing.T) {
	const begin, end = "/* ---- shared by every settings page", "/* ---- end of shared ---- */"
	blocks := func(html string) []string {
		var out []string
		for {
			i := strings.Index(html, begin)
			if i < 0 {
				return out
			}
			j := strings.Index(html[i:], end)
			if j < 0 {
				return out
			}
			out = append(out, html[i:i+j])
			html = html[i+j+len(end):]
		}
	}
	// the not-paired card the shared script drives
	gateMarkup := func(html string) string {
		i := strings.Index(html, `<section id="gate"`)
		if i < 0 {
			return ""
		}
		j := strings.Index(html[i:], "</section>")
		if j < 0 {
			return ""
		}
		return html[i : i+j]
	}
	var want []string
	for _, f := range []string{"../health.html", "../memory.html", "../channels.html", "../accounts.html", "../protocols.html",
		"../devices_add.html", "../devices.html", "../backup.html", "../trust.html", "../restore_review.html", "../reach.html", "../voice_setup.html", "../spending.html"} {
		html := readPage(t, f)
		got := append(blocks(html), gateMarkup(html))
		if len(got) != 3 || got[2] == "" {
			t.Fatalf("%s: %d shared blocks, want the CSS, the script and the gate", f, len(got))
		}
		if want == nil {
			want = got
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: shared block %d differs from health.html's", f, i+1)
			}
		}
	}
}

// The design keeps the paid service's name off the presence screen, and the
// twin never pitches it: not on the device and safety pages either, where
// the paid option doesn't exist yet.
func TestScreenNeverNamesTheCloud(t *testing.T) {
	for _, f := range []string{"../ui.html", "../devices_add.html", "../devices.html", "../backup.html", "../trust.html", "../restore_review.html"} {
		html := strings.ToLower(readPage(t, f))
		for _, word := range []string{"mirrin cloud", "upgrade", "subscribe", "pricing", "paid"} {
			if strings.Contains(html, word) {
				t.Errorf("%s mentions %q", f, word)
			}
		}
	}
}
