package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/persona"
)

// Acknowledgements are tiny spoken "I heard you" sounds played the moment the
// wake phrase is confirmed, before the model answers, so the thinking pause
// never feels like silence. They are synthesised once with the active voice.
var ackPhrases = []string{"Right.", "Let me see.", "On it.", "One moment."}

// The first exchange after a quiet spell opens with the persona's hello for
// the part of the day ("Good morning, sir.", "Late one, sir."), with the
// owner's name and form of address. Each is cached by its words, so a new
// name or address makes a new clip instead of playing the old one.

// plainHellos are the hellos of a persona that has none of its own.
var plainHellos = map[string]string{"morning": "Good morning.", "afternoon": "Good afternoon.", "evening": "Good evening.", "late": "Hello."}

// SetHellos gives the voice the persona's hellos (persona.Hellos).
func (c *Channel) SetHellos(hellos map[string]string) { c.hellos = hellos }

// SetName tells the voice the owner's first name, for hellos that use it.
func (c *Channel) SetName(name string) { c.ownerName.Store(&name) }

// helloPhrase is the hello for the part of the day now falls in, as said
// to the owner now.
func (c *Channel) helloPhrase(now time.Time) string { return c.helloFor(persona.PartOfDay(now)) }

// helloFor is the hello for a part of the day, as said to the owner now.
func (c *Channel) helloFor(part string) string {
	h := c.hellos[part]
	if strings.TrimSpace(h) == "" {
		h = plainHellos[part]
	}
	return persona.Render(h, stored(&c.ownerName), stored(&c.address))
}

// startGreeting is what the voice says as it starts: the persona's
// greeting, as said to the owner, or "Online." when it has none.
func (c *Channel) startGreeting() string {
	if g := persona.Render(c.greeting, stored(&c.ownerName), stored(&c.address)); strings.TrimSpace(g) != "" {
		return g
	}
	return "Online."
}

// stored is a string set with Store, "" before one is.
func stored(p *atomic.Pointer[string]) string {
	if s := p.Load(); s != nil {
		return *s
	}
	return ""
}

// bareWakeFor is what a bare "Hey Nyra" is answered with: the owner's
// form of address as a question ("Sir?", "Akshaya?"), or "Yes?" when
// there is none.
func bareWakeFor(address string) string {
	address = strings.TrimRight(strings.TrimSpace(address), ".,!?")
	if address == "" {
		return "Yes?"
	}
	r, size := utf8.DecodeRuneInString(address)
	return string(unicode.ToUpper(r)) + address[size:] + "?"
}

// SetAddress tells the voice how the twin addresses the owner (the
// daemon's address, "" for none), for its answer to a bare wake.
func (c *Channel) SetAddress(address string) {
	w := bareWakeFor(address)
	c.bare.Store(&w)
	c.address.Store(&address)
}

// bareWake is what a bare wake is answered with now.
func (c *Channel) bareWake() string {
	if w := c.bare.Load(); w != nil {
		return *w
	}
	return bareWakeFor("")
}

type acks struct {
	mu    sync.Mutex
	ack   []string          // clips of ackPhrases (or the persona's)
	hello map[string]string // the hellos' clips, by their words
	done  chan struct{}
	// more makes the clip for a hello not made yet (a new name or address);
	// nil until there is a voice to make it with.
	more func(phrase string)
}

// ackSpeed is how fast acknowledgements are spoken: a touch brisk, still natural.
const ackSpeed = 1.08

// ackMaker is how acknowledgement clips are made with the active voice.
// recipe names everything that shapes a clip (engine, voice, speed, the
// synthesis script), and is part of each clip's cache name, so changing any
// of it (or a phrase) makes new clips instead of replaying stale ones.
type ackMaker struct {
	ext    string
	recipe string
	make   func(ctx context.Context, phrase, path string) error
}

func (c *Channel) ackMaker() ackMaker {
	if c.ackMakerFn != nil {
		return c.ackMakerFn()
	}
	switch {
	case c.kokoro != nil:
		k := c.kokoro
		return ackMaker{"wav", fmt.Sprintf("kokoro %s speed %.2f pitch %d script %s", k.voice, ackSpeed, k.pitch, shortHash(kokoroScript)),
			func(ctx context.Context, phrase, path string) error {
				return k.synthesizeSpeed(ctx, phrase, path, ackSpeed)
			}}
	case c.eleven != nil:
		e := c.eleven
		return ackMaker{"mp3", "elevenlabs " + e.voice + " " + e.model,
			func(ctx context.Context, phrase, path string) error { return e.synthesizeTo(ctx, phrase, path) }}
	case c.macVoice != "":
		v := c.macVoice
		return ackMaker{"aiff", "say " + v,
			func(ctx context.Context, phrase, path string) error {
				return exec.CommandContext(ctx, "say", "-v", v, "-o", path, phrase).Run()
			}}
	}
	return ackMaker{}
}

// shortHash is 10 hex characters of b's SHA-256.
func shortHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:5])
}

// ackClipName is where a phrase's clip is cached for a voice and recipe.
func ackClipName(voiceKey, recipe, phrase, ext string) string {
	return fmt.Sprintf("ack-%s-%s.%s", voiceKey, shortHash([]byte(recipe+"\n"+phrase)), ext)
}

