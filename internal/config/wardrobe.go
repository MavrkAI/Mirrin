package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Wardrobe is what each character wears on the screen, keyed by character:
// mirrin (the man in the dinner jacket), nyra and penguin (Pickoo, and any
// persona without a drawing of its own). It is the menu bar's Persona →
// Outfit and Outfit colour, and the `wardrobe:` section of config.yaml.
// An outfit or colour it doesn't know means the character as drawn.
type Wardrobe map[string]Outfit

// Outfit is one character's choice: an outfit from its wardrobe (Outfits)
// and a colour from the palette by name, or any #rrggbb.
type Outfit struct {
	Outfit string `yaml:"outfit,omitempty"`
	Colour string `yaml:"colour,omitempty"`
}

// Look is what the screen draws: the outfit's id ("" for the one drawn)
// and the colour as #rrggbb ("" for the drawn colours).
type Look struct {
	Outfit string `json:"outfit,omitempty"`
	Colour string `json:"colour,omitempty"`
}

// Choice is an outfit or a colour as the menu offers it.
type Choice struct {
	ID    string
	Label string
}

// Colour is one of the palette.
type Colour struct {
	Name, Label, Hex string
}

// wardrobes are each character's outfits, the drawn one first.
var wardrobes = map[string][]Choice{
	"mirrin":  {{"dinner", "Dinner jacket"}, {"waistcoat", "Waistcoat"}, {"jumper", "Knit jumper"}, {"linen", "Linen shirt"}},
	"nyra":    {{"blazer", "Navy blazer"}, {"cardigan", "Cardigan"}, {"turtleneck", "Turtleneck"}, {"denim", "Denim jacket"}},
	"penguin": {{"bowtie", "Bow tie"}, {"scarf", "Scarf"}, {"beanie", "Beanie"}, {"sunhat", "Sun hat"}},
}

// palette is the colours the menu offers, in its order.
var palette = []Colour{
	{"coral", "Coral", "#ff6f61"}, {"rose", "Rose", "#e8718d"}, {"mustard", "Mustard", "#e3b23c"}, {"forest", "Forest", "#2f9e63"},
	{"teal", "Teal", "#2bb3a3"}, {"sky", "Sky", "#5bb8f5"}, {"plum", "Plum", "#9b59b6"}, {"charcoal", "Charcoal", "#4a4f5c"},
}

var reHex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Characters are the characters with a wardrobe, in order.
func Characters() []string {
	out := make([]string, 0, len(wardrobes))
	for k := range wardrobes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Outfits is a character's wardrobe, the drawn outfit first; nil for a
// character that isn't drawn.
func Outfits(character string) []Choice {
	return wardrobes[character]
}

// Palette is the colours the menu offers.
func Palette() []Colour { return palette }

// CharacterOf is which character a persona is drawn as (characters.js's
// kindFor): Mirrin (and mavrk and plain, his ids before) and Nyra as
// themselves, everyone else, a pack's included, as the penguin.
func CharacterOf(personaID string) string {
	id := strings.ToLower(personaID)
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	switch id {
	case "nyra":
		return "nyra"
	case "mirrin", "mavrk", "maverick", "plain", "robot":
		return "mirrin"
	}
	return "penguin"
}

// entry is a character's entry, by a key read in any case, trimmed, with
// Pickoo's own name meaning the penguin.
func (w Wardrobe) entry(character string) (Outfit, bool) {
	for k, o := range w {
		if characterKey(k) == character {
			return o, true
		}
	}
	return Outfit{}, false
}

// characterKey is a wardrobe key as the wardrobe reads it.
func characterKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	if k == "pickoo" {
		return "penguin"
	}
	return k
}

// Look is what a character wears: its entry, with anything the wardrobe
// doesn't know left as drawn.
func (w Wardrobe) Look(character string) Look {
	o, ok := w.entry(character)
	if !ok {
		return Look{}
	}
	var l Look
	if id := strings.ToLower(strings.TrimSpace(o.Outfit)); outfitKnown(character, id) {
		l.Outfit = id
	}
	l.Colour, _ = colourHex(o.Colour)
	return l
}

// Unknown names the entries the wardrobe can't use, one line each for the
// log, in order by character.
func (w Wardrobe) Unknown() []string {
	var out []string
	for _, key := range sortedKeys(w) {
		o, character := w[key], characterKey(key)
		outfits := wardrobes[character]
		if outfits == nil {
			out = append(out, fmt.Sprintf("%s: no character %q (%s; Pickoo is the penguin)", key, key, orList(Characters())))
			continue
		}
		if id := strings.ToLower(strings.TrimSpace(o.Outfit)); id != "" && !outfitKnown(character, id) {
			ids := make([]string, len(outfits))
			for i, c := range outfits {
				ids[i] = c.ID
			}
			out = append(out, fmt.Sprintf("%s: no outfit %q (%s)", character, o.Outfit, orList(ids)))
		}
		if _, ok := colourHex(o.Colour); !ok {
			names := make([]string, len(palette))
			for i, c := range palette {
				names[i] = c.Name
			}
			out = append(out, fmt.Sprintf("%s: no colour %q (%s or #rrggbb)", character, o.Colour, strings.Join(names, ", ")))
		}
	}
	return out
}

func outfitKnown(character, id string) bool {
	for _, c := range wardrobes[character] {
		if c.ID == id {
			return true
		}
	}
	return false
}

// colourHex is a colour as #rrggbb: a palette name or a hex colour. "" is
// the drawn colour, known; anything else is unknown.
func colourHex(c string) (string, bool) {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "" {
		return "", true
	}
	for _, p := range palette {
		if p.Name == c {
			return p.Hex, true
		}
	}
	if reHex.MatchString(c) {
		return c, true
	}
	return "", false
}

func sortedKeys(w Wardrobe) []string {
	keys := make([]string, 0, len(w))
	for k := range w {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// orList is "a, b or c".
func orList(s []string) string {
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " or " + s[len(s)-1]
}
