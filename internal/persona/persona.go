// Package persona defines how a twin presents itself: its name, its
// character, how it speaks, what it sounds like. A persona is one YAML file,
// so it can be written, shared and installed like a protocol. Mirrin is the
// bundled default; a user can call their twin anything.
package persona

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Persona is a personality.
type Persona struct {
	// ID is the file name / identifier (lowercase). Name is what the twin is called.
	// Pack personas are namespaced as <pack>/<id>.
	ID   string `yaml:"id,omitempty"`
	Name string `yaml:"name"`
	// Pronunciation is how the name is spoken by the voice, if it differs.
	Pronunciation string   `yaml:"pronunciation,omitempty"`
	Tagline       string   `yaml:"tagline,omitempty"`
	Author        string   `yaml:"author,omitempty"`
	Version       string   `yaml:"version,omitempty"`
	Tags          []string `yaml:"tags,omitempty"`
	// Character is the prose description of who this is (the heart of the persona).
	Character string `yaml:"character"`
	// Address is how it addresses the user by default ("sir", "boss", "" for by name).
	Address string `yaml:"address,omitempty"`
	// Style are short rules for voice and format, appended to the system prompt.
	Style []string `yaml:"style,omitempty"`
	// Greeting is said when a session starts; Acks play the moment the user stops talking.
	// Greeting may name the owner, as Hellos do (Render).
	Greeting string   `yaml:"greeting,omitempty"`
	Acks     []string `yaml:"acks,omitempty"`
	// Hellos are its hello for each part of the day, keyed morning (5 to 12),
	// afternoon (12 to 17), evening (17 to 23) and late (23 to 5): one line
	// each, never picked at random. They may hold {name}, {address} and
	// {, address} (Render).
	Hellos map[string]string `yaml:"hellos,omitempty"`
	// Voice is the preferred speaking voice (Kokoro id, macOS or ElevenLabs name).
	Voice string `yaml:"voice,omitempty"`
	// VoicePitch (cents) and VoiceSpeed shape that voice into a character's
	// (a small penguin's is higher and a touch quicker). Kokoro only.
	VoicePitch int     `yaml:"voice_pitch,omitempty"`
	VoiceSpeed float64 `yaml:"voice_speed,omitempty"`
	// WakeWord is what the user says ("mirrin"); WakeAliases are misrecognitions to accept.
	WakeWord    string   `yaml:"wake_word,omitempty"`
	WakeAliases []string `yaml:"wake_aliases,omitempty"`
	// WakeModel is an openWakeWord model file for this name, if one has been trained.
	WakeModel string `yaml:"wake_model,omitempty"`
	// Protocols are protocol names this persona suggests enabling.
	Protocols []string `yaml:"protocols,omitempty"`

	Source string `yaml:"-"`
	Pack   string `yaml:"-"`
}

//go:embed mirrin.yaml
var mirrinYAML []byte

//go:embed pickoo.yaml
var pickooYAML []byte

//go:embed nyra.yaml
var nyraYAML []byte

// Bundled are the personas that ship with Mirrin.
func Bundled() []Persona {
	var out []Persona
	for _, b := range [][]byte{mirrinYAML, pickooYAML, nyraYAML} {
		var p Persona
		if yaml.Unmarshal(b, &p) == nil {
			p.Source = "bundled"
			out = append(out, p)
		}
	}
	return out
}

// Dir is where user personas live.
func Dir(home string) string { return filepath.Join(home, "personas") }

// Problem is a persona file that was skipped, and why.
type Problem struct {
	File    string
	Message string
}

func (p Problem) String() string { return p.File + ": " + p.Message }

// Load returns bundled personas plus those in the user's personas dir and in
// each installed pack (packs/<pack>/personas/). One bad file never costs the
// others: it is skipped, and LoadAll says which and why. The error is kept for
// existing callers and is always nil.
func Load(home, protocolsDir string) ([]Persona, error) {
	ps, _ := LoadAll(home, protocolsDir)
	return ps, nil
}

