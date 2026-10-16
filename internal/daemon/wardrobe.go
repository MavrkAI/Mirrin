package daemon

import (
	"context"
	"strings"
	"sync"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/persona"
)

// wardrobeNotes remembers what noteWardrobe last said, so a save that
// changes nothing about the wardrobe says nothing.
type wardrobeNotes struct {
	mu   sync.Mutex
	last string
}

// The wardrobe (config.Wardrobe) is what each character wears on the
// screen: the menu bar's Persona → Outfit and Outfit colour. The screen is
// sent the current persona's character's look with its data, and the
// welcome page's cast asks for each character's (api.WelcomeDresser).

// lookFor is what the screen draws the persona in: its character's entry
// in the wardrobe, as drawn where there is none. Call it with cmu held.
func lookFor(cfg *config.Config, pr persona.Persona) config.Look {
	return cfg.Wardrobe.Look(config.CharacterOf(pr.ID))
}

// Look is what a character wears, for the welcome page's cast.
func (d *Daemon) Look(character string) config.Look {
	return d.Config().Wardrobe.Look(character)
}

// noteWardrobe logs an outfit or colour the wardrobe can't use, once each
// time the wardrobe changes; the character is drawn as it is instead.
func (d *Daemon) noteWardrobe(cfg *config.Config) {
	lines := cfg.Wardrobe.Unknown()
	all := strings.Join(lines, "\n")
	d.wardrobe.mu.Lock()
	same := d.wardrobe.last == all
	d.wardrobe.last = all
	d.wardrobe.mu.Unlock()
	if same {
		return
	}
	for _, line := range lines {
		d.log.Warn("wardrobe: drawn as it is instead", "entry", line)
	}
}

// Dress is setup dressing the twin as it names it (api.WelcomeWardrobe):
// the look goes to the persona's character, through config.Dress, so what
// the wardrobe doesn't know leaves the character as drawn.
func (d *Daemon) Dress(ctx context.Context, personaID, outfit, colour string) error {
	return d.UpdateConfig(func(c *config.Config) { c.Dress(config.CharacterOf(personaID), outfit, colour) })
}
