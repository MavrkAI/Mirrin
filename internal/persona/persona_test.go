package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundledAndResolve(t *testing.T) {
	ps := Bundled()
	if len(ps) != 3 {
		t.Fatalf("expected 3 bundled personas, got %d", len(ps))
	}
	// Nyra: a warm, composed woman, said "Nyra"
	if p, ok := Find(ps, "nyra"); !ok || p.Name != "Nyra" || p.Spoken() != "Nyra" || p.Voice != "af_nova" || p.WakeWord != "nyra" || p.Character == "" {
		t.Fatalf("nyra: %+v", p)
	}
	// the penguin: a cute character voice, shaped from a Kokoro one
	if p, ok := Find(ps, "pickoo"); !ok || p.Voice != "am_puck" || p.VoicePitch != 520 || p.VoiceSpeed != 1.06 || p.WakeWord != "pickoo" {
		t.Fatalf("pickoo: %+v", p)
	}
	home := t.TempDir()
	p := Resolve(home, filepath.Join(home, "protocols"), "mirrin", "")
	if p.Name != "Mirrin" || p.Spoken() != "Mirrin" || p.WakeWord != "mirrin" || p.WakeModel != "hey_mirrin.onnx" || p.Address != "sir" || p.Voice != "bm_george" {
		t.Fatalf("mirrin: %+v", p)
	}
	// user renames the twin: character stays, name/wake word follow
	p = Resolve(home, filepath.Join(home, "protocols"), "mirrin", "Jeeves")
	if p.Name != "Jeeves" || p.WakeWord != "jeeves" || p.Spoken() != "Jeeves" || p.WakeModel != "" || p.Address != "sir" {
		t.Fatalf("renamed: %+v", p)
	}
	// a user persona file overrides
	if _, err := Scaffold(home, "Nova"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(Dir(home), "nova.yaml")); err != nil {
		t.Fatal("scaffold missing")
	}
	all, err := Load(home, filepath.Join(home, "protocols"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Find(all, "nova"); !ok {
		t.Fatal("user persona not loaded")
	}
	if _, ok := Find(all, "Nyra"); !ok {
		t.Fatal("find by name failed")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// One broken persona file, anywhere, used to make Load fail, and Resolve then
// quietly swapped the chosen persona for Mirrin.
func TestBadPersonaFileIsSkippedNotFatal(t *testing.T) {
	home := t.TempDir()
	protos := filepath.Join(home, "protocols")
	writeFile(t, filepath.Join(Dir(home), "broken.yaml"), "name: Broken\n")
	writeFile(t, filepath.Join(Dir(home), "garbled.yaml"), "name: [unclosed\n")
	writeFile(t, filepath.Join(Dir(home), "nova.yaml"), "name: Nova\ncharacter: Bright.\n")
	writeFile(t, filepath.Join(protos, "packs", "extra", "personas", "bad.yaml"), "character: no name\n")

	p := Resolve(home, protos, "nyra", "Nyra")
	if p.ID != "nyra" || !strings.Contains(p.Character, "brilliant, composed woman") {
		t.Fatalf("chosen persona lost to a bad file: %+v", p)
	}
	ps, err := Load(home, protos)
	if err != nil {
		t.Fatalf("Load should skip bad files, got %v", err)
	}
	if _, ok := Find(ps, "nova"); !ok {
		t.Fatal("good persona next to a bad one was dropped")
	}
	_, skipped := LoadAll(home, protos)
	var files []string
	for _, s := range skipped {
		files = append(files, filepath.Base(s.File)+": "+s.Message)
	}
	got := strings.Join(files, "\n")
	for _, want := range []string{"broken.yaml: name and character are required", "garbled.yaml: not valid YAML", "bad.yaml: name and character are required"} {
		if !strings.Contains(got, want) {
			t.Errorf("skipped files should include %q, got:\n%s", want, got)
		}
	}
}

// A pack persona used to replace the user's own persona, or a bundled one,
// when it reused the id.
func TestPackPersonasNeverReplaceYoursOrBundled(t *testing.T) {
	home := t.TempDir()
	protos := filepath.Join(home, "protocols")
	writeFile(t, filepath.Join(Dir(home), "mine.yaml"), "name: Mine\ncharacter: MY OWN CHARACTER\n")
	writeFile(t, filepath.Join(Dir(home), "mirrin.yaml"), "name: Mirrin\ncharacter: MY RETUNED MIRRIN\n")
	pack := filepath.Join(protos, "packs", "somepack", "personas")
	writeFile(t, filepath.Join(pack, "x.yaml"), "id: mirrin\nname: Mirrin\ncharacter: PACK CHARACTER FOR MIRRIN\n")
	writeFile(t, filepath.Join(pack, "y.yaml"), "id: mine\nname: Mine\ncharacter: PACK CHARACTER FOR MINE\n")
	writeFile(t, filepath.Join(pack, "nyra.yaml"), "name: Nyra\ncharacter: PACK CHARACTER FOR NYRA\n")
	writeFile(t, filepath.Join(pack, "butler.yaml"), "name: Hudson\ncharacter: PACK BUTLER\n")

	ps, _ := LoadAll(home, protos)
	cases := []struct {
		lookup, wantID, wantCharacter string
	}{
		{"mirrin", "mirrin", "MY RETUNED MIRRIN"},
		{"Mirrin", "mirrin", "MY RETUNED MIRRIN"},
		{"mavrk", "mirrin", "MY RETUNED MIRRIN"}, // his id before he was Mirrin
		{"mine", "mine", "MY OWN CHARACTER"},
		{"nyra", "nyra", "brilliant, composed woman"},
		{"Nyra", "nyra", "brilliant, composed woman"},
		{"somepack/nyra", "somepack/nyra", "PACK CHARACTER FOR NYRA"},
		{"somepack/mirrin", "somepack/mirrin", "PACK CHARACTER FOR MIRRIN"},
		{"somepack/mine", "somepack/mine", "PACK CHARACTER FOR MINE"},
		// configs written before pack ids were namespaced still resolve
		{"butler", "somepack/butler", "PACK BUTLER"},
		{"Hudson", "somepack/butler", "PACK BUTLER"},
	}
	for _, c := range cases {
		p, ok := Find(ps, c.lookup)
		if !ok || p.ID != c.wantID || !strings.Contains(p.Character, c.wantCharacter) {
			t.Errorf("Find(%q) = %q %q, want %q with %q", c.lookup, p.ID, p.Character, c.wantID, c.wantCharacter)
		}
	}
	if p := Resolve(home, protos, "mine", ""); p.Character != "MY OWN CHARACTER" {
		t.Fatalf("Resolve(mine) = %q", p.Character)
	}
}

func TestPackPersonaSymlinkIsSkipped(t *testing.T) {
	home := t.TempDir()
	protos := filepath.Join(home, "protocols")
	outside := filepath.Join(home, "elsewhere.yaml")
	writeFile(t, outside, "name: Linked\ncharacter: from outside the pack\n")
	pack := filepath.Join(protos, "packs", "p", "personas")
	if err := os.MkdirAll(pack, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pack, "linked.yaml")); err != nil {
		t.Skip("symlinks not available:", err)
	}
	ps, skipped := LoadAll(home, protos)
	if _, ok := Find(ps, "p/linked"); ok || len(skipped) != 1 {
		t.Fatalf("a symlinked pack persona should be skipped: %v", skipped)
	}

	// a pack's whole personas/ folder can be a link too
	elsewhere := filepath.Join(home, "elsewhere")
	writeFile(t, filepath.Join(elsewhere, "far.yaml"), "name: Far\ncharacter: from outside the pack\n")
	if err := os.MkdirAll(filepath.Join(protos, "packs", "q"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(protos, "packs", "q", "personas")); err != nil {
		t.Fatal(err)
	}
	ps, skipped = LoadAll(home, protos)
	if _, ok := Find(ps, "q/far"); ok || len(skipped) != 2 || !strings.Contains(skipped[1].Message, "symbolic links") {
		t.Fatalf("a symlinked personas folder should be skipped: %v", skipped)
	}
}

// `persona new` used to overwrite a persona of the same name, and printed an
// id ("captain-jack's") that `persona use` could not find.
func TestScaffoldNeverOverwritesAndItsIDResolves(t *testing.T) {
	home := t.TempDir()
	path, err := Scaffold(home, "Captain Jack's")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "captain-jack-s.yaml" {
		t.Fatalf("path %s", path)
	}
	mine := "name: Captain Jack's\ncharacter: HAND WRITTEN\n"
	writeFile(t, path, mine)
	if _, err := Scaffold(home, "Captain Jack's"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second scaffold should refuse, got %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != mine {
		t.Fatalf("hand-written persona was overwritten:\n%s", b)
	}
	ps, _ := LoadAll(home, filepath.Join(home, "protocols"))
	for _, lookup := range []string{"captain-jack-s", "captain-jack's", "Captain Jack's"} {
		if p, ok := Find(ps, lookup); !ok || p.Character != "HAND WRITTEN" {
			t.Errorf("Find(%q) failed: %+v", lookup, p)
		}
	}

	for _, bad := range []string{"Mirrin", "mirrin", "Nyra", "!!!"} {
		if _, err := Scaffold(home, bad); err == nil {
			t.Errorf("Scaffold(%q) should refuse", bad)
		}
	}
	// names in any script get their own files
	a, errA := Scaffold(home, "朝の執事")
	b, errB := Scaffold(home, "夜の執事")
	if errA != nil || errB != nil || a == b || strings.HasPrefix(filepath.Base(a), ".") {
		t.Fatalf("non-Latin names: %q %v, %q %v", a, errA, b, errB)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Nova":            "nova",
		"Captain Jack's":  "captain-jack-s",
		"  Mr. -- Smith ": "mr-smith",
		"Café Bot":        "café-bot",
		"朝の執事":            "朝の執事",
		"!!!":             "",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickReportsUnknownPersona(t *testing.T) {
	ps := Bundled()
	if p, ok := Pick(ps, "nobody", ""); ok || p.ID != "mirrin" {
		t.Fatalf("unknown id: ok=%v id=%s", ok, p.ID)
	}
	if _, ok := Pick(ps, "", ""); !ok {
		t.Fatal("no persona configured is the default, not a miss")
	}
	if p, ok := Pick(ps, "nyra", ""); !ok || p.ID != "nyra" {
		t.Fatalf("nyra: ok=%v %+v", ok, p)
	}
}

// The default persona was called MAVRK (said Maverick) before he was
// Mirrin. A config.yaml from then says persona: mavrk and name: MAVRK; it
// still means him, by his new name, answering to "Hey Mirrin".
func TestFormerDefaultIsMirrin(t *testing.T) {
	ps := Bundled()
	for _, id := range []string{"mavrk", "MAVRK", "maverick", " Mavrk "} {
		p, ok := Find(ps, id)
		if !ok || p.ID != "mirrin" {
			t.Errorf("Find(%q) = %q, %v; want mirrin", id, p.ID, ok)
		}
	}
	for _, name := range []string{"MAVRK", "mavrk", "Maverick", ""} {
		p, ok := Pick(ps, "mavrk", name)
		if !ok || p.ID != "mirrin" || p.Name != "Mirrin" || p.WakeWord != "mirrin" || p.WakeModel != "hey_mirrin.onnx" || len(p.WakeAliases) == 0 {
			t.Errorf("Pick(mavrk, %q) = ok %v, %+v", name, ok, p)
		}
	}
	// the old name is no name of the owner's for another persona either
	if p, _ := Pick(ps, "nyra", "MAVRK"); p.Name != "Nyra" {
		t.Errorf("nyra named MAVRK = %q, want Nyra", p.Name)
	}
	// a name of the owner's own still wins
	if p, _ := Pick(ps, "mavrk", "Jeeves"); p.Name != "Jeeves" || p.WakeWord != "jeeves" {
		t.Errorf("renamed: %+v", p)
	}
	// an unknown id is still a miss, standing in Mirrin
	if p, ok := Pick(ps, "nobody", "MAVRK"); ok || p.Name != "Mirrin" {
		t.Errorf("unknown: ok %v, %+v", ok, p)
	}
}

// Ava (id plain) retired. A config.yaml that chose her says persona: plain
// and name: Ava; it now means Mirrin, by his name, answering to "Hey
// Mirrin", never a Mirrin called Ava who answers to "ava".
func TestRetiredAvaIsMirrin(t *testing.T) {
	ps := Bundled()
	for _, p := range ps {
		if IsRetired(p.ID) || IsRetired(p.Name) {
			t.Errorf("%s ships again but is listed as retired", p.ID)
		}
	}
	for _, id := range []string{"plain", "Plain", " ava ", "Ava"} {
		if p, ok := Find(ps, id); !ok || p.ID != DefaultID {
			t.Errorf("Find(%q) = %q, %v; want mirrin", id, p.ID, ok)
		}
	}
	for _, c := range []struct{ id, name string }{{"plain", "Ava"}, {"plain", ""}, {"", "Ava"}, {"plain", "ava"}} {
		p, ok := Pick(ps, c.id, c.name)
		if !ok || p.ID != DefaultID || p.Name != "Mirrin" || p.WakeWord != "mirrin" || p.WakeModel != "hey_mirrin.onnx" {
			t.Errorf("Pick(%q, %q) = ok %v, %+v", c.id, c.name, ok, p)
		}
	}
	// a name of the owner's own still wins, and so does Ava as a name
	// chosen for another persona
	if p, _ := Pick(ps, "plain", "Juniper"); p.ID != DefaultID || p.Name != "Juniper" {
		t.Errorf("plain named Juniper: %+v", p)
	}
	if p, _ := Pick(ps, "nyra", "Ava"); p.ID != "nyra" || p.Name != "Ava" || p.WakeWord != "ava" {
		t.Errorf("Nyra named Ava: %+v", p)
	}
}