// LoadAll is Load plus the files it skipped.
//
// Precedence is yours, then bundled, then packs. A file of yours with a
// bundled id replaces that persona, which is how you retune Mirrin. Pack
// personas get the id <pack>/<id>, so a pack can add characters but never
// stand in for yours or a bundled one.
func LoadAll(home, protocolsDir string) ([]Persona, []Problem) {
	out := Bundled()
	var problems []Problem
	skip := func(path, msg string) { problems = append(problems, Problem{path, msg}) }
	add := func(dir, pack string) {
		// A pack's files are its own; a link could point anywhere on this machine.
		if st, err := os.Lstat(dir); pack != "" && err == nil && st.Mode()&fs.ModeSymlink != 0 {
			skip(dir, "symbolic links are not loaded from packs")
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				skip(dir, err.Error())
			}
			return
		}
		from := map[string]string{} // id -> file name, within this dir
		for _, e := range entries {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || (ext != ".yaml" && ext != ".yml") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if pack != "" && e.Type()&fs.ModeSymlink != 0 {
				skip(path, "symbolic links are not loaded from packs")
				continue
			}
			p, err := parse(path)
			if err != nil {
				skip(path, err.Error())
				continue
			}
			if p.ID == "" {
				p.ID = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			}
			if prev, dup := from[strings.ToLower(p.ID)]; dup {
				skip(path, fmt.Sprintf("id %q is already used by %s", p.ID, prev))
				continue
			}
			from[strings.ToLower(p.ID)] = e.Name()
			p.Source, p.Pack = path, pack
			if pack != "" {
				p.ID = pack + "/" + p.ID
				out = append(out, p)
				continue
			}
			replaced := false
			for i := range out {
				if strings.EqualFold(out[i].ID, p.ID) {
					out[i], replaced = p, true
				}
			}
			if !replaced {
				out = append(out, p)
			}
		}
	}
	add(Dir(home), "")
	if packs, err := os.ReadDir(filepath.Join(protocolsDir, "packs")); err == nil {
		for _, pk := range packs {
			// Dot directories are packs still being installed.
			if pk.IsDir() && !strings.HasPrefix(pk.Name(), ".") {
				add(filepath.Join(protocolsDir, "packs", pk.Name(), "personas"), pk.Name())
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, problems
}

// parse reads one persona file and checks the two fields every persona needs.
func parse(path string) (Persona, error) {
	var p Persona
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := yaml.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("not valid YAML: %w", err)
	}
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Character) == "" {
		return p, errors.New("name and character are required")
	}
	return p, nil
}

// Find returns a persona by id or name. An exact id wins, then the id a name
// gets as a file ("Captain Jack's" is captain-jack-s), then a name, with yours
// and the bundled ones ahead of pack personas. A bare pack persona id
// ("butler" for starter/butler) still resolves, for configs written before
// pack ids were namespaced.
func Find(ps []Persona, idOrName string) (Persona, bool) {
	k := strings.ToLower(strings.TrimSpace(idOrName))
	if k == "" {
		return Persona{}, false
	}
	slug := Slug(k)
	for _, match := range []func(p Persona) bool{
		func(p Persona) bool { return strings.ToLower(p.ID) == k },
		func(p Persona) bool { return p.Pack == "" && strings.ToLower(p.ID) == slug },
		func(p Persona) bool { return p.Pack == "" && strings.ToLower(p.Name) == k },
		func(p Persona) bool { return p.Pack != "" && strings.ToLower(p.Name) == k },
		func(p Persona) bool {
			bare := strings.ToLower(strings.TrimPrefix(p.ID, p.Pack+"/"))
			return p.Pack != "" && (bare == k || bare == slug)
		},
	} {
		for _, p := range ps {
			if match(p) {
				return p, true
			}
		}
	}
	if FormerDefault(k) || IsRetired(k) {
		return Find(ps, DefaultID)
	}
	return Persona{}, false
}

// Retired are the bundled personas that no longer ship, as earlier releases
// shipped them: Ava (id plain, the coral rover) retired on 2026-10-06. A
// config.yaml that chose one now means the default persona, and what one
// asked of the voice settings is no choice of the owner's.
var Retired = []Persona{
	{ID: "plain", Name: "Ava", Voice: "af_heart", WakeWord: "ava", WakeModel: "hey_ava.onnx"},
}

// IsRetired reports whether a persona id or name is a retired bundled one's.
func IsRetired(idOrName string) bool {
	k := strings.TrimSpace(idOrName)
	for _, p := range Retired {
		if strings.EqualFold(k, p.ID) || strings.EqualFold(k, p.Name) {
			return true
		}
	}
	return false
}

// DefaultID is the bundled default persona's id.
const DefaultID = "mirrin"

// FormerDefault reports whether a persona id or twin name is the one the
// default persona had before he was called Mirrin (MAVRK, said Maverick),
// so a config.yaml written then still means him.
func FormerDefault(idOrName string) bool {
	switch strings.ToLower(strings.TrimSpace(idOrName)) {
	case "mavrk", "maverick":
		return true
	}
	return false
}

// Default is Mirrin.
func Default() Persona {
	for _, p := range Bundled() {
		if p.ID == DefaultID {
			return p
		}
	}
	return Persona{ID: DefaultID, Name: "Mirrin", Character: "A calm, dry, loyal personal AI."}
}

