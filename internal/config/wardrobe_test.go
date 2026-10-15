package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// What each character wears comes from the wardrobe by character, and
// anything the wardrobe doesn't know means the character as drawn.
func TestWardrobeLookFallsBackToAsDrawn(t *testing.T) {
	w := Wardrobe{
		"penguin": {Outfit: "scarf", Colour: "coral"},
		"nyra":    {Outfit: "kimono", Colour: "#12abEF"},
		"mirrin":  {Outfit: "Jumper", Colour: "neon"},
	}
	if l := w.Look("penguin"); l != (Look{Outfit: "scarf", Colour: "#ff6f61"}) {
		t.Fatalf("penguin: %+v", l)
	}
	// an outfit it doesn't have is the one drawn; a colour by hex passes, lower-cased
	if l := w.Look("nyra"); l != (Look{Colour: "#12abef"}) {
		t.Fatalf("nyra: %+v", l)
	}
	// outfits read any case; a colour it doesn't know is the drawn one
	if l := w.Look("mirrin"); l != (Look{Outfit: "jumper"}) {
		t.Fatalf("mirrin: %+v", l)
	}
	if l := w.Look("robot"); l != (Look{}) {
		t.Fatalf("no entry: %+v", l)
	}
	if l := Wardrobe(nil).Look("penguin"); l != (Look{}) {
		t.Fatalf("no wardrobe: %+v", l)
	}
	// the drawn outfit named outright is itself
	if l := (Wardrobe{"penguin": {Outfit: "bowtie"}}).Look("penguin"); l != (Look{Outfit: "bowtie"}) {
		t.Fatalf("bowtie: %+v", l)
	}
	// a key in any case, with a stray space, and Pickoo's own name for the penguin
	if l := (Wardrobe{" Penguin": {Outfit: "beanie"}}).Look("penguin"); l != (Look{Outfit: "beanie"}) {
		t.Fatalf("Penguin: %+v", l)
	}
	if l := (Wardrobe{"Pickoo": {Outfit: "sunhat"}}).Look("penguin"); l != (Look{Outfit: "sunhat"}) {
		t.Fatalf("pickoo: %+v", l)
	}
	if u := (Wardrobe{"Pickoo": {Outfit: "sunhat"}}).Unknown(); u != nil {
		t.Fatalf("pickoo is the penguin, but: %v", u)
	}
}

func TestEachCharacterHasItsOwnWardrobe(t *testing.T) {
	want := map[string][]string{
		"penguin": {"bowtie", "scarf", "beanie", "sunhat"},
		"nyra":    {"blazer", "cardigan", "turtleneck", "denim"},
		"mirrin":  {"dinner", "waistcoat", "jumper", "linen"},
	}
	if got := Characters(); !reflect.DeepEqual(got, []string{"mirrin", "nyra", "penguin"}) {
		t.Fatal(got)
	}
	for kind, ids := range want {
		var got []string
		for _, o := range Outfits(kind) {
			got = append(got, o.ID)
			if o.Label == "" || strings.ToLower(o.Label[:1]) == o.Label[:1] {
				t.Errorf("%s: outfit %q wants a label that reads as a menu item, got %q", kind, o.ID, o.Label)
			}
		}
		if !reflect.DeepEqual(got, ids) {
			t.Errorf("%s: outfits %v, want %v", kind, got, ids)
		}
	}
	if Outfits("robot") != nil {
		t.Fatal("a character that isn't drawn has no wardrobe")
	}
}

func TestThePaletteIsEightNamedColours(t *testing.T) {
	p := Palette()
	if len(p) != 8 {
		t.Fatalf("%d colours", len(p))
	}
	seen := map[string]bool{}
	for _, c := range p {
		if c.Name == "" || c.Label == "" || len(c.Hex) != 7 || c.Hex[0] != '#' || seen[c.Name] {
			t.Errorf("colour %+v", c)
		}
		seen[c.Name] = true
		if l := (Wardrobe{"nyra": {Colour: c.Name}}).Look("nyra"); l.Colour != c.Hex {
			t.Errorf("%s: %q, want %q", c.Name, l.Colour, c.Hex)
		}
	}
	for _, n := range []string{"coral", "rose", "mustard", "forest", "teal", "sky", "plum", "charcoal"} {
		if !seen[n] {
			t.Errorf("no %s", n)
		}
	}
}

// For the log: which entries the wardrobe couldn't use, in words.
func TestWardrobeNamesWhatItDoesNotKnow(t *testing.T) {
	w := Wardrobe{
		"penguin": {Outfit: "scarf", Colour: "coral"},
		"nyra":    {Outfit: "kimono"},
		"mirrin":  {Colour: "neon"},
		"robot":   {Outfit: "scarf"},
	}
	got := w.Unknown()
	want := []string{
		`mirrin: no colour "neon" (coral, rose, mustard, forest, teal, sky, plum, charcoal or #rrggbb)`,
		`nyra: no outfit "kimono" (blazer, cardigan, turtleneck or denim)`,
		`robot: no character "robot" (mirrin, nyra or penguin; Pickoo is the penguin)`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if Wardrobe(nil).Unknown() != nil {
		t.Fatal("nothing to say about no wardrobe")
	}
}

// Which character a persona is drawn as: the wardrobe's key.
func TestCharacterOfAPersona(t *testing.T) {
	for id, want := range map[string]string{
		"mirrin": "mirrin", "mavrk": "mirrin", "plain": "mirrin", "maverick": "mirrin", "robot": "mirrin", "pack/robot": "mirrin",
		"nyra": "nyra", "Nyra": "nyra",
		"pickoo": "penguin", "mirrin-pack-starter/butler": "penguin", "": "penguin",
	} {
		if got := CharacterOf(id); got != want {
			t.Errorf("CharacterOf(%q) = %q, want %q", id, got, want)
		}
	}
}

// A wardrobe is a plain section of config.yaml, and a config without one
// writes none.
func TestWardrobeInConfigYAML(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("persona: pickoo\nwardrobe:\n  penguin: {outfit: scarf, colour: coral}\n  nyra:\n    outfit: cardigan\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.Wardrobe["penguin"] != (Outfit{Outfit: "scarf", Colour: "coral"}) || c.Wardrobe["nyra"] != (Outfit{Outfit: "cardigan"}) {
		t.Fatalf("%+v", c.Wardrobe)
	}
	out, err := yaml.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "wardrobe") {
		t.Fatal("a fresh config writes a wardrobe section it doesn't need")
	}
}
