package daemon

import (
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/persona"
)

// The screen greets the owner by the part of the day, in the persona's own
// words: "Good morning, sir." from Mirrin at 7:40, "Late one, sir." at 1am,
// "Morning, Akshaya." from Nyra. A wall screen paired only to look never
// gets these (api's shownToAll leaves them out), so it says the plain
// "Good morning" to whoever walks past.

// hellosFor are pr's hellos as said to cfg's owner, by part of the day; nil
// when pr has none, and the screen says the plain words.
func hellosFor(cfg *config.Config, pr persona.Persona) map[string]string {
	if len(pr.Hellos) == 0 {
		return nil
	}
	name, address := helloName(cfg), addressOf(cfg, pr) // address.go
	out := map[string]string{}
	for _, part := range persona.PartsOfDay {
		if h := pr.Hellos[part]; h != "" {
			out[part] = persona.Render(h, name, address)
		}
	}
	return out
}