// Resolve picks the persona named in config (or the default) and applies the
// user's chosen twin name over it, so "call it Ada, with Mirrin's character" works.
func Resolve(home, protocolsDir, personaID, twinName string) Persona {
	ps, _ := LoadAll(home, protocolsDir)
	p, _ := Pick(ps, personaID, twinName)
	return p
}

// Pick is Resolve over personas already loaded. ok is false when personaID
// named a persona that isn't there, so Mirrin stood in. A twin name that
// was the default's old one (MAVRK) is no name of the owner's own, and nor
// is a retired persona's name (Ava) where that persona, or none, was chosen.
func Pick(ps []Persona, personaID, twinName string) (p Persona, ok bool) {
	if FormerDefault(twinName) || (IsRetired(twinName) && (strings.TrimSpace(personaID) == "" || IsRetired(personaID))) {
		twinName = ""
	}
	p, ok = Find(ps, personaID)
	if !ok {
		if p, ok = Find(ps, DefaultID); !ok {
			p = Default()
		}
		ok = strings.TrimSpace(personaID) == ""
	}
	if twinName != "" && !strings.EqualFold(twinName, p.Name) {
		p.Name = twinName
		p.Pronunciation = ""
		p.WakeWord = strings.ToLower(twinName)
		p.WakeAliases = nil
		p.WakeModel = ""
	}
	if p.WakeWord == "" {
		p.WakeWord = strings.ToLower(p.Name)
	}
	return p, ok
}

// PartsOfDay are the keys of Hellos, in the day's order.
var PartsOfDay = []string{"morning", "afternoon", "evening", "late"}

// PartOfDay is the part of the day t falls in: morning from 5 to 12,
// afternoon to 17, evening to 23, and late until 5.
func PartOfDay(t time.Time) string {
	switch h := t.Hour(); {
	case h >= 5 && h < 12:
		return "morning"
	case h >= 12 && h < 17:
		return "afternoon"
	case h >= 17 && h < 23:
		return "evening"
	}
	return "late"
}

// Render fills in a greeting or hello for the owner: {name} is their first
// name and {address} their form of address. "{, address}" is ", sir", or
// nothing when there is no address; a missing name takes its comma with it,
// so "Morning, {name}." is "Morning.". What is filled in is never read
// again for placeholders.
func Render(s, name, address string) string {
	pairs := []string{}
	for _, f := range [][2]string{{"name", strings.TrimSpace(name)}, {"address", strings.TrimSpace(address)}} {
		key, val := "{"+f[0]+"}", f[1]
		if val == "" {
			pairs = append(pairs, "{, "+f[0]+"}", "", ", "+key, "", " "+key, "", key, "")
			continue
		}
		pairs = append(pairs, "{, "+f[0]+"}", ", "+val, key, val)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// Spoken is the name as the voice should say it.
func (p Persona) Spoken() string {
	if p.Pronunciation != "" {
		return p.Pronunciation
	}
	return p.Name
}

// Write saves a new persona to the user's personas dir, in a file named after
// its id (or after its name, as Slug makes it). It never replaces a file.
func Write(home string, p Persona) (string, error) {
	if p.ID == "" {
		p.ID = Slug(p.Name)
	}
	if p.ID == "" {
		return "", errors.New("give the persona a name with at least one letter or number")
	}
	if err := os.MkdirAll(Dir(home), 0o700); err != nil {
		return "", err
	}
	b, err := yaml.Marshal(p)
	if err != nil {
		return "", err
	}
	path := filepath.Join(Dir(home), p.ID+".yaml")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("%s already exists; edit it, or pick another name", path)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

// Scaffold writes a template persona to edit.
func Scaffold(home, name string) (string, error) {
	for _, b := range Bundled() {
		if Slug(name) == b.ID || strings.EqualFold(strings.TrimSpace(name), b.Name) {
			return "", fmt.Errorf("%s is a built-in persona; pick another name for yours", b.Name)
		}
	}
	return Write(home, Persona{
		Name:      name,
		Tagline:   "One line on who this is.",
		Author:    "your name or handle",
		Version:   "0.1.0",
		Character: "Describe the personality in a paragraph: temperament, humour, how it treats the user, what it cares about. Write it as a character brief, not a feature list.",
		Address:   "",
		Style:     []string{"Short sentences.", "Warm, never gushing."},
		Greeting:  "Hello. I'm here.",
		Acks:      []string{"Yes?", "Go on."},
	})
}

// Slug is the id a name gets as a file: lowercase letters and digits in any
// script, with each run of anything else turned into one hyphen.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}
