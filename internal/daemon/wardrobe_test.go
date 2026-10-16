package daemon

import (
	"context"
	"reflect"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// The screen is sent what the current character wears, from the wardrobe
// by character, so a persona switch brings that character's own outfit;
// the welcome page's cast asks for each character's.
func TestTheScreenIsSentTheCharactersLook(t *testing.T) {
	td := newTestDaemon(t, nil)
	ctx := context.Background()
	if err := td.UpdateConfig(func(c *config.Config) {
		c.Persona = "pickoo"
		c.Wardrobe = config.Wardrobe{"penguin": {Outfit: "scarf", Colour: "coral"}, "nyra": {Outfit: "kimono", Colour: "#0a0b0c"}}
	}); err != nil {
		t.Fatal(err)
	}
	if sd := td.screenData(ctx); sd.Look != (config.Look{Outfit: "scarf", Colour: "#ff6f61"}) {
		t.Fatalf("pickoo: %+v", sd.Look)
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.Persona = "nyra" }); err != nil {
		t.Fatal(err)
	}
	if sd := td.screenData(ctx); sd.Look != (config.Look{Colour: "#0a0b0c"}) {
		t.Fatalf("nyra, in an outfit she doesn't have: %+v", sd.Look)
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.Persona = "mirrin" }); err != nil {
		t.Fatal(err)
	}
	if sd := td.screenData(ctx); sd.Look != (config.Look{}) {
		t.Fatalf("mirrin, as drawn: %+v", sd.Look)
	}
	if l := td.Look("penguin"); l != (config.Look{Outfit: "scarf", Colour: "#ff6f61"}) {
		t.Fatalf("the cast's penguin: %+v", l)
	}
}

// Setup dresses the twin as it is named (api.WelcomeWardrobe): the look
// goes to the chosen persona's character, through config.Dress, so a
// persona without a drawing of its own dresses the penguin, and what the
// wardrobe doesn't know leaves the character as drawn.
func TestSetupDressesTheChosenCharacter(t *testing.T) {
	td := newTestDaemon(t, nil)
	ctx := context.Background()
	if err := td.Dress(ctx, "pickoo", "scarf", "coral"); err != nil {
		t.Fatal(err)
	}
	if got := td.Config().Wardrobe; !reflect.DeepEqual(got, config.Wardrobe{"penguin": {Outfit: "scarf", Colour: "coral"}}) {
		t.Fatalf("%+v", got)
	}
	if err := td.Dress(ctx, "mirrin", "kimono", "#5bb8f5"); err != nil {
		t.Fatal(err)
	}
	if got := td.Config().Wardrobe; !reflect.DeepEqual(got, config.Wardrobe{"penguin": {Outfit: "scarf", Colour: "coral"}, "mirrin": {Colour: "#5bb8f5"}}) {
		t.Fatalf("%+v", got)
	}
	if l := td.Look("mirrin"); l != (config.Look{Colour: "#5bb8f5"}) {
		t.Fatalf("mirrin for the cast: %+v", l)
	}
}
