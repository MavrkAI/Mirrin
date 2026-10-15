package api

import (
	"encoding/json"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
)

// dresserFake is a welcome backend that can say what each character wears.
type dresserFake struct {
	welcomeFake
	looks map[string]config.Look
}

func (f *dresserFake) Look(character string) config.Look { return f.looks[character] }

// The welcome page's cast wears what the wardrobe says, each character its
// own; a backend that can't say dresses them as drawn.
func TestWelcomePersonasWearTheirLooks(t *testing.T) {
	type got struct {
		ID   string      `json:"id"`
		Look config.Look `json:"look"`
	}
	cast := func(b WelcomeBackend) []got {
		e := newEnv(t)
		e.s.WithWelcome(b)
		w := e.do(onLoopback, req{path: "/welcome/personas", header: bearer(master)})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		var out []got
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out) != 3 {
			t.Fatalf("%d personas", len(out))
		}
		return out
	}
	dressed := cast(&dresserFake{looks: map[string]config.Look{"penguin": {Outfit: "scarf", Colour: "#ff6f61"}, "nyra": {Outfit: "cardigan"}}})
	want := map[string]config.Look{"mirrin": {}, "nyra": {Outfit: "cardigan"}, "pickoo": {Outfit: "scarf", Colour: "#ff6f61"}}
	for _, g := range dressed {
		if g.Look != want[g.ID] {
			t.Errorf("%s wears %+v, want %+v", g.ID, g.Look, want[g.ID])
		}
	}
	for _, g := range cast(&welcomeFake{}) {
		if g.Look != (config.Look{}) {
			t.Errorf("%s wears %+v with no wardrobe to ask", g.ID, g.Look)
		}
	}
}

// Which character a persona is drawn as is one answer everywhere (config.CharacterOf):
// the welcome page's cast and a restored twin get the kind characters.js draws.
func TestCharacterOfAPersonaMatchesTheWardrobe(t *testing.T) {
	for id, want := range map[string]string{"mirrin": "maverick", "mavrk": "maverick", "plain": "maverick", "robot": "maverick", "pack/robot": "maverick", "Nyra": "nyra", "pickoo": "penguin", "pack/butler": "penguin"} {
		if got := characterOf(id); got != want {
			t.Errorf("characterOf(%q) = %q, want %q", id, got, want)
		}
	}
}

// A wall screen draws the character in its outfit too: the look is one of
// the fields it is sent as they are.
func TestAWallScreenSeesTheLook(t *testing.T) {
	e := newEnv(t)
	day := aFullDay()
	day["look"] = map[string]any{"outfit": "scarf", "colour": "#ff6f61"}
	e.s.WithScreen(privateScreen{e.f, day})
	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	w := e.do(onRemote, req{path: "/screen", header: bearer(kiosk)})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var got struct {
		Look config.Look `json:"look"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Look != (config.Look{Outfit: "scarf", Colour: "#ff6f61"}) {
		t.Fatalf("a wall screen got %+v: %s", got.Look, w.Body)
	}
}
