package pagetest

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// Every character has a wardrobe in the drawing code, each outfit drawn as
// its own group so the screen and a test can tell them apart. The first is
// the character as drawn, an outfit it doesn't have is that one too, and
// every outfit keeps the hooks the poses, the blink and the pointer use.
func TestEachCharacterDrawsItsWardrobe(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "loaded")
	want := map[string][]string{
		"penguin":  {"bowtie", "scarf", "beanie", "sunhat"},
		"nyra":     {"blazer", "cardigan", "turtleneck", "denim"},
		"maverick": {"dinner", "waistcoat", "jumper", "linen"},
	}
	if got := eval[map[string][]string](t, ctx, `Characters.outfits`); !reflect.DeepEqual(got, want) {
		t.Fatalf("Characters.outfits = %v, want %v", got, want)
	}
	for kind, ids := range want {
		plain := eval[string](t, ctx, fmt.Sprintf(`Characters.draw(%q, 'x')`, kind))
		if plain == "" || strings.Contains(plain, `class="outfit`) {
			t.Fatalf("%s as drawn wears an outfit group: %.80s", kind, plain)
		}
		for i, id := range ids {
			svg := eval[string](t, ctx, fmt.Sprintf(`Characters.draw(%q, 'x', %q)`, kind, id))
			if i == 0 {
				if svg != plain {
					t.Errorf("%s in %s (the first outfit) isn't the character as drawn", kind, id)
				}
				continue
			}
			if !strings.Contains(svg, `class="outfit `+id+`"`) || svg == plain {
				t.Errorf("%s in %s: no outfit group of its own", kind, id)
			}
			for _, hook := range []string{`class="eyes"`, `class="lid"`, `class="head"`, `class="front"`, `class="turn"`, `class="lower"`} {
				if !strings.Contains(svg, hook) {
					t.Errorf("%s in %s lost %s", kind, id, hook)
				}
			}
			if strings.Count(svg, "<svg") != 1 || !strings.HasSuffix(svg, "</svg>") {
				t.Errorf("%s in %s isn't one svg", kind, id)
			}
		}
		if eval[string](t, ctx, fmt.Sprintf(`Characters.draw(%q, 'x', 'kimono')`, kind)) != plain {
			t.Errorf("%s in an outfit it doesn't have isn't the character as drawn", kind)
		}
	}
}

// The screen draws the current character in the look the twin sends, in
// the owner's colour, on the screen and in the widget; a hat sits in the
// head group under the eyes, so the blink still covers them; and the
// character is back as drawn when the look goes.
func TestTheScreenDressesTheCharacter(t *testing.T) {
	d := newDaemon(t)
	dressed := func(look map[string]any) map[string]any {
		return screen(func(m map[string]any) {
			m["persona"] = "pickoo"
			m["quiet_hours"] = "00:00-23:59" // night all day: the dozing figure is drawn
			if look != nil {
				m["look"] = look
			}
		})
	}
	d.handle("GET /screen", jsonH(dressed(map[string]any{"outfit": "scarf", "colour": "#ff6f61"})))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, `!!document.querySelector('#talkOrb svg .outfit.scarf')`, "Pickoo in a scarf")
	run(t, ctx, chromedp.Evaluate(`renderDoze(new Date())`, nil)) // the clock does this once a minute
	for what, expr := range map[string]string{
		"in the widget too":            `!!document.querySelector('#widget .orb svg .outfit.scarf')`,
		"in coral":                     `getComputedStyle(document.querySelector('#talkOrb')).getPropertyValue('--o1').trim() === '#ff6f61' && getComputedStyle(document.querySelector('#widget .orb')).getPropertyValue('--o1').trim() === '#ff6f61'`,
		"the bow tie resting in coral": `getComputedStyle(document.querySelector('#talkOrb')).getPropertyValue('--c1').trim() === '#ff6f61'`,
		"one svg in each place":        `document.querySelectorAll('#talkOrb svg').length === 1 && document.querySelectorAll('#widget .orb svg').length === 1`,
		"dozing in it too":             `!!document.querySelector('#doze svg .outfit.scarf')`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("not %s", what)
		}
	}
	// the screen asks again every 15 seconds; here it is asked to now, so a
	// change of look alone redresses it, with no page load
	d.handle("GET /screen", jsonH(dressed(map[string]any{"outfit": "beanie"})))
	run(t, ctx, chromedp.Evaluate(`load()`, nil))
	waitFor(t, ctx, `!!document.querySelector('#talkOrb svg .head .outfit.beanie')`, "Pickoo in a beanie")
	if !eval[bool](t, ctx, `(() => { const k = [...document.querySelector('#talkOrb svg .head').children], hat = k.findIndex(e => e.classList.contains('outfit')); return hat >= 0 && hat < k.findIndex(e => e.classList.contains('eyes')); })()`) {
		t.Fatal("the beanie isn't in the head group before the eyes")
	}
	if got := eval[string](t, ctx, `getComputedStyle(document.querySelector('#talkOrb')).getPropertyValue('--o1').trim()`); got != "" {
		t.Fatalf("as coloured, but the orb still says %q", got)
	}
	d.handle("GET /screen", jsonH(dressed(nil)))
	run(t, ctx, chromedp.Evaluate(`load()`, nil))
	waitFor(t, ctx, `!document.querySelector('#talkOrb svg .outfit') && !document.querySelector('#widget .orb svg .outfit')`, "Pickoo as drawn")
}

// The welcome page's cast wears what the twin says each character wears.
func TestWelcomeCastWearsTheWardrobe(t *testing.T) {
	ctx, d, _ := welcomeStep2(t, true)
	cast := make([]map[string]any, len(welcomeCast))
	for i, p := range welcomeCast {
		cast[i] = maps.Clone(p)
	}
	cast[1]["look"] = map[string]any{"outfit": "cardigan", "colour": "#9b59b6"}
	cast[2]["look"] = map[string]any{"outfit": "sunhat"}
	d.handle("GET /welcome/personas", jsonH(cast))
	run(t, ctx, chromedp.Navigate(d.url("/welcome")))
	waitFor(t, ctx, `!!document.querySelector('#cast .pic.nyra .outfit.cardigan')`, "Nyra in a cardigan")
	for what, expr := range map[string]string{
		"Nyra in plum":                  `document.querySelector('#cast .pic.nyra').style.getPropertyValue('--o1') === '#9b59b6'`,
		"Pickoo in a sun hat, as drawn": `!!document.querySelector('#cast .pic.penguin .outfit.sunhat') && document.querySelector('#cast .pic.penguin').style.getPropertyValue('--o1') === ''`,
		"Mirrin as drawn":               `!document.querySelector('#cast .pic.maverick .outfit')`,
		"every id once":                 `(() => { const ids = [...document.querySelectorAll('[id]')].map(e => e.id); return ids.length === new Set(ids).size; })()`,
	} {
		if !eval[bool](t, ctx, expr) {
			t.Errorf("not %s", what)
		}
	}
}