// prepareAcks synthesises the acknowledgement clips in the background.
func (c *Channel) prepareAcks(ctx context.Context) {
	if c.acks == nil {
		c.acks = &acks{}
	}
	done := make(chan struct{})
	c.acks.mu.Lock()
	c.acks.done = done
	c.acks.mu.Unlock()
	phrases := ackPhrases
	if len(c.ackPhrases) > 0 {
		phrases = c.ackPhrases
	}
	voiceKey := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, c.voiceName())
	m := c.ackMaker()
	go func() {
		defer close(done)
		if m.make == nil {
			return // no voice to make them with: the chime stands in
		}
		var making sync.Mutex // one clip at a time, here and for later hellos
		making.Lock()
		defer making.Unlock()
		keep := map[string]bool{}
		clip := func(phrase string) string {
			path := filepath.Join(c.dataDir, ackClipName(voiceKey, m.recipe, phrase, m.ext))
			keep[path] = true
			if _, err := os.Stat(path); err != nil {
				if err := m.make(ctx, phrase, path); err != nil {
					_ = os.Remove(path)
					return ""
				}
				normalize(path)
			}
			return path
		}
		var ack []string
		for _, phrase := range phrases {
			if path := clip(phrase); path != "" {
				ack = append(ack, path)
			}
		}
		// One hello for each part of the day, made now so it plays at once.
		hello := map[string]string{}
		for _, part := range persona.PartsOfDay {
			if phrase := c.helloFor(part); hello[phrase] == "" {
				if path := clip(phrase); path != "" {
					hello[phrase] = path
				}
			}
		}
		// Clips this voice made with an older recipe (or an old phrase) are stale.
		old, _ := filepath.Glob(filepath.Join(c.dataDir, "ack-"+voiceKey+"-*"))
		for _, p := range old {
			if !keep[p] {
				_ = os.Remove(p)
			}
		}
		more := func(phrase string) {
			making.Lock()
			defer making.Unlock()
			c.acks.mu.Lock()
			made := c.acks.hello[phrase] != ""
			c.acks.mu.Unlock()
			if made || ctx.Err() != nil {
				return
			}
			if path := clip(phrase); path != "" {
				c.acks.mu.Lock()
				c.acks.hello[phrase] = path
				c.acks.mu.Unlock()
			}
		}
		c.acks.mu.Lock()
		c.acks.ack, c.acks.hello, c.acks.more = ack, hello, more
		c.acks.mu.Unlock()
	}()
}

// ackClip picks a clip: the hello for the part of the day after a quiet
// spell, otherwise a brief "on it"; "" when none is ready.
func (c *Channel) ackClip(hello bool) string { return c.ackClipAt(hello, time.Now()) }

// ackClipAt is ackClip at the time now. A hello whose clip isn't made yet
// (the owner's name or address just changed) is a brief "on it" this time,
// and its clip is made for the next.
func (c *Channel) ackClipAt(hello bool, now time.Time) string {
	if c.acks == nil {
		return ""
	}
	c.acks.mu.Lock()
	defer c.acks.mu.Unlock()
	if hello {
		phrase := c.helloPhrase(now)
		if path := c.acks.hello[phrase]; path != "" {
			return path
		}
		if c.acks.more != nil {
			go c.acks.more(phrase)
		}
	}
	if len(c.acks.ack) == 0 {
		return ""
	}
	return c.acks.ack[rand.Intn(len(c.acks.ack))]
}

// acknowledge plays an acknowledgement, or the chime if no clip is ready.
func (c *Channel) acknowledge(hello bool) {
	if c.ackFn != nil {
		c.ackFn(hello)
		return
	}
	if clip := c.ackClip(hello); clip != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = play(ctx, clip, nil) // full level: the user has finished talking
		return
	}
	c.chime()
}

// playQuiet plays a clip at reduced volume (used for the chime, which arrives while the
// user may still be talking).
func playQuiet(ctx context.Context, path string) error {
	if _, err := exec.LookPath("afplay"); err == nil {
		return exec.CommandContext(ctx, "afplay", "-v", "0.6", path).Run()
	}
	return play(ctx, path, nil)
}

// normalize brings a short clip up to full level (short words from the synthesiser
// come out quieter than sentences) and trims leading silence so they start crisply.
func normalize(path string) {
	if _, err := exec.LookPath("sox"); err != nil {
		return
	}
	tmp := path + ".norm" + filepath.Ext(path)
	if err := exec.Command("sox", "-q", path, tmp, "silence", "1", "0.02", "0.5%", "gain", "-n", "-4").Run(); err == nil {
		_ = os.Rename(tmp, path)
	} else {
		_ = os.Remove(tmp)
	}
}

// stripAck removes a leading acknowledgement that the microphone picked up
// from the twin itself ("mm-hm", "sir", "yes", "on it"), or one of extra (the
// current bare wake, "Akshaya?").
func stripAck(text string, extra ...string) string {
	t := strings.TrimSpace(text)
	lt := strings.ToLower(t)
	acks := []string{"let me see.", "let me see", "one moment.", "one moment", "i'm here.", "i'm here", "on it.", "on it", "hello.", "hello", "right.", "sir?", "yes?", "go on.", "go on", "okay."}
	for _, a := range extra {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			acks = append(acks, a)
		}
	}
	for _, a := range acks {
		if strings.HasPrefix(lt, a) {
			return strings.TrimSpace(strings.TrimLeft(t[len(a):], " ,.?!"))
		}
	}
	return t
}
