package tray

import (
	"reflect"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Outfit and Outfit colour under Persona write the wardrobe by character.
// The drawn outfit, or "As drawn" for the colour, leaves nothing behind,
// so config.yaml stays as it was for someone who never chose.
func TestChoosingAnOutfitWritesTheWardrobe(t *testing.T) {
	var c config.Config
	setOutfit(&c, "penguin", "scarf")
	if c.Wardrobe["penguin"] != (config.Outfit{Outfit: "scarf"}) {
		t.Fatalf("%+v", c.Wardrobe)
	}
	setColour(&c, "penguin", "coral")
	if c.Wardrobe["penguin"] != (config.Outfit{Outfit: "scarf", Colour: "coral"}) {
		t.Fatalf("%+v", c.Wardrobe)
	}
	setOutfit(&c, "penguin", "bowtie") // the drawn one
	if c.Wardrobe["penguin"] != (config.Outfit{Colour: "coral"}) {
		t.Fatalf("%+v", c.Wardrobe)
	}
	setColour(&c, "penguin", "")
	if c.Wardrobe != nil {
		t.Fatalf("nothing chosen, but a wardrobe stays: %+v", c.Wardrobe)
	}
	setOutfit(&c, "nyra", "denim")
	setOutfit(&c, "mirrin", "linen")
	if !reflect.DeepEqual(c.Wardrobe, config.Wardrobe{"nyra": {Outfit: "denim"}, "mirrin": {Outfit: "linen"}}) {
		t.Fatalf("%+v", c.Wardrobe)
	}
}

// What the menus tick: the character's entry, or the drawn outfit and
// colour when it has none, or the wardrobe doesn't know the entry.
func TestTheMenusTickWhatTheCharacterWears(t *testing.T) {
	c := config.Config{Wardrobe: config.Wardrobe{"nyra": {Outfit: "Cardigan", Colour: "Plum"}, "mirrin": {Outfit: "kimono", Colour: "#0a0b0c"}}}
	if got := outfitOf(c, "nyra"); got != "cardigan" {
		t.Fatal(got)
	}
	if got := colourOf(c, "nyra"); got != "plum" {
		t.Fatal(got)
	}
	if got := outfitOf(c, "penguin"); got != "bowtie" {
		t.Fatalf("nothing chosen is the drawn outfit, got %q", got)
	}
	if got := colourOf(c, "penguin"); got != "" {
		t.Fatalf("nothing chosen is as drawn, got %q", got)
	}
	if got := outfitOf(c, "mirrin"); got != "dinner" {
		t.Fatalf("an outfit the wardrobe doesn't have is the drawn one, got %q", got)
	}
	if got := colourOf(c, "mirrin"); got != "" {
		t.Fatalf("a colour that isn't on the menu ticks nothing, got %q", got)
	}
}

func TestTheWardrobeMenusOfferEachCharactersOutfitsAndThePalette(t *testing.T) {
	if got := outfitChoices("penguin"); !reflect.DeepEqual(got, []choice{{"Bow tie", "bowtie"}, {"Scarf", "scarf"}, {"Beanie", "beanie"}, {"Sun hat", "sunhat"}}) {
		t.Fatalf("%+v", got)
	}
	if got := outfitChoices("robot"); got != nil {
		t.Fatalf("%+v", got)
	}
	got := colourChoices()
	if len(got) != 9 || got[0] != (choice{"As drawn", ""}) || got[1] != (choice{"Coral", "coral"}) || got[8] != (choice{"Charcoal", "charcoal"}) {
		t.Fatalf("%+v", got)
	}
}
