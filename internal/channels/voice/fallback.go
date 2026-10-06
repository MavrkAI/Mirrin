package voice

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// When the chosen speech engine fails (ElevenLabs out of quota, the network
// down, Kokoro's helper crashing), the sentence is spoken by a backup
// instead of skipped: Kokoro standing in for ElevenLabs when it is
// installed, else the system voice. The owner is told once, and the backup
// says so before its first sentence, so a different voice is never a
// mystery. A failed engine rests for a few minutes, so a dead network
// doesn't cost every sentence a timeout.

// restFor is how long a failed engine is skipped.
const restFor = 5 * time.Minute

// clip is one synthesised sentence, or (err set) the text to say another way.
type clip struct {
	text  string
	path  string
	err   error
	synth time.Duration
}

// backupNote is what the backup says before its first sentence.
const backupNote = "Quick note: my usual voice isn't working, so this is my backup."

// synthesize voices one sentence for the PCM player; seq numbers its files.
func (c *Channel) synthesize(ctx context.Context, text string, seq *int) []clip {
	next := func(ext string) string {
		*seq++
		name := fmt.Sprintf("voice-out-%d", *seq%4)
		if ext != "" {
			name += "." + ext
		}
		return filepath.Join(c.dataDir, name)
	}
	t0 := time.Now()
	name, down, ext, fn := c.chosenEngine(ctx, text)
	var why error
	if fn != nil && !c.resting(down) {
		path := next(ext)
		err := fn(path)
		if err == nil {
			c.recovered(troubleVoice)
			return []clip{{text: text, path: path, synth: time.Since(t0)}}
		}
		if ctx.Err() != nil {
			return []clip{{text: text, err: ctx.Err()}}
		}
		c.rest(down)
		why = err
	}
	var out []clip
	if !c.toldAbout(troubleVoice) {
		msg := "My " + name + " voice isn't working"
		if why != nil {
			msg += " (" + brief(why) + ")"
		}
		c.problem(troubleVoice, msg+", so I'm speaking with my backup voice until it's back.")
		out = append(out, c.backupClip(ctx, backupNote, next))
	}
	cl := c.backupClip(ctx, text, next)
	cl.synth = time.Since(t0)
	return append(out, cl)
}

// chosenEngine is the configured file-writing engine: its name, its rest
// marker, the file type it writes and how it writes one.
func (c *Channel) chosenEngine(ctx context.Context, text string) (string, *time.Time, string, func(string) error) {
	switch {
	case c.eleven != nil:
		return "ElevenLabs", &c.elevenDown, "mp3", func(p string) error { return c.eleven.synthesizeTo(ctx, text, p) }
	case c.kokoro != nil:
		return "Kokoro", &c.kokoroDown, "wav", func(p string) error { return c.kokoro.synthesize(ctx, text, p) }
	}
	return "usual", nil, "", nil
}

// backupClip voices a line with the backup.
func (c *Channel) backupClip(ctx context.Context, line string, next func(string) string) clip {
	if k := c.backup(); k != nil {
		path := next("wav")
		if err := k.synthesize(ctx, line, path); err == nil {
			return clip{text: line, path: path}
		}
		c.rest(&c.kokoroDown)
	}
	path, err := c.systemSynthTo(ctx, line, next(""))
	return clip{text: line, path: path, err: err}
}

func (c *Channel) resting(until *time.Time) bool {
	if until == nil {
		return false
	}
	c.backupMu.Lock()
	defer c.backupMu.Unlock()
	return time.Now().Before(*until)
}

func (c *Channel) rest(until *time.Time) {
	c.backupMu.Lock()
	*until = time.Now().Add(restFor)
	c.backupMu.Unlock()
}

// backup is Kokoro standing in for ElevenLabs, when it is installed. When
// Kokoro is the chosen voice, the system voice is its backup.
func (c *Channel) backup() *kokoro {
	if c.eleven == nil {
		return nil
	}
	if c.resting(&c.kokoroDown) || !kokoroReady(c.cfg.KokoroDir) {
		return nil
	}
	lang := speechLanguage(c.cfg.Language)
	voice := kokoroVoiceFor(lang, "") // its own default voice: the configured one is ElevenLabs'
	if lang != "" && voice == "" {
		return nil // Kokoro can't speak this language; the system voice will
	}
	c.backupMu.Lock()
	defer c.backupMu.Unlock()
	if c.backupKokoro == nil {
		c.backupKokoro = newKokoro(c.cfg.KokoroDir, voice, c.cfg.Speed)
		c.backupKokoro.lang = lang
	}
	return c.backupKokoro
}

func (c *Channel) stopBackup() {
	c.backupMu.Lock()
	defer c.backupMu.Unlock()
	if c.backupKokoro != nil {
		c.backupKokoro.stop()
		c.backupKokoro = nil
	}
}

// systemSynthTo writes the system voice saying text to base plus an
// extension, for the PCM player.
var systemSynthTo = func(c *Channel, ctx context.Context, text, base string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		path := base + ".aiff"
		args := []string{"-o", path}
		if c.macVoice != "" {
			args = append(args, "-v", c.macVoice)
		}
		return path, exec.CommandContext(ctx, "say", append(args, text)...).Run()
	case "windows":
		path := base + ".wav"
		ps := fmt.Sprintf(`Add-Type -AssemblyName System.Speech; $s = New-Object System.Speech.Synthesis.SpeechSynthesizer; $s.SetOutputToWaveFile('%s'); $s.Speak([Console]::In.ReadToEnd()); $s.Dispose()`, strings.ReplaceAll(path, "'", "''"))
		cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", ps)
		cmd.Stdin = strings.NewReader(text)
		return path, cmd.Run()
	default:
		path := base + ".wav"
		if _, err := exec.LookPath("espeak"); err != nil {
			return "", errors.New("no offline voice to fall back on (install espeak)")
		}
		return path, exec.CommandContext(ctx, "espeak", "-w", path, text).Run()
	}
}

func (c *Channel) systemSynthTo(ctx context.Context, text, base string) (string, error) {
	return systemSynthTo(c, ctx, text, base)
}
