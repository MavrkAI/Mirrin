package api

import (
	"context"
	"net/http"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// The welcome page's dress-up, under the cast in step 2: the chosen
// character drawn large, its outfits and the palette, each tap redrawing
// it as the screen will. Nothing is sent until "Meet my twin", which
// carries the look on the names request.

// WelcomeWardrobe is a WelcomeBackend that can dress the twin: the look
// chosen in step 2 goes to the persona's character (config.Dress).
type WelcomeWardrobe interface {
	Dress(ctx context.Context, persona, outfit, colour string) error
}

// welcomeNames is the names request (POST /welcome/names). Outfit and
// Colour are the look, present when the page offered the dress-up: "" is
// as drawn, chosen; both absent is a page without the dress-up, which
// leaves the look as it was. The page sends both or neither; one on its
// own is a whole look too, with the other as drawn.
type welcomeNames struct {
	You, Twin, Persona, Address string
	Outfit, Colour              *string
}

// dress gives the backend the look on a names request, when there is one
// and it can take it.
func (v welcomeNames) dress(ctx context.Context, b WelcomeBackend) error {
	if v.Outfit == nil && v.Colour == nil {
		return nil
	}
	d, ok := b.(WelcomeWardrobe)
	if !ok {
		return nil
	}
	var outfit, colour string
	if v.Outfit != nil {
		outfit = *v.Outfit
	}
	if v.Colour != nil {
		colour = *v.Colour
	}
	return d.Dress(ctx, v.Persona, outfit, colour)
}

// WelcomeWardrobeInfo is GET /welcome/wardrobe: each character's outfits,
// by the kind characters.js draws it as, the drawn one first, and the
// palette in its order.
type WelcomeWardrobeInfo struct {
	Outfits map[string][]WelcomeChoice `json:"outfits"`
	Palette []WelcomeColour            `json:"palette"`
}
type WelcomeChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
type WelcomeColour struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Hex   string `json:"hex"`
}

func welcomeWardrobe() WelcomeWardrobeInfo {
	info := WelcomeWardrobeInfo{Outfits: map[string][]WelcomeChoice{}}
	for _, character := range config.Characters() {
		kind := characterOf(character)
		for _, o := range config.Outfits(character) {
			info.Outfits[kind] = append(info.Outfits[kind], WelcomeChoice{o.ID, o.Label})
		}
	}
	for _, c := range config.Palette() {
		info.Palette = append(info.Palette, WelcomeColour{c.Name, c.Label, c.Hex})
	}
	return info
}

func welcomeWardrobeHandler(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, welcomeWardrobe())
	return nil
}
