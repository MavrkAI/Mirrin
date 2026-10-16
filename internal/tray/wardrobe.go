package tray

import (
	"context"
	"strings"
	"time"

	"fyne.io/systray"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Outfit and Outfit colour under Persona: what the current persona's
// character wears on the screen (config.Wardrobe). The outfits offered are
// the character's own, so a switch of persona changes the list; the
// colours are the palette, and "As drawn" the character's own. Choosing the
// drawn outfit or colour leaves nothing in config.yaml.

// outfitChoices is a character's wardrobe as menu items.
func outfitChoices(character string) []choice {
	var out []choice
	for _, o := range config.Outfits(character) {
		out = append(out, choice{o.Label, o.ID})
	}
	return out
}

// colourChoices is "As drawn" and the palette.
func colourChoices() []choice {
	out := []choice{{"As drawn", ""}}
	for _, c := range config.Palette() {
		out = append(out, choice{c.Label, c.Name})
	}
	return out
}

// outfitOf is the outfit to tick for a character: its entry, or the drawn
// one (the first of its wardrobe) when it has none the wardrobe knows.
func outfitOf(c config.Config, character string) string {
	l := c.Wardrobe.Look(character)
	if l.Outfit != "" {
		return l.Outfit
	}
	if outfits := config.Outfits(character); len(outfits) > 0 {
		return outfits[0].ID
	}
	return ""
}

// colourOf is the colour to tick: the palette name in the entry, or "" (As
// drawn) for none, or for a colour that isn't on the menu.
func colourOf(c config.Config, character string) string {
	name := strings.ToLower(strings.TrimSpace(c.Wardrobe.Entry(character).Colour))
	for _, p := range config.Palette() {
		if p.Name == name {
			return name
		}
	}
	return ""
}

// setOutfit writes a character's outfit (config.Dress): the drawn one, the
// first of its wardrobe, is nothing to write.
func setOutfit(c *config.Config, character, id string) {
	c.Dress(character, id, c.Wardrobe.Entry(character).Colour)
}

// setColour writes a character's colour; "" is as drawn.
func setColour(c *config.Config, character, name string) {
	c.Dress(character, c.Wardrobe.Entry(character).Outfit, name)
}

// currentCharacter is the character the persona in c is drawn as.
func currentCharacter(c config.Config) string { return config.CharacterOf(c.Persona) }

// wardrobeMenu adds Outfit and Outfit colour under parent. Every
// character's outfits are items, and only the current character's show,
// so the list follows a persona switch within a second.
func wardrobeMenu(ctx context.Context, parent *systray.MenuItem, backend Backend, name string) {
	om := parent.AddSubMenuItem("Outfit", "What it wears on the screen")
	cm := parent.AddSubMenuItem("Outfit colour", "The colour of what it wears")
	type item struct {
		character string
		ch        choice
		it        *systray.MenuItem
	}
	var items []item
	for _, character := range config.Characters() {
		for _, ch := range outfitChoices(character) {
			items = append(items, item{character, ch, om.AddSubMenuItemCheckbox(ch.label, "", false)})
		}
	}
	show := func() {
		cfg := backend.Config()
		cur := currentCharacter(cfg)
		tick := outfitOf(cfg, cur)
		for _, i := range items {
			if i.character != cur {
				i.it.Hide()
				continue
			}
			i.it.Show()
			if i.ch.value == tick {
				i.it.Check()
			} else {
				i.it.Uncheck()
			}
		}
	}
	for _, i := range items {
		i := i
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-i.it.ClickedCh:
					// a click on another character's outfit (the list was a
					// moment behind a persona switch) changes nothing
					if currentCharacter(backend.Config()) != i.character {
						show()
						continue
					}
					if err := backend.UpdateConfig(func(c *config.Config) { setOutfit(c, i.character, i.ch.value) }); err != nil {
						notify(name, "Could not save: "+err.Error())
					}
					show()
				}
			}
		}()
	}
	colours := radio(ctx, cm, colourChoices(),
		func(c config.Config) string { return colourOf(c, currentCharacter(c)) },
		func(c *config.Config, v string) { setColour(c, currentCharacter(*c), v) }, backend, name)
	show()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				show()
				colours()
			}
		}
	}()
}
